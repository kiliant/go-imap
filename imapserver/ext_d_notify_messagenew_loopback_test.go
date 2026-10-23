package imapserver_test

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
	"github.com/kiliant/go-imap/imapserver/memory"
)

// messageNewBackend lets a test withhold BINARY, select mailboxes as
// NOMODSEQ, fail the selected mailbox's Fetch before or after writing its
// first message, and count Notify calls without a registration.
type messageNewBackend struct {
	*memory.Backend
	noBinary   bool
	noModSeq   bool
	failFetch  atomic.Bool
	failAfter  atomic.Bool
	notifyNone atomic.Int32
}

type messageNewSession struct {
	imapserver.Session
	backend *messageNewBackend
}

type messageNewMailbox struct {
	imapserver.SelectedMailbox
	backend *messageNewBackend
}

func (b *messageNewBackend) SupportsCapability(ctx context.Context, name string, options *imapserver.CapabilitySupportOptions) bool {
	if b.noBinary && strings.HasPrefix(name, "BINARY") {
		return false
	}
	return b.Backend.SupportsCapability(ctx, name, options)
}

func (b *messageNewBackend) Authenticate(ctx context.Context, info *imapserver.ConnInfo, credentials *imapserver.Credentials, options *imapserver.AuthenticateOptions) (imapserver.Session, error) {
	session, err := b.Backend.Authenticate(ctx, info, credentials, options)
	if err != nil {
		return nil, err
	}
	return &messageNewSession{session, b}, nil
}

func (s *messageNewSession) Select(ctx context.Context, name string, updater *imapserver.Updater, options *imapserver.SelectOptions) (*imapserver.SelectResult, error) {
	result, err := s.Session.Select(ctx, name, updater, options)
	if err == nil {
		result.Mailbox = &messageNewMailbox{result.Mailbox, s.backend}
		if s.backend.noModSeq {
			result.Snapshot.Status.NoModSeq = true
		}
	}
	return result, err
}

func (s *messageNewSession) Notify(ctx context.Context, updater *imapserver.SessionUpdater, config *imapserver.NotifyConfig, options *imapserver.NotifyOptions) error {
	if config == nil {
		s.backend.notifyNone.Add(1)
	}
	return s.Session.(imapserver.NotifySession).Notify(ctx, updater, config, options)
}

func (m *messageNewMailbox) Fetch(ctx context.Context, writer *imapserver.FetchWriter, uids imap.UIDSet, options *imapserver.FetchOptions) error {
	if m.backend.failFetch.Load() {
		return errors.New("fetch failed")
	}
	if m.backend.failAfter.Load() {
		first, _ := uids.Nums()
		if err := m.SelectedMailbox.Fetch(ctx, writer, imap.UIDSetNum(first[0]), options); err != nil {
			return err
		}
		return errors.New("fetch failed after one message")
	}
	return m.SelectedMailbox.Fetch(ctx, writer, uids, options)
}

// messageNewPair opens a watching session and a second one that delivers.
// Both are logged in; neither has a mailbox selected.
func messageNewPair(t *testing.T, b imapserver.Backend) (watcher, other *r4Wire) {
	t.Helper()
	return r4Open(t, b, nil), r4Open(t, b, nil)
}

func messageNewCmd(t *testing.T, w *r4Wire, tag, command string) []string {
	t.Helper()
	lines := w.cmd(tag, command)
	if !strings.HasPrefix(r4Tagged(lines), tag+" OK") {
		t.Fatalf("%s = %q", command, lines)
	}
	return lines
}

// lineIndex is the index of the first line matching re, or -1.
func lineIndex(lines []string, re string) int {
	x := regexp.MustCompile(re)
	for i, line := range lines {
		if x.MatchString(line) {
			return i
		}
	}
	return -1
}

// RFC 5465 section 5.2: a new message in the selected mailbox is announced by
// EXISTS, followed by an unsolicited FETCH with the requested attributes.
func TestNotifyMessageNewFetchAttributes(t *testing.T) {
	for _, specifier := range []string{"selected", "selected-delayed"} {
		t.Run(specifier, func(t *testing.T) {
			w, other := messageNewPair(t, r4Memory())
			messageNewCmd(t, w, "N", "NOTIFY SET ("+specifier+" (MessageNew (UID FLAGS BODY.PEEK[HEADER.FIELDS (Subject)]) MessageExpunge))")
			messageNewCmd(t, w, "S", "SELECT INBOX")
			r4Append(other, "INBOX", `\Flagged`, "hello")
			lines := messageNewCmd(t, w, "Q", "NOOP")
			exists := lineIndex(lines, `^\* 1 EXISTS$`)
			fetch := lineIndex(lines, `^\* 1 FETCH \(BODY\[HEADER\.FIELDS \(Subject\)\] \{\d+\}<\d+ octets> FLAGS \([^)]*\\Flagged[^)]*\) UID 1\)$`)
			if exists < 0 || fetch < exists {
				t.Fatalf("EXISTS then FETCH expected: %q", lines)
			}
		})
	}
}

// The client's own APPEND gets its EXISTS but no MessageNew FETCH, as RFC
// 5465 section 5.2 recommends.
func TestNotifyMessageNewSkipsOwnAppend(t *testing.T) {
	w := r4Open(t, r4Memory(), nil)
	messageNewCmd(t, w, "N", "NOTIFY SET (selected (MessageNew (UID FLAGS) MessageExpunge))")
	messageNewCmd(t, w, "S", "SELECT INBOX")
	lines := r4Append(w, "INBOX", "", "mine")
	lines = append(lines, messageNewCmd(t, w, "Q", "NOOP")...)
	if lineIndex(lines, `^\* 1 EXISTS$`) < 0 || lineIndex(lines, `FETCH`) >= 0 {
		t.Fatalf("own APPEND = %q", lines)
	}
}

// The registration follows the selection: only the mailbox selected when the
// message arrives is fetched, in the client's current numbering.
func TestNotifyMessageNewFollowsSelection(t *testing.T) {
	w, other := messageNewPair(t, r4Memory())
	messageNewCmd(t, other, "C", "CREATE Other")
	r4Append(other, "Other", "", "existing")
	messageNewCmd(t, w, "N", "NOTIFY SET (selected (MessageNew (UID) MessageExpunge))")
	messageNewCmd(t, w, "S", "SELECT INBOX")
	r4Append(other, "INBOX", "", "inbox one")
	if lines := messageNewCmd(t, w, "Q", "NOOP"); lineIndex(lines, `^\* 1 FETCH \(UID 1\)$`) < 0 {
		t.Fatalf("INBOX delivery = %q", lines)
	}
	messageNewCmd(t, w, "S", "SELECT Other")
	r4Append(other, "INBOX", "", "inbox two")
	r4Append(other, "Other", "", "other two")
	lines := messageNewCmd(t, w, "Q", "NOOP")
	if lineIndex(lines, `^\* 2 EXISTS$`) < 0 || lineIndex(lines, `^\* 2 FETCH \(UID 2\)$`) < 0 || strings.Count(strings.Join(lines, "\n"), "FETCH") != 1 {
		t.Fatalf("delivery after SELECT Other = %q", lines)
	}
}

// RFC 5465 section 5.2: the FETCH follows the ESEARCH ADDTO the new message
// causes, which itself follows the EXISTS.
func TestNotifyMessageNewFollowsContextUpdate(t *testing.T) {
	w, other := messageNewPair(t, r4Memory())
	messageNewCmd(t, w, "N", "NOTIFY SET (selected (MessageNew (UID) MessageExpunge))")
	messageNewCmd(t, w, "S", "SELECT INBOX")
	messageNewCmd(t, w, "CTX", "SEARCH RETURN (UPDATE) ALL")
	r4Append(other, "INBOX", "", "matched")
	lines := messageNewCmd(t, w, "Q", "NOOP")
	exists := lineIndex(lines, `^\* 1 EXISTS$`)
	addto := lineIndex(lines, `^\* ESEARCH \(TAG "CTX"\) ADDTO \(0 1\)$`)
	fetch := lineIndex(lines, `^\* 1 FETCH \(UID 1\)$`)
	if exists < 0 || addto < exists || fetch < addto {
		t.Fatalf("want EXISTS, ADDTO, FETCH in order: %q", lines)
	}
}

// A replacement registration without attributes stops the FETCH; NOTIFY NONE
// silences the selected mailbox altogether.
func TestNotifyMessageNewReplacement(t *testing.T) {
	w, other := messageNewPair(t, r4Memory())
	messageNewCmd(t, w, "N", "NOTIFY SET (selected (MessageNew (UID) MessageExpunge))")
	messageNewCmd(t, w, "S", "SELECT INBOX")
	messageNewCmd(t, w, "N", "NOTIFY SET (selected (MessageNew MessageExpunge))")
	r4Append(other, "INBOX", "", "one")
	if lines := messageNewCmd(t, w, "Q", "NOOP"); lineIndex(lines, `^\* 1 EXISTS$`) < 0 || lineIndex(lines, `FETCH`) >= 0 {
		t.Fatalf("after replacement = %q", lines)
	}
	messageNewCmd(t, w, "N", "NOTIFY NONE")
	r4Append(other, "INBOX", "", "two")
	if lines := messageNewCmd(t, w, "Q", "NOOP"); len(lines) != 1 {
		t.Fatalf("after NOTIFY NONE = %q", lines)
	}
}

// Data that cannot be delivered ends the registration with
// NOTIFICATIONOVERFLOW and NOTIFY NONE behaviour (RFC 5465 section 5.8),
// leaving the connection usable.
func TestNotifyMessageNewOverflow(t *testing.T) {
	b := &messageNewBackend{Backend: r4Memory()}
	w, other := messageNewPair(t, b)
	messageNewCmd(t, w, "N", "NOTIFY SET (selected (MessageNew (UID) MessageExpunge))")
	messageNewCmd(t, w, "S", "SELECT INBOX")
	b.failFetch.Store(true)
	r4Append(other, "INBOX", "", "lost")
	lines := messageNewCmd(t, w, "Q", "NOOP")
	if lineIndex(lines, `^\* 1 EXISTS$`) < 0 || lineIndex(lines, `^\* OK \[NOTIFICATIONOVERFLOW\]`) < 0 || lineIndex(lines, `FETCH`) >= 0 {
		t.Fatalf("overflow = %q", lines)
	}
	if b.notifyNone.Load() != 1 {
		t.Fatalf("backend Notify(nil) calls = %d, want 1", b.notifyNone.Load())
	}
	b.failFetch.Store(false)
	r4Append(other, "INBOX", "", "after")
	if lines := messageNewCmd(t, w, "Q", "NOOP"); len(lines) != 1 {
		t.Fatalf("after overflow = %q", lines)
	}
}

// MODSEQ among the attributes is a CONDSTORE enabling use, as FETCH MODSEQ
// is, and UIDONLY reshapes the unsolicited response like any other.
func TestNotifyMessageNewModSeqAndUIDOnly(t *testing.T) {
	t.Run("MODSEQ", func(t *testing.T) {
		w, other := messageNewPair(t, r4Memory())
		messageNewCmd(t, w, "N", "NOTIFY SET (selected (MessageNew (UID MODSEQ) MessageExpunge))")
		if lines := messageNewCmd(t, w, "S", "SELECT INBOX"); lineIndex(lines, `HIGHESTMODSEQ`) < 0 {
			t.Fatalf("SELECT after MODSEQ registration = %q", lines)
		}
		r4Append(other, "INBOX", "", "clocked")
		if lines := messageNewCmd(t, w, "Q", "NOOP"); lineIndex(lines, `^\* 1 FETCH \(MODSEQ \(\d+\) UID 1\)$`) < 0 {
			t.Fatalf("MODSEQ delivery = %q", lines)
		}
	})
	t.Run("UIDONLY", func(t *testing.T) {
		w, other := messageNewPair(t, r4Memory())
		messageNewCmd(t, w, "E", "ENABLE UIDONLY")
		messageNewCmd(t, w, "N", "NOTIFY SET (selected (MessageNew (UID FLAGS) MessageExpunge))")
		messageNewCmd(t, w, "S", "SELECT INBOX")
		r4Append(other, "INBOX", "", "uidonly")
		if lines := messageNewCmd(t, w, "Q", "NOOP"); lineIndex(lines, `^\* 1 UIDFETCH \(FLAGS \([^)]*\)\)$`) < 0 {
			t.Fatalf("UIDONLY delivery = %q", lines)
		}
	})
}

// Attributes are gated as FETCH items are: one the session was not offered is
// refused with NO [CANNOT], before the backend is asked.
func TestNotifyMessageNewGatesItems(t *testing.T) {
	b := &messageNewBackend{Backend: r4Memory(), noBinary: true}
	w := r4Open(t, b, nil)
	lines := w.cmd("N", "NOTIFY SET (selected (MessageNew (UID BINARY.PEEK[1]) MessageExpunge))")
	if !strings.HasPrefix(r4Tagged(lines), "N NO [CANNOT]") {
		t.Fatalf("BINARY without BINARY = %q", lines)
	}
}

// A Fetch that fails after delivering data ends the registration; the
// responses already sent stand and the connection stays usable.
func TestNotifyMessageNewOverflowAfterPartialDelivery(t *testing.T) {
	b := &messageNewBackend{Backend: r4Memory()}
	w, other := messageNewPair(t, b)
	messageNewCmd(t, w, "N", "NOTIFY SET (selected (MessageNew (UID) MessageExpunge))")
	messageNewCmd(t, w, "S", "SELECT INBOX")
	// The backend writes the message's data, then fails the call.
	b.failAfter.Store(true)
	r4Append(other, "INBOX", "", "written")
	lines := messageNewCmd(t, w, "Q", "NOOP")
	fetch := lineIndex(lines, `^\* 1 FETCH \(UID 1\)$`)
	overflow := lineIndex(lines, `^\* OK \[NOTIFICATIONOVERFLOW\]`)
	if fetch < 0 || overflow < fetch {
		t.Fatalf("partial delivery = %q", lines)
	}
}

// Where CONDSTORE is not in effect for the selection, MODSEQ is left out, and
// a list left with nothing sends no FETCH: a response needs an item.
func TestNotifyMessageNewNoModSeq(t *testing.T) {
	for _, test := range []struct{ items, want string }{
		{"MODSEQ", ""},
		{"UID MODSEQ", `^\* 1 FETCH \(UID 1\)$`},
	} {
		t.Run(test.items, func(t *testing.T) {
			w, other := messageNewPair(t, &messageNewBackend{Backend: r4Memory(), noModSeq: true})
			messageNewCmd(t, w, "N", "NOTIFY SET (selected (MessageNew ("+test.items+") MessageExpunge))")
			messageNewCmd(t, w, "S", "SELECT INBOX")
			r4Append(other, "INBOX", "", "unclocked")
			lines := messageNewCmd(t, w, "Q", "NOOP")
			fetches := strings.Count(strings.Join(lines, "\n"), "FETCH")
			if lineIndex(lines, `^\* 1 EXISTS$`) < 0 || test.want == "" && fetches != 0 || test.want != "" && (fetches != 1 || lineIndex(lines, test.want) < 0) {
				t.Fatalf("NOMODSEQ delivery = %q", lines)
			}
		})
	}
}

// Under UIDONLY the UID is the response's subject, so a list without UID still
// works, and a list of only UID keeps the item rather than send "()". UID
// FETCH n (UID) gets the same (RFC 9586 section 3.3).
func TestNotifyMessageNewUIDOnlyItems(t *testing.T) {
	for _, test := range []struct{ items, want string }{
		{"FLAGS", `^\* 1 UIDFETCH \(FLAGS \([^)]*\)\)$`},
		{"UID", `^\* 1 UIDFETCH \(UID 1\)$`},
	} {
		t.Run(test.items, func(t *testing.T) {
			w, other := messageNewPair(t, r4Memory())
			messageNewCmd(t, w, "E", "ENABLE UIDONLY")
			messageNewCmd(t, w, "N", "NOTIFY SET (selected (MessageNew ("+test.items+") MessageExpunge))")
			messageNewCmd(t, w, "S", "SELECT INBOX")
			r4Append(other, "INBOX", "", "uidonly")
			if lines := messageNewCmd(t, w, "Q", "NOOP"); lineIndex(lines, test.want) < 0 {
				t.Fatalf("UIDONLY (%s) delivery = %q", test.items, lines)
			}
		})
	}
	w := r4Open(t, r4Memory(), nil)
	r4Append(w, "INBOX", "", "one")
	messageNewCmd(t, w, "E", "ENABLE UIDONLY")
	messageNewCmd(t, w, "S", "SELECT INBOX")
	if lines := messageNewCmd(t, w, "F", "UID FETCH 1 (UID)"); lineIndex(lines, `^\* 1 UIDFETCH \(UID 1\)$`) < 0 {
		t.Fatalf("UID FETCH 1 (UID) under UIDONLY = %q", lines)
	}
}
