package imapserver_test

// Independent review probes for candidate 4a61a32 (T33, issue #54).
// Every assertion states an RFC or documented-contract requirement, never the
// candidate's current behaviour. Only exported API is used.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapclient"
	"github.com/kiliant/go-imap/imapserver"
	"github.com/kiliant/go-imap/imapserver/maildir"
	"github.com/kiliant/go-imap/imapserver/memory"
)

// ---- wire helpers -------------------------------------------------------

type r4Wire struct {
	t       *testing.T
	c       net.Conn
	r       *bufio.Reader
	partial string
}

func r4Memory() *memory.Backend {
	return memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}})
}

func r4Maildir(t *testing.T) *maildir.Backend {
	t.Helper()
	b, err := maildir.Open(t.Context(), t.TempDir(), &maildir.Options{Accounts: []maildir.AccountOptions{{Username: "alice", Password: "secret"}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close(context.Background(), nil) })
	return b
}

func r4Backend(t *testing.T, kind string) imapserver.Backend {
	if kind == "memory" {
		return r4Memory()
	}
	return r4Maildir(t)
}

func r4Open(t *testing.T, b imapserver.Backend, opts *imapserver.Options) *r4Wire {
	t.Helper()
	if opts == nil {
		opts = &imapserver.Options{AllowInsecureAuth: true}
	}
	c, peer := net.Pipe()
	s := imapserver.New(b, opts)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(func() { cancel(); _ = c.Close(); _ = s.Close(context.Background(), nil) })
	go func() { _ = s.ServeConn(ctx, peer, nil) }()
	w := &r4Wire{t: t, c: c, r: bufio.NewReader(c)}
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := w.line(); err != nil {
		t.Fatal(err)
	}
	w.cmd("L", "LOGIN alice secret")
	return w
}

// line reads one CRLF line, folding a literal into the line it belongs to.
func (w *r4Wire) line() (string, error) {
	s, err := w.r.ReadString('\n')
	s = w.partial + s
	if err != nil {
		w.partial = s
		return "", err
	}
	w.partial = ""
	s = strings.TrimRight(s, "\r\n")
	if m := regexp.MustCompile(`\{(\d+)\}$`).FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		buf := make([]byte, n)
		if _, err := io.ReadFull(w.r, buf); err != nil {
			return "", err
		}
		rest, err := w.line()
		if err != nil {
			return "", err
		}
		s = s + "<" + strconv.Itoa(n) + " octets>" + rest
	}
	return s, nil
}

func (w *r4Wire) write(raw string) {
	go func() { _, _ = io.WriteString(w.c, raw) }()
}

// until reads lines until the tagged response for tag.
func (w *r4Wire) until(tag string) []string {
	w.t.Helper()
	_ = w.c.SetReadDeadline(time.Now().Add(10 * time.Second))
	var lines []string
	for {
		l, err := w.line()
		if err != nil {
			w.t.Fatalf("reading for %s: %v (so far %q)", tag, err, lines)
		}
		lines = append(lines, l)
		if strings.HasPrefix(l, tag+" ") {
			return lines
		}
	}
}

func (w *r4Wire) cmd(tag, command string) []string {
	w.t.Helper()
	w.write(tag + " " + command + "\r\n")
	return w.until(tag)
}

// quiet collects whatever the server sends within d without a command.
func (w *r4Wire) quiet(d time.Duration) []string {
	_ = w.c.SetReadDeadline(time.Now().Add(d))
	var lines []string
	for {
		l, err := w.line()
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				return lines
			}
			lines = append(lines, "<read error: "+err.Error()+">")
			return lines
		}
		lines = append(lines, l)
	}
}

func r4Append(w *r4Wire, mailbox, flags, subject string) []string {
	raw := "Subject: " + subject + "\r\n\r\nbody " + subject + "\r\n"
	f := ""
	if flags != "" {
		f = " (" + flags + ")"
	}
	return w.cmd("A", fmt.Sprintf("APPEND %s%s {%d+}\r\n%s", mailbox, f, len(raw), raw))
}

func r4Tagged(lines []string) string { return lines[len(lines)-1] }

func r4Has(lines []string, re string) bool {
	x := regexp.MustCompile(re)
	for _, l := range lines {
		if x.MatchString(l) {
			return true
		}
	}
	return false
}

func r4Index(lines []string, re string) int {
	x := regexp.MustCompile(re)
	for i, l := range lines {
		if x.MatchString(l) {
			return i
		}
	}
	return -1
}

func r4HighestModSeq(lines []string) uint64 {
	x := regexp.MustCompile(`HIGHESTMODSEQ (\d+)`)
	for _, l := range lines {
		if m := x.FindStringSubmatch(l); m != nil {
			v, _ := strconv.ParseUint(m[1], 10, 64)
			return v
		}
	}
	return 0
}

func r4Log(t *testing.T, label string, lines []string) {
	t.Helper()
	t.Logf("%s: %s", label, strings.Join(lines, " | "))
}

// ---- P1: QRESYNC EXPUNGE / UID EXPUNGE / MOVE HIGHESTMODSEQ ---------------

// RFC 7162 sections 3.2.7 and 3.2.9: "If at least one message got expunged
// and QRESYNC was enabled, the server MUST send the updated per-mailbox
// modification sequence using the HIGHESTMODSEQ response code ... in the
// tagged OK response." RFC 6851 section 4.4: after a MOVE of at least one
// message "the server MUST send the updated per-mailbox modification sequence
// using the HIGHESTMODSEQ response code ... in the tagged or untagged OK".
func TestReview4QResyncRemovalHighestModSeq(t *testing.T) {
	for _, kind := range []string{"memory", "maildir"} {
		t.Run(kind, func(t *testing.T) {
			w := r4Open(t, r4Backend(t, kind), nil)
			for i := 0; i < 3; i++ {
				r4Append(w, "INBOX", "", fmt.Sprint("m", i))
			}
			w.cmd("C", "CREATE other")
			w.cmd("E", "ENABLE QRESYNC")
			sel := w.cmd("S", "SELECT INBOX")
			before := r4HighestModSeq(sel)
			w.cmd("D", `UID STORE 1 +FLAGS.SILENT (\Deleted)`)
			lines := w.cmd("X", "EXPUNGE")
			r4Log(t, "EXPUNGE", lines)
			if got := r4HighestModSeq([]string{r4Tagged(lines)}); !strings.HasPrefix(r4Tagged(lines), "X OK [HIGHESTMODSEQ ") || got <= before {
				t.Errorf("EXPUNGE tagged OK lacks updated HIGHESTMODSEQ (before %d): %q", before, r4Tagged(lines))
			}
			w.cmd("D", `UID STORE 2 +FLAGS.SILENT (\Deleted)`)
			lines = w.cmd("Y", "UID EXPUNGE 2")
			r4Log(t, "UID EXPUNGE", lines)
			if !strings.HasPrefix(r4Tagged(lines), "Y OK [HIGHESTMODSEQ ") {
				t.Errorf("UID EXPUNGE tagged OK lacks HIGHESTMODSEQ: %q", r4Tagged(lines))
			}
			lines = w.cmd("M", "UID MOVE 3 other")
			r4Log(t, "UID MOVE", lines)
			if r4HighestModSeq(lines) <= before {
				t.Errorf("MOVE sent no updated HIGHESTMODSEQ in a tagged or untagged OK")
			}
		})
	}
}

// ---- P2: a withheld addition must never appear in a non-EARLIER VANISHED ---

// A contract-valid adapter: it forwards memory unchanged except that each
// UpdateExpunge published to this session's selection becomes the public
// UpdateVanished vocabulary (non-EARLIER), which the Update contract offers
// for exactly this ("removes one or more UIDs using QRESYNC VANISHED
// semantics"). Only used where the session has enabled QRESYNC.
type r4VanishBackend struct{ *memory.Backend }
type r4VanishSession struct {
	imapserver.Session
	notify imapserver.NotifySession
}

func (b *r4VanishBackend) Authenticate(ctx context.Context, info *imapserver.ConnInfo, cred *imapserver.Credentials, opts *imapserver.AuthenticateOptions) (imapserver.Session, error) {
	s, err := b.Backend.Authenticate(ctx, info, cred, opts)
	if err != nil {
		return nil, err
	}
	return &r4VanishSession{Session: s, notify: s.(imapserver.NotifySession)}, nil
}
func (s *r4VanishSession) Notify(ctx context.Context, u *imapserver.SessionUpdater, c *imapserver.NotifyConfig, o *imapserver.NotifyOptions) error {
	return s.notify.Notify(ctx, u, c, o)
}
func (s *r4VanishSession) Select(ctx context.Context, name string, u *imapserver.Updater, opts *imapserver.SelectOptions) (*imapserver.SelectResult, error) {
	wrapped := &imapserver.Updater{PushFunc: func(b *imapserver.UpdateBatch) error {
		out := &imapserver.UpdateBatch{Before: b.Before, After: b.After, Origin: b.Origin}
		for _, ch := range b.Changes {
			if e, ok := ch.(*imapserver.UpdateExpunge); ok {
				out.Changes = append(out.Changes, &imapserver.UpdateVanished{UIDs: imap.UIDSetNum(e.UID)})
				continue
			}
			out.Changes = append(out.Changes, ch)
		}
		return u.Push(out)
	}}
	return s.Session.Select(ctx, name, wrapped, opts)
}

// RFC 7162 section 3.2.10.2: "A VANISHED response without the EARLIER tag MUST
// refer only to messages that are visible to the client in the current
// session ... servers MUST NOT send UIDs for ... messages that were not
// announced to the client via EXISTS."
func TestReview4WithheldAdditionNeverVanished(t *testing.T) {
	for _, adapter := range []string{"memory", "vanished-adapter"} {
		for _, scenario := range []string{"foreign-expunge-then-notify-set", "own-expunge"} {
			t.Run(adapter+"/"+scenario, func(t *testing.T) {
				store := r4Memory()
				var observerBackend imapserver.Backend = store
				if adapter == "vanished-adapter" {
					observerBackend = &r4VanishBackend{store}
				}
				obs := r4Open(t, observerBackend, nil)
				other := r4Open(t, store, nil)
				r4Append(other, "INBOX", "", "seed")
				obs.cmd("E", "ENABLE QRESYNC")
				obs.cmd("S", "SELECT INBOX")
				r4Log(t, "NOTIFY", obs.cmd("N", "NOTIFY SET (selected NONE)"))
				// UID 2 arrives while the registration withholds additions.
				r4Append(other, "INBOX", `\Deleted`, "withheld")
				var lines []string
				if scenario == "own-expunge" {
					lines = obs.cmd("X", "EXPUNGE")
				} else {
					other.cmd("S", "SELECT INBOX")
					other.cmd("X", "EXPUNGE")
					lines = obs.cmd("N2", "NOTIFY SET (selected (MessageNew MessageExpunge))")
				}
				lines = append(lines, obs.cmd("Q", "NOOP")...)
				r4Log(t, "observer", lines)
				r4Log(t, "server view afterwards", obs.cmd("V", "UID FETCH 1:* (UID)"))
				if r4Has(lines, `^\* VANISHED (2|.*[,:]2)\b`) {
					t.Errorf("non-EARLIER VANISHED names UID 2, which was never announced by EXISTS")
				}
				if r4Has(lines, `\* 2 EXISTS`) && r4Has(lines, `^\* VANISHED`) {
					t.Logf("note: EXISTS 2 was sent for the removed message")
				}
			})
		}
	}
}

// ---- P3: STORE must report MODSEQ once CONDSTORE is enabled ---------------

// An adapter whose Store writes "the resulting flags" (SelectedMailbox.Store)
// and UID, as the FetchWriter contract requires, but no MODSEQ, which no
// exported contract asks Store to write.
type r4FlagsOnlyBackend struct{ *memory.Backend }
type r4FlagsOnlySession struct{ imapserver.Session }
type r4FlagsOnlySelected struct {
	imapserver.SelectedMailbox
	cond   imapserver.CondStoreMailbox
	resync imapserver.QResyncMailbox
}

func (b *r4FlagsOnlyBackend) Authenticate(ctx context.Context, info *imapserver.ConnInfo, cred *imapserver.Credentials, opts *imapserver.AuthenticateOptions) (imapserver.Session, error) {
	s, err := b.Backend.Authenticate(ctx, info, cred, opts)
	if err != nil {
		return nil, err
	}
	return &r4FlagsOnlySession{s}, nil
}
func (s *r4FlagsOnlySession) Select(ctx context.Context, name string, u *imapserver.Updater, opts *imapserver.SelectOptions) (*imapserver.SelectResult, error) {
	r, err := s.Session.Select(ctx, name, u, opts)
	if err == nil {
		r.Mailbox = &r4FlagsOnlySelected{SelectedMailbox: r.Mailbox, cond: r.Mailbox.(imapserver.CondStoreMailbox), resync: r.Mailbox.(imapserver.QResyncMailbox)}
	}
	return r, err
}
func r4StripModSeq(w *imapserver.FetchWriter) *imapserver.FetchWriter {
	return &imapserver.FetchWriter{WriteFunc: func(ctx context.Context, d *imap.FetchMessageData) error {
		copied := *d
		copied.Items = make(map[imap.FetchDataKey][]imap.FetchData, len(d.Items))
		for k, v := range d.Items {
			if !strings.EqualFold(string(k), "MODSEQ") {
				if strings.EqualFold(string(k), "FLAGS") {
					k = "flags" // A valid backend need not canonicalize map keys.
				}
				copied.Items[k] = v
			}
		}
		return w.WriteMessage(ctx, &copied)
	}}
}
func (s *r4FlagsOnlySelected) Store(ctx context.Context, w *imapserver.FetchWriter, uids imap.UIDSet, f *imapserver.StoreFlags, o *imapserver.StoreOptions) error {
	return s.SelectedMailbox.Store(ctx, r4StripModSeq(w), uids, f, o)
}
func (s *r4FlagsOnlySelected) StoreCondStore(ctx context.Context, w *imapserver.FetchWriter, uids imap.UIDSet, f *imapserver.StoreFlags, o *imapserver.StoreOptions) (*imapserver.CondStoreResult, error) {
	return s.cond.StoreCondStore(ctx, r4StripModSeq(w), uids, f, o)
}
func (s *r4FlagsOnlySelected) Resync(ctx context.Context, q *imapserver.QResyncSelect, o *imapserver.QResyncOptions) (*imapserver.QResyncResult, error) {
	return s.resync.Resync(ctx, q, o)
}

// RFC 7162 section 3.1: once CONDSTORE is enabled "The server MUST then include
// mod-sequence data in all subsequent untagged FETCH responses ... whether they
// were caused by a regular STORE, a STORE with an UNCHANGEDSINCE modifier, or
// an external agent."
func TestReview4StoreModSeqFromFlagsOnlyBackend(t *testing.T) {
	w := r4Open(t, &r4FlagsOnlyBackend{r4Memory()}, nil)
	r4Append(w, "INBOX", "", "one")
	r4Append(w, "INBOX", "", "two")
	w.cmd("E", "ENABLE CONDSTORE")
	w.cmd("S", "SELECT INBOX")
	for _, tc := range []struct{ name, cmd string }{
		{"unconditional", `STORE 1 +FLAGS (\Flagged)`},
		{"conditional", `STORE 2 (UNCHANGEDSINCE 100000) +FLAGS (\Flagged)`},
	} {
		lines := w.cmd("F", tc.cmd)
		lines = append(lines, w.cmd("Q", "NOOP")...)
		r4Log(t, tc.name, lines)
		if !r4Has(lines, `^\* \d+ FETCH .*MODSEQ`) {
			t.Errorf("%s STORE: the client never received the new MODSEQ", tc.name)
		}
		for _, line := range lines {
			if strings.Contains(line, "FETCH") && strings.Count(strings.ToUpper(line), "FLAGS (") > 1 {
				t.Errorf("%s STORE returned duplicate FLAGS: %s", tc.name, line)
			}
		}
	}
	r4Log(t, "actual clocks", w.cmd("V", "UID FETCH 1:2 (MODSEQ FLAGS)"))
}

// ---- P4: NOTIFY selected timing under pipelining, between commands, IDLE --

func r4SeedSelected(t *testing.T, n int) (obs, other *r4Wire) {
	store := r4Memory()
	obs = r4Open(t, store, nil)
	other = r4Open(t, store, nil)
	for i := 0; i < n; i++ {
		r4Append(other, "INBOX", "", fmt.Sprint("m", i))
	}
	obs.cmd("S", "SELECT INBOX")
	other.cmd("S", "SELECT INBOX")
	return obs, other
}

// RFC 5465 section 6.1.2 with RFC 3501 section 7.4.1: under SELECTED-DELAYED a
// MessageExpunge is delayed "until the client issues a command that allows
// returning information about expunged messages"; FETCH, STORE and SEARCH
// do not.
func TestReview4SelectedDelayedPipelined(t *testing.T) {
	obs, other := r4SeedSelected(t, 3)
	obs.cmd("N", "NOTIFY SET (selected-delayed (MessageNew MessageExpunge FlagChange))")
	other.cmd("D", `STORE 1 +FLAGS.SILENT (\Deleted)`)
	other.cmd("X", "EXPUNGE")
	obs.write("a FETCH 1:* (FLAGS)\r\nb STORE 2 +FLAGS (\\Seen)\r\nc SEARCH ALL\r\nd NOOP\r\n")
	lines := obs.until("d")
	r4Log(t, "pipelined", lines)
	first := r4Index(lines, `^\* \d+ EXPUNGE$`)
	cTag := r4Index(lines, `^c `)
	if first >= 0 && first < cTag {
		t.Errorf("EXPUNGE delivered while a FETCH/STORE/SEARCH was in progress under SELECTED-DELAYED")
	}
	if first < 0 {
		t.Errorf("removal never delivered by NOOP")
	}
}

// RFC 5465 sections 3.1 and 6.1.1: the client "listens for event
// notifications all the time (even when no command is in progress)" and
// SELECTED "requires the server to send immediate notifications".
func TestReview4SelectedImmediateBetweenCommands(t *testing.T) {
	obs, other := r4SeedSelected(t, 2)
	obs.cmd("N", "NOTIFY SET (selected (MessageNew MessageExpunge FlagChange))")
	other.cmd("F", `STORE 2 +FLAGS (\Flagged)`)
	got := obs.quiet(700 * time.Millisecond)
	r4Log(t, "after foreign STORE", got)
	if !r4Has(got, `^\* 2 FETCH \(.*FLAGS`) || !r4Has(got, `UID 2`) {
		t.Errorf("SELECTED FlagChange not delivered immediately with UID")
	}
	other.cmd("D", `STORE 1 +FLAGS.SILENT (\Deleted)`)
	other.cmd("X", "EXPUNGE")
	got = obs.quiet(700 * time.Millisecond)
	r4Log(t, "after foreign EXPUNGE", got)
	if !r4Has(got, `^\* 1 EXPUNGE$`) {
		t.Errorf("SELECTED MessageExpunge not delivered immediately")
	}
	r4Append(other, "INBOX", "", "new")
	got = obs.quiet(700 * time.Millisecond)
	r4Log(t, "after foreign APPEND", got)
	if !r4Has(got, `^\* 2 EXISTS$`) {
		t.Errorf("SELECTED MessageNew not delivered immediately")
	}
}

// RFC 5465 section 4: "while processing any IDLE command, the server MUST send
// exactly the same events as instructed by the client using the NOTIFY
// command."
func TestReview4NotifyIdle(t *testing.T) {
	for _, tc := range []struct {
		name, notify        string
		exists, fetch, gone bool
	}{
		{"NONE", "NOTIFY NONE", false, false, false},
		{"selected-delayed-all", "NOTIFY SET (selected-delayed (MessageNew MessageExpunge FlagChange))", true, true, true},
		{"selected-no-flags", "NOTIFY SET (selected (MessageNew MessageExpunge))", true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obs, other := r4SeedSelected(t, 2)
			obs.cmd("N", tc.notify)
			obs.write("I IDLE\r\n")
			_ = obs.c.SetReadDeadline(time.Now().Add(5 * time.Second))
			if l, err := obs.line(); err != nil || !strings.HasPrefix(l, "+") {
				t.Fatalf("IDLE continuation: %q %v", l, err)
			}
			other.cmd("F", `STORE 2 +FLAGS (\Flagged)`)
			other.cmd("D", `STORE 1 +FLAGS.SILENT (\Deleted)`)
			other.cmd("X", "EXPUNGE")
			r4Append(other, "INBOX", "", "new")
			got := obs.quiet(800 * time.Millisecond)
			obs.write("DONE\r\n")
			got = append(got, obs.until("I")...)
			r4Log(t, "IDLE", got)
			if r4Has(got, `EXISTS$`) != tc.exists {
				t.Errorf("EXISTS during IDLE = %v, registration says %v", !tc.exists, tc.exists)
			}
			if r4Has(got, `^\* \d+ FETCH`) != tc.fetch {
				t.Errorf("flag FETCH during IDLE = %v, registration says %v", !tc.fetch, tc.fetch)
			}
			if r4Has(got, `EXPUNGE$`) != tc.gone {
				t.Errorf("EXPUNGE during IDLE = %v, registration says %v", !tc.gone, tc.gone)
			}
		})
	}
}

// RFC 9586 section 3.3/7 and RFC 5465 section 5.1/5.3 together.
func TestReview4NotifyUIDOnly(t *testing.T) {
	// ENABLE is valid only in the authenticated state (RFC 5161 section 3.1),
	// so the observer enables UIDONLY before selecting.
	store := r4Memory()
	obs := r4Open(t, store, nil)
	other := r4Open(t, store, nil)
	for i := 0; i < 2; i++ {
		r4Append(other, "INBOX", "", fmt.Sprint("m", i))
	}
	other.cmd("S", "SELECT INBOX")
	r4Log(t, "ENABLE", obs.cmd("E", "ENABLE UIDONLY"))
	obs.cmd("S", "SELECT INBOX")
	obs.cmd("N", "NOTIFY SET (selected (MessageNew MessageExpunge FlagChange))")
	other.cmd("F", `STORE 2 +FLAGS (\Flagged)`)
	got := obs.quiet(700 * time.Millisecond)
	r4Log(t, "flag change", got)
	if !r4Has(got, `^\* 2 UIDFETCH \(.*FLAGS`) {
		t.Errorf("UIDONLY FlagChange is not a uidfetch-resp")
	}
	other.cmd("D", `STORE 1 +FLAGS.SILENT (\Deleted)`)
	other.cmd("X", "EXPUNGE")
	got = obs.quiet(700 * time.Millisecond)
	r4Log(t, "removal", got)
	if r4Has(got, `EXPUNGE$`) || !r4Has(got, `^\* VANISHED 1$`) {
		t.Errorf("UIDONLY removal must be VANISHED, never EXPUNGE")
	}
	obs.cmd("N", "NOTIFY NONE")
	other.cmd("F", `STORE 2 -FLAGS (\Flagged)`)
	r4Append(other, "INBOX", "", "late")
	got = append(obs.quiet(500*time.Millisecond), obs.cmd("Q", "NOOP")...)
	r4Log(t, "NONE", got)
	if r4Has(got, `UIDFETCH|EXISTS|VANISHED`) {
		t.Errorf("NOTIFY NONE delivered selected events under UIDONLY")
	}
	r4Log(t, "view", obs.cmd("U", "UID FETCH 1:* (FLAGS)"))
}

// RFC 5465 section 5.2/5.3 with RFC 5267: a search context reports changes
// with the matching EXISTS/EXPUNGE, and NOTIFY NONE suppresses those.
func TestReview4NotifyContextSearch(t *testing.T) {
	obs, other := r4SeedSelected(t, 2)
	r4Log(t, "context", obs.cmd("C", "SEARCH RETURN (UPDATE) ALL"))
	obs.cmd("N", "NOTIFY NONE")
	r4Append(other, "INBOX", "", "added")
	other.cmd("D", `STORE 1 +FLAGS.SILENT (\Deleted)`)
	other.cmd("X", "EXPUNGE")
	got := append(obs.quiet(500*time.Millisecond), obs.cmd("Q", "NOOP")...)
	r4Log(t, "suppressed", got)
	if r4Has(got, `ADDTO|REMOVEFROM|EXISTS|EXPUNGE`) {
		t.Errorf("suppressed changes leaked through the search context")
	}
	got = obs.cmd("R", "NOTIFY SET (selected (MessageNew MessageExpunge))")
	r4Log(t, "released", got)
	exists, addto := r4Index(got, `EXISTS$`), r4Index(got, `ADDTO`)
	if addto >= 0 && (exists < 0 || exists > addto) {
		t.Errorf("ESEARCH ADDTO precedes its EXISTS (RFC 5465 section 5.2)")
	}
	if !r4Has(got, `^\* 1 EXPUNGE$`) || !r4Has(got, `EXISTS$`) {
		t.Errorf("NOTIFY SET did not return accumulated changes (RFC 5465 section 3.1)")
	}
	r4Log(t, "view", obs.cmd("F", "FETCH 1:* (UID)"))
}

// ---- P5: the ghost view under SELECTED NONE -------------------------------

// RFC 5465 section 5: NOTIFY SET ... NONE "can be used as a snapshot facility".
// RFC 3501 sections 7.4.1 and 2.3.1.2: sequence numbers the client holds must
// not change meaning without an EXPUNGE. RFC 2180 section 4 permits OK/NO for
// operations on messages expunged elsewhere; a BYE or BAD is not among them.
func TestReview4GhostView(t *testing.T) {
	obs, other := r4SeedSelected(t, 3)
	obs.cmd("C", "CREATE other")
	obs.cmd("N", "NOTIFY SET (selected NONE)")
	other.cmd("D", `STORE 1 +FLAGS.SILENT (\Deleted)`)
	other.cmd("X", "EXPUNGE")
	all := []string{}
	for _, c := range []string{"FETCH 1:3 (UID FLAGS)", `STORE 1 +FLAGS (\Seen)`, "COPY 1 other", "SEARCH ALL", "FETCH 1 (BODY.PEEK[])", "NOOP"} {
		lines := obs.cmd("G", c)
		all = append(all, lines...)
		r4Log(t, c, lines)
		if strings.HasPrefix(r4Tagged(lines), "G BAD") || r4Has(lines, `^\* BYE`) {
			t.Errorf("%s on a ghost: %q", c, r4Tagged(lines))
		}
	}
	if !r4Has(all, `^\* 2 FETCH \(.*UID 2\)`) || !r4Has(all, `^\* 3 FETCH \(.*UID 3\)`) {
		t.Errorf("sequence numbers changed meaning without EXPUNGE")
	}
	if r4Has(all, `EXPUNGE$`) {
		t.Errorf("SELECTED NONE announced a foreign removal")
	}
	got := obs.cmd("R", "NOTIFY SET (selected (MessageNew MessageExpunge))")
	r4Log(t, "release", got)
	if !r4Has(got, `^\* 1 EXPUNGE$`) {
		t.Errorf("NOTIFY SET did not release the accumulated removal")
	}
	r4Log(t, "after", obs.cmd("F", "FETCH 1:* (UID)"))
}

// ---- P6: failure cleanup of selection-ending siblings --------------------

type r4UnselectBackend struct {
	*memory.Backend
	fail         atomic.Bool
	unselects    atomic.Int32
	releaseErr   error
	refuseUnauth bool
}
type r4UnselectSession struct {
	imapserver.Session
	owner *r4UnselectBackend
}
type r4UnselectSelected struct {
	imapserver.SelectedMailbox
	owner *r4UnselectBackend
}

func (b *r4UnselectBackend) Authenticate(ctx context.Context, info *imapserver.ConnInfo, cred *imapserver.Credentials, opts *imapserver.AuthenticateOptions) (imapserver.Session, error) {
	s, err := b.Backend.Authenticate(ctx, info, cred, opts)
	if err != nil {
		return nil, err
	}
	return &r4UnselectSession{s, b}, nil
}
func (s *r4UnselectSession) Select(ctx context.Context, name string, u *imapserver.Updater, opts *imapserver.SelectOptions) (*imapserver.SelectResult, error) {
	r, err := s.Session.Select(ctx, name, u, opts)
	if err == nil {
		r.Mailbox = &r4UnselectSelected{r.Mailbox, s.owner}
	}
	return r, err
}
func (s *r4UnselectSelected) Unselect(ctx context.Context, o *imapserver.UnselectOptions) error {
	s.owner.unselects.Add(1)
	err := s.SelectedMailbox.Unselect(ctx, o)
	if s.owner.fail.Load() {
		if s.owner.releaseErr != nil {
			return s.owner.releaseErr
		}
		return errors.New("private unselect diagnostic")
	}
	return err
}

// RFC 3501 section 6.3.1: a failed SELECT leaves no mailbox selected. RFC 3501
// section 6.4.2 and RFC 3691 section 2 define only OK ("now in authenticated
// state") and BAD for CLOSE and UNSELECT: a NO must at least not contradict
// the state the server actually entered, and a NO is answered only if the
// command did not take effect (RFC 3501 section 7.1.2).
func TestReview4SelectionReleaseSiblings(t *testing.T) {
	t.Run("select-replacement-fails", func(t *testing.T) {
		b := &r4UnselectBackend{Backend: r4Memory()}
		w := r4Open(t, b, nil)
		r4Append(w, "INBOX", "", "x")
		w.cmd("S", "SELECT INBOX")
		r4Log(t, "SELECT missing", w.cmd("T", "SELECT nosuch"))
		lines := w.cmd("F", "FETCH 1 (UID)")
		if !strings.HasPrefix(r4Tagged(lines), "F BAD") || b.unselects.Load() != 1 {
			t.Errorf("failed replacing SELECT: FETCH %q, unselects %d", r4Tagged(lines), b.unselects.Load())
		}
	})
	for _, cmd := range []string{"CLOSE", "UNSELECT"} {
		t.Run(strings.ToLower(cmd)+"-unselect-fails", func(t *testing.T) {
			var diagnostics atomic.Int32
			opts := &imapserver.Options{AllowInsecureAuth: true, OnError: func(_ context.Context, e *imapserver.ServerEvent) {
				diagnostics.Add(1)
			}}
			b := &r4UnselectBackend{Backend: r4Memory()}
			w := r4Open(t, b, opts)
			r4Append(w, "INBOX", "", "x")
			w.cmd("S", "SELECT INBOX")
			b.fail.Store(true)
			result := w.cmd("U", cmd)
			r4Log(t, cmd, result)
			w.write("F FETCH 1 (UID)\r\n")
			_ = w.c.SetReadDeadline(time.Now().Add(3 * time.Second))
			var after []string
			for {
				l, err := w.line()
				if err != nil {
					after = append(after, "<"+err.Error()+">")
					break
				}
				after = append(after, l)
				if strings.HasPrefix(l, "F ") {
					break
				}
			}
			r4Log(t, "next command", after)
			t.Logf("unselect calls %d", b.unselects.Load())
			tagged := r4Tagged(result)
			deselected := r4Has(after, `^F BAD`)
			alive := r4Has(after, `^F `)
			switch {
			case strings.HasPrefix(tagged, "U NO") && deselected:
				t.Errorf("%s answered NO but the server had already left the selected state", cmd)
			case strings.HasPrefix(tagged, "U NO") && !alive:
				t.Errorf("%s answered NO and the connection then ended (no BYE: %v)", cmd, !r4Has(after, `BYE`))
			}
			if strings.Contains(strings.Join(result, " "), "private") {
				t.Errorf("private diagnostic on the wire")
			}
		})
	}
}

// ---- P7: SAVEDATE of COPY / MOVE / RENAME INBOX destinations -------------

// RFC 8514 section 3: "when a message is delivered to the mailbox by the IMAP
// COPY command or the IMAP MOVE command, the save date attribute is not copied
// from the source message. Instead, the current date and time at which the
// message is delivered to a mailbox MUST be used". The save date is "the date
// and time at which the message was saved in the mailbox it is now located in".
func TestReview4SaveDateOfDeliveredCopies(t *testing.T) {
	w := r4Open(t, r4Memory(), nil)
	if !r4Has(w.cmd("C", "CAPABILITY"), `SAVEDATE`) {
		t.Skip("SAVEDATE not advertised")
	}
	r4Append(w, "INBOX", "", "one")
	r4Append(w, "INBOX", "", "two")
	w.cmd("C", "CREATE copies")
	w.cmd("C", "CREATE moves")
	w.cmd("S", "SELECT INBOX")
	saved := regexp.MustCompile(`SAVEDATE "([^"]+)"`)
	date := func(lines []string) string {
		for _, l := range lines {
			if m := saved.FindStringSubmatch(l); m != nil {
				return m[1]
			}
		}
		return ""
	}
	original := date(w.cmd("F", "UID FETCH 1 (SAVEDATE)"))
	time.Sleep(1100 * time.Millisecond)
	w.cmd("K", "UID COPY 1 copies")
	w.cmd("M", "UID MOVE 2 moves")
	w.cmd("U", "UNSELECT")
	time.Sleep(1100 * time.Millisecond)
	w.cmd("R", "RENAME INBOX archive")
	for _, mbox := range []string{"copies", "moves", "archive"} {
		w.cmd("S", "EXAMINE "+mbox)
		got := date(w.cmd("F", "FETCH 1 (SAVEDATE)"))
		t.Logf("%s SAVEDATE %q, source %q", mbox, got, original)
		if got == "" || got == original {
			t.Errorf("%s: SAVEDATE copied from the source message", mbox)
		}
	}
}

// ---- P8: QRESYNC records MOVE removals in both backends (A-3) ------------

func TestReview4MoveRemovalResync(t *testing.T) {
	for _, kind := range []string{"memory", "maildir"} {
		t.Run(kind, func(t *testing.T) {
			b := r4Backend(t, kind)
			w := r4Open(t, b, nil)
			r4Append(w, "INBOX", "", "a")
			r4Append(w, "INBOX", "", "b")
			w.cmd("C", "CREATE other")
			w.cmd("E", "ENABLE QRESYNC")
			sel := w.cmd("S", "SELECT INBOX")
			modseq := r4HighestModSeq(sel)
			validity := regexp.MustCompile(`UIDVALIDITY (\d+)`).FindStringSubmatch(strings.Join(sel, " "))[1]
			w.cmd("M", "UID MOVE 1 other")
			w.cmd("U", "UNSELECT")
			w2 := r4Open(t, b, nil)
			w2.cmd("E", "ENABLE QRESYNC")
			lines := w2.cmd("S", fmt.Sprintf("SELECT INBOX (QRESYNC (%s %d))", validity, modseq))
			r4Log(t, "resync", lines)
			if !r4Has(lines, `^\* VANISHED \(EARLIER\) 1$`) {
				t.Errorf("MOVE removal not reported by QRESYNC")
			}
		})
	}
}

// ---- P9: RFC 5465 grammar — STATUS indicator and several groups ----------

// RFC 5465 section 8: notify-set = "SET" [status-indicator] SP event-groups,
// event-groups = event-group *(SP event-group). The server must accept the
// legal form; the root client's encoding is checked against the same server.
func TestReview4NotifyGrammarAndRootClient(t *testing.T) {
	w := r4Open(t, r4Memory(), nil)
	legal := w.cmd("N", "NOTIFY SET STATUS (selected (MessageNew MessageExpunge)) (personal (MessageNew MessageExpunge))")
	r4Log(t, "legal", legal)
	if !strings.HasPrefix(r4Tagged(legal), "N OK") {
		t.Errorf("server refused RFC 5465 section 8 NOTIFY SET STATUS with two groups")
	}
	for _, tc := range []struct {
		name    string
		filters []imapclient.NotifyFilter
		options *imapclient.NotifyOptions
	}{
		{"status", []imapclient.NotifyFilter{{Specifier: imapclient.NotifySelected, Events: []imapclient.NotifyEventName{imapclient.NotifyEventMessageNew, imapclient.NotifyEventMessageExpunge}}}, &imapclient.NotifyOptions{Status: true}},
		{"two-groups", []imapclient.NotifyFilter{
			{Specifier: imapclient.NotifySelected, Events: []imapclient.NotifyEventName{imapclient.NotifyEventMessageNew, imapclient.NotifyEventMessageExpunge}},
			{Specifier: imapclient.NotifyPersonal, Events: []imapclient.NotifyEventName{imapclient.NotifyEventMessageNew, imapclient.NotifyEventMessageExpunge}},
		}, nil},
	} {
		t.Run("root-client/"+tc.name, func(t *testing.T) {
			c, peer := net.Pipe()
			s := imapserver.New(r4Memory(), &imapserver.Options{AllowInsecureAuth: true})
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			defer s.Close(context.Background(), nil)
			go func() { _ = s.ServeConn(ctx, peer, nil) }()
			capture := &r4CaptureConn{Conn: c}
			client := imapclient.NewClient(capture, &imapclient.Options{AllowInsecureAuth: true})
			defer client.Close()
			if err := client.Login(ctx, "alice", "secret", nil); err != nil {
				t.Fatal(err)
			}
			err := client.Notify(ctx, tc.filters, tc.options)
			t.Logf("client sent %q, err %v", capture.notifyLine(), err)
			if err != nil {
				t.Errorf("root client NOTIFY rejected by an RFC 5465 server: %v", err)
			}
		})
	}
}

// r4CaptureConn records what the root client writes.
type r4CaptureConn struct {
	net.Conn
	written strings.Builder
}

func (c *r4CaptureConn) Write(p []byte) (int, error) {
	c.written.Write(p)
	return c.Conn.Write(p)
}

func (c *r4CaptureConn) notifyLine() string {
	for _, l := range strings.Split(c.written.String(), "\r\n") {
		if strings.Contains(l, "NOTIFY") {
			return l
		}
	}
	return ""
}

// ---- P10: the public UpdateVanished vocabulary on the wire ---------------

type r4VanishModeBackend struct {
	*memory.Backend
	earlier bool
}
type r4VanishModeSession struct {
	imapserver.Session
	earlier bool
}

func (b *r4VanishModeBackend) Authenticate(ctx context.Context, info *imapserver.ConnInfo, cred *imapserver.Credentials, opts *imapserver.AuthenticateOptions) (imapserver.Session, error) {
	s, err := b.Backend.Authenticate(ctx, info, cred, opts)
	if err != nil {
		return nil, err
	}
	return &r4VanishModeSession{Session: s, earlier: b.earlier}, nil
}
func (s *r4VanishModeSession) Select(ctx context.Context, name string, u *imapserver.Updater, opts *imapserver.SelectOptions) (*imapserver.SelectResult, error) {
	wrapped := &imapserver.Updater{PushFunc: func(b *imapserver.UpdateBatch) error {
		out := &imapserver.UpdateBatch{Before: b.Before, After: b.After, Origin: b.Origin}
		for _, ch := range b.Changes {
			if e, ok := ch.(*imapserver.UpdateExpunge); ok {
				out.Changes = append(out.Changes, &imapserver.UpdateVanished{UIDs: imap.UIDSetNum(e.UID), Earlier: s.earlier})
				continue
			}
			out.Changes = append(out.Changes, ch)
		}
		return u.Push(out)
	}}
	return s.Session.Select(ctx, name, wrapped, opts)
}

// RFC 7162 sections 3.2.3 and 3.2.10: VANISHED replaces EXPUNGE only after
// ENABLE QRESYNC; a removal in the selected mailbox caused by another
// connection is reported without EARLIER, and VANISHED (EARLIER) "MUST NOT"
// make the client decrement sequence numbers, so it cannot announce one.
func TestReview4UpdateVanishedProjection(t *testing.T) {
	for _, tc := range []struct {
		name            string
		qresync, earlie bool
	}{
		{"rev1-session", false, false},
		{"qresync-earlier", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := r4Memory()
			obs := r4Open(t, &r4VanishModeBackend{Backend: store, earlier: tc.earlie}, nil)
			other := r4Open(t, store, nil)
			r4Append(other, "INBOX", "", "a")
			r4Append(other, "INBOX", "", "b")
			if tc.qresync {
				obs.cmd("E", "ENABLE QRESYNC")
			}
			obs.cmd("S", "SELECT INBOX")
			other.cmd("S", "SELECT INBOX")
			other.cmd("D", `UID STORE 1 +FLAGS.SILENT (\Deleted)`)
			other.cmd("X", "EXPUNGE")
			lines := obs.cmd("Q", "NOOP")
			lines = append(lines, obs.cmd("F", "FETCH 1:* (UID)")...)
			r4Log(t, "observer", lines)
			if !tc.qresync && r4Has(lines, `VANISHED`) {
				t.Errorf("VANISHED sent to a session that never enabled QRESYNC")
			}
			if tc.earlie && r4Has(lines, `^\* VANISHED \(EARLIER\)`) && r4Has(lines, `^\* 1 FETCH \(.*UID 2\)`) {
				t.Errorf("unsolicited VANISHED (EARLIER) renumbered the client's view")
			}
		})
	}
}
