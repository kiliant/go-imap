package imapserver_test

// Further probes for the 4a61a32 review: release siblings, judgement calls
// and R2 under UIDONLY. Assertions state RFC or documented-contract
// requirements; observation-only probes say so and assert nothing.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
	"github.com/kiliant/go-imap/imapserver/memory"
)

func (s *r4UnselectSession) Unauthenticate(ctx context.Context, o *imapserver.UnauthenticateOptions) error {
	if s.owner.refuseUnauth {
		return &imap.Error{Type: imap.ErrorTypeNo, Code: imap.CodeCannot, Text: "refused"}
	}
	return s.Session.(imapserver.UnauthenticateSession).Unauthenticate(ctx, o)
}

// RFC 8437 section 3 (UNAUTHENTICATE) and RFC 3501 section 7.1.2: a tagged NO
// means the command did not complete, so the client keeps its state; the
// server must not have left the selected state behind it.
func TestReview4UnauthenticateReleaseFailure(t *testing.T) {
	b := &r4UnselectBackend{Backend: r4Memory()}
	w := r4Open(t, b, nil)
	if !r4Has(w.cmd("C", "CAPABILITY"), `UNAUTHENTICATE`) {
		t.Skip("UNAUTHENTICATE not advertised through the wrapper")
	}
	r4Append(w, "INBOX", "", "x")
	w.cmd("S", "SELECT INBOX")
	b.fail.Store(true)
	result := w.cmd("U", "UNAUTHENTICATE")
	r4Log(t, "UNAUTHENTICATE", result)
	after := w.cmd("F", "FETCH 1 (UID)")
	r4Log(t, "next command", after)
	if strings.HasPrefix(r4Tagged(result), "U NO") && strings.HasPrefix(r4Tagged(after), "F BAD") {
		t.Errorf("UNAUTHENTICATE answered NO but the selection was already gone")
	}
}

// Observation (judgement call 7): RENAME INBOX to a name beneath INBOX.
func TestReview4ObserveRenameInboxBeneathInbox(t *testing.T) {
	for _, kind := range []string{"memory", "maildir"} {
		t.Run(kind, func(t *testing.T) {
			w := r4Open(t, r4Backend(t, kind), nil)
			r4Append(w, "INBOX", "", "x")
			r4Log(t, "RENAME INBOX INBOX/old", w.cmd("R", "RENAME INBOX INBOX/old"))
			r4Log(t, "LIST", w.cmd("L", `LIST "" "*"`))
			r4Log(t, "STATUS INBOX", w.cmd("T", "STATUS INBOX (MESSAGES)"))
		})
	}
}

// A NotifySession that refuses every SET after the first.
type r4NotifyRefuseBackend struct{ *r4VanishBackend }
type r4NotifyRefuseSession struct {
	imapserver.Session
	inner imapserver.NotifySession
	calls atomic.Int32
}

func (b *r4NotifyRefuseBackend) Authenticate(ctx context.Context, info *imapserver.ConnInfo, cred *imapserver.Credentials, opts *imapserver.AuthenticateOptions) (imapserver.Session, error) {
	s, err := b.Backend.Authenticate(ctx, info, cred, opts)
	if err != nil {
		return nil, err
	}
	return &r4NotifyRefuseSession{Session: s, inner: s.(imapserver.NotifySession)}, nil
}
func (s *r4NotifyRefuseSession) Notify(ctx context.Context, u *imapserver.SessionUpdater, c *imapserver.NotifyConfig, o *imapserver.NotifyOptions) error {
	if c != nil && s.calls.Add(1) > 1 {
		return &imap.Error{Type: imap.ErrorTypeNo, Code: imap.CodeBadEvent, CodeArgs: "(MessageNew MessageExpunge)", Text: "refused"}
	}
	return s.inner.Notify(ctx, u, c, o)
}

// Observation (judgement call 4): after a refused NOTIFY SET, which selected
// policy applies, and does a registration for other mailboxes survive?
func TestReview4ObserveNotifyRefusalPolicy(t *testing.T) {
	store := r4Memory()
	obs := r4Open(t, &r4NotifyRefuseBackend{&r4VanishBackend{store}}, nil)
	other := r4Open(t, store, nil)
	r4Append(other, "INBOX", "", "seed")
	obs.cmd("C", "CREATE watched")
	obs.cmd("S", "SELECT INBOX")
	r4Log(t, "first SET", obs.cmd("N", "NOTIFY SET (selected NONE) (mailboxes watched (MessageNew MessageExpunge))"))
	r4Log(t, "refused SET", obs.cmd("N", "NOTIFY SET (selected (MessageNew MessageExpunge FlagChange)) (mailboxes watched (MessageNew MessageExpunge FlagChange))"))
	other.cmd("S", "SELECT INBOX")
	other.cmd("F", `STORE 1 +FLAGS (\Flagged)`)
	r4Append(other, "watched", "", "w")
	got := append(obs.quiet(500*time.Millisecond), obs.cmd("Q", "NOOP")...)
	r4Log(t, "after refusal", got)
}

// RFC 5465 sections 3.1 and 6.1 (judgement call 5).
func TestReview4NotifySelectedEventClassification(t *testing.T) {
	w := r4Open(t, r4Memory(), nil)
	for _, tc := range []struct{ cmd, want string }{
		{"NOTIFY SET (selected (MessageNew MessageExpunge FutureEvent))", "N NO [BADEVENT"},
		{"NOTIFY SET (selected (MailboxName))", "N BAD"},
		{"NOTIFY SET (selected (MessageNew MessageExpunge AnnotationChange))", "N NO [BADEVENT"},
		{"NOTIFY SET (personal (MailboxName))", "N NO [BADEVENT"},
		{"NOTIFY SET (selected (MessageNew MessageExpunge)) (selected-delayed NONE)", "N BAD"},
		{"NOTIFY SET STATUS", "N BAD"},
		{"NOTIFY SET (mailboxes () (MessageNew MessageExpunge))", "N BAD"},
	} {
		lines := w.cmd("N", tc.cmd)
		r4Log(t, tc.cmd, lines)
		if !strings.HasPrefix(r4Tagged(lines), tc.want) {
			t.Errorf("%s: got %q, want prefix %q", tc.cmd, r4Tagged(lines), tc.want)
		}
		if strings.HasPrefix(tc.want, "N NO [BADEVENT") && !strings.Contains(r4Tagged(lines), "BADEVENT (MessageNew MessageExpunge FlagChange)") {
			t.Errorf("BADEVENT must list every supported event")
		}
	}
}

// R2 under UIDONLY: RFC 7162 section 3.1.3 still requires MODSEQ, and RFC
// 9586 section 3.3 the uidfetch-resp form.
func TestReview4ConditionalSilentUIDOnly(t *testing.T) {
	for _, kind := range []string{"memory", "maildir"} {
		t.Run(kind, func(t *testing.T) {
			w := r4Open(t, r4Backend(t, kind), nil)
			r4Append(w, "INBOX", "", "x")
			r4Log(t, "ENABLE", w.cmd("E", "ENABLE UIDONLY CONDSTORE"))
			w.cmd("S", "SELECT INBOX")
			lines := w.cmd("F", `UID STORE 1 (UNCHANGEDSINCE 100000) +FLAGS.SILENT (\Flagged)`)
			r4Log(t, "conditional silent", lines)
			if !r4Has(lines, `^\* 1 UIDFETCH \(.*MODSEQ`) || r4Has(lines, `FLAGS`) {
				t.Errorf("conditional STORE.SILENT under UIDONLY must report MODSEQ only, as uidfetch-resp")
			}
			// Observation: RFC 7162 Example 14 shows MODSEQ for an
			// unconditional .SILENT store too; no normative text requires it.
			r4Log(t, "unconditional silent (observation)", w.cmd("G", `UID STORE 1 +FLAGS.SILENT (\Seen)`))
		})
	}
}

// Observation: what a refused UNSELECT looks like to OnError.
func TestReview4ObserveUnselectDiagnostic(t *testing.T) {
	var kinds atomic.Value
	kinds.Store("")
	opts := &imapserver.Options{AllowInsecureAuth: true, OnError: func(_ context.Context, e *imapserver.ServerEvent) {
		kinds.Store(kinds.Load().(string) + fmt.Sprintf("[%v %s %v]", e.Kind, e.Command, errors.Unwrap(e.Err) != nil || e.Err != nil))
	}}
	b := &r4UnselectBackend{Backend: r4Memory()}
	w := r4Open(t, b, opts)
	w.cmd("S", "SELECT INBOX")
	b.fail.Store(true)
	r4Log(t, "UNSELECT", w.cmd("U", "UNSELECT"))
	time.Sleep(200 * time.Millisecond)
	t.Logf("OnError: %s", kinds.Load())
}

// A-2: UNAUTHENTICATE ends the NOTIFY registration (RFC 8437 section 3: the
// connection returns to the not-authenticated state; RFC 5465 section 3.1:
// the registration lasts until the next NOTIFY or the connection closes, and
// cannot outlive the session it describes).
func TestReview4UnauthenticateEndsNotify(t *testing.T) {
	store := r4Memory()
	w := r4Open(t, store, nil)
	other := r4Open(t, store, nil)
	w.cmd("C", "CREATE watched")
	w.cmd("N", "NOTIFY SET (mailboxes watched (MessageNew MessageExpunge))")
	w.cmd("U", "UNAUTHENTICATE")
	r4Append(other, "watched", "", "late")
	got := w.quiet(400 * time.Millisecond)
	got = append(got, w.cmd("L", "LOGIN alice secret")...)
	got = append(got, w.cmd("Q", "NOOP")...)
	r4Log(t, "after UNAUTHENTICATE", got)
	if r4Has(got, `^\* STATUS`) {
		t.Errorf("the previous session's NOTIFY registration delivered after UNAUTHENTICATE")
	}
}

// A-4: Resync's Changed may name messages newer than the snapshot; SELECT
// must not fail for that (RFC 7162 section 3.2.5: changed messages since the
// client's modseq are reported for the client's view).
type r4ChangedBackend struct{ *memory.Backend }
type r4ChangedSession struct{ imapserver.Session }
type r4ChangedSelected struct {
	imapserver.SelectedMailbox
	resync imapserver.QResyncMailbox
	cond   imapserver.CondStoreMailbox
}

func (b *r4ChangedBackend) Authenticate(ctx context.Context, info *imapserver.ConnInfo, cred *imapserver.Credentials, opts *imapserver.AuthenticateOptions) (imapserver.Session, error) {
	s, err := b.Backend.Authenticate(ctx, info, cred, opts)
	if err != nil {
		return nil, err
	}
	return &r4ChangedSession{s}, nil
}
func (s *r4ChangedSession) Select(ctx context.Context, name string, u *imapserver.Updater, o *imapserver.SelectOptions) (*imapserver.SelectResult, error) {
	r, err := s.Session.Select(ctx, name, u, o)
	if err == nil {
		r.Mailbox = &r4ChangedSelected{SelectedMailbox: r.Mailbox, resync: r.Mailbox.(imapserver.QResyncMailbox), cond: r.Mailbox.(imapserver.CondStoreMailbox)}
	}
	return r, err
}
func (m *r4ChangedSelected) Resync(ctx context.Context, q *imapserver.QResyncSelect, o *imapserver.QResyncOptions) (*imapserver.QResyncResult, error) {
	r, err := m.resync.Resync(ctx, q, o)
	if err != nil || r == nil {
		return r, err
	}
	changed := append(imap.UIDSet(nil), r.Changed...)
	changed = append(changed, imap.UIDSetNum(9999)...)
	return &imapserver.QResyncResult{Vanished: r.Vanished, Changed: changed}, nil
}
func (m *r4ChangedSelected) StoreCondStore(ctx context.Context, w *imapserver.FetchWriter, uids imap.UIDSet, f *imapserver.StoreFlags, o *imapserver.StoreOptions) (*imapserver.CondStoreResult, error) {
	return m.cond.StoreCondStore(ctx, w, uids, f, o)
}

func TestReview4QResyncChangedBeyondSnapshot(t *testing.T) {
	w := r4Open(t, &r4ChangedBackend{r4Memory()}, nil)
	r4Append(w, "INBOX", "", "a")
	w.cmd("E", "ENABLE QRESYNC")
	lines := w.cmd("S", "SELECT INBOX (QRESYNC (1 1))")
	r4Log(t, "SELECT", lines)
	if !strings.HasPrefix(r4Tagged(lines), "S OK") {
		t.Errorf("SELECT failed because Changed named a UID outside the snapshot")
	}
}

// A-5: RFC 3501 section 2.3.2 — a message is recent in the mailbox no
// session has seen it in; a RENAME INBOX destination is such a mailbox.
func TestReview4RenameInboxRecent(t *testing.T) {
	for _, kind := range []string{"memory", "maildir"} {
		t.Run(kind, func(t *testing.T) {
			w := r4Open(t, r4Backend(t, kind), nil)
			r4Append(w, "INBOX", "", "a")
			r4Append(w, "INBOX", "", "b")
			w.cmd("S", "SELECT INBOX")
			w.cmd("U", "UNSELECT")
			w.cmd("R", "RENAME INBOX archive")
			lines := w.cmd("S", "SELECT archive")
			r4Log(t, "SELECT archive", lines)
			if !r4Has(lines, `^\* 2 RECENT$`) {
				t.Errorf("RENAME INBOX destination messages are not recent")
			}
		})
	}
}

// Judgement call 1: RFC 5465 section 3.1, "A successful NOTIFY SET command
// MUST cause the server to immediately return any accumulated changes to the
// currently selected mailbox (if any), such as flag changes and new or
// expunged messages", and section 5, "NOTIFY SET ... NONE can be used as a
// snapshot facility by clients".
func TestReview4NotifySnapshotReturnsFlagChanges(t *testing.T) {
	for _, tc := range []struct{ name, first, second string }{
		{"snapshot", "NOTIFY SET (selected NONE)", "NOTIFY SET (selected NONE)"},
		{"enable-flagchange", "NOTIFY SET (selected (MessageNew MessageExpunge))", "NOTIFY SET (selected (MessageNew MessageExpunge FlagChange))"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obs, other := r4SeedSelected(t, 2)
			obs.cmd("N", tc.first)
			other.cmd("F", `STORE 1 +FLAGS (\Flagged)`)
			r4Append(other, "INBOX", "", "new")
			got := obs.cmd("N2", tc.second)
			r4Log(t, "second SET", got)
			if !r4Has(got, `EXISTS$`) {
				t.Errorf("accumulated addition not returned")
			}
			if !r4Has(got, `^\* 1 FETCH \(.*\\Flagged`) {
				t.Errorf("accumulated flag change not returned by NOTIFY SET")
			}
		})
	}
}

// Consequence of dropping suppressed flag changes for a QRESYNC client: a
// later MODSEQ it receives (here from its own STORE, with no fence) is a
// checkpoint past the dropped change, so RFC 7162 section 3.2.5 resync from
// that checkpoint never reports it. Observation of the data path; the
// assertion is the RFC 7162 resync contract.
func TestReview4SuppressedFlagChangeLostAcrossResync(t *testing.T) {
	store := r4Memory()
	obs := r4Open(t, store, nil)
	other := r4Open(t, store, nil)
	r4Append(other, "INBOX", "", "a")
	r4Append(other, "INBOX", "", "b")
	obs.cmd("E", "ENABLE QRESYNC")
	sel := obs.cmd("S", "SELECT INBOX")
	validity := regexp.MustCompile(`UIDVALIDITY (\d+)`).FindStringSubmatch(strings.Join(sel, " "))[1]
	other.cmd("S", "SELECT INBOX")
	obs.cmd("N", "NOTIFY SET (selected NONE)")
	other.cmd("F", `UID STORE 1 +FLAGS (\Flagged)`)
	own := obs.cmd("O", `UID STORE 2 +FLAGS (\Seen)`)
	r4Log(t, "own STORE", own)
	checkpoint := uint64(0)
	if m := regexp.MustCompile(`MODSEQ \((\d+)\)`).FindStringSubmatch(strings.Join(own, " ")); m != nil {
		checkpoint, _ = strconv.ParseUint(m[1], 10, 64)
	}
	if fenced := r4HighestModSeq(own); fenced != 0 && fenced < checkpoint {
		checkpoint = fenced
	}
	again := r4Open(t, store, nil)
	again.cmd("E", "ENABLE QRESYNC")
	lines := again.cmd("S", fmt.Sprintf("SELECT INBOX (QRESYNC (%s %d))", validity, checkpoint))
	r4Log(t, "resync from client checkpoint", lines)
	if !r4Has(lines, `^\* 1 FETCH \(.*\\Flagged`) {
		t.Errorf("flag change of UID 1 is lost: suppressed, never returned, and older than the checkpoint %d the client was given", checkpoint)
	}
}

// A silent own mutation reports no flags, even when conditional STORE reports
// its MODSEQ. It cannot acknowledge another session's older withheld flags.
func TestReview4SilentStorePreservesWithheldFlags(t *testing.T) {
	for _, command := range []string{
		`UID STORE 1 +FLAGS.SILENT (\Seen)`,
		`UID STORE 1 (UNCHANGEDSINCE 99999) +FLAGS.SILENT (\Seen)`,
	} {
		t.Run(command, func(t *testing.T) {
			store := r4Memory()
			obs, other := r4Open(t, store, nil), r4Open(t, store, nil)
			r4Append(other, "INBOX", "", "a")
			obs.cmd("E", "ENABLE QRESYNC")
			obs.cmd("S", "SELECT INBOX")
			other.cmd("S", "SELECT INBOX")
			obs.cmd("N", "NOTIFY SET (selected NONE)")
			other.cmd("F", `UID STORE 1 +FLAGS (\Flagged)`)
			own := obs.cmd("O", command)
			if r4HighestModSeq(own) == 0 {
				t.Errorf("silent STORE did not fence withheld flags: %v", own)
			}
			got := obs.cmd("N2", "NOTIFY SET (selected NONE)")
			if !r4Has(got, `^\* 1 FETCH \(.*\\Flagged`) || !r4Has(got, `^\* 1 FETCH \(.*\\Seen`) {
				t.Errorf("snapshot lost the latest flags: %v", got)
			}
		})
	}
}

func TestReview4CleanupFailuresAreReported(t *testing.T) {
	for _, failure := range []error{errors.New("private diagnostic"), context.Canceled, &imap.Error{Type: imap.ErrorTypeNo, Code: imap.CodeCannot, Text: "private refusal"}} {
		for _, command := range []string{"CLOSE", "UNSELECT", "UNAUTHENTICATE"} {
			t.Run(command+"/"+failure.Error(), func(t *testing.T) {
				diagnostics := make(chan error, 4)
				b := &r4UnselectBackend{Backend: r4Memory(), releaseErr: failure}
				w := r4Open(t, b, &imapserver.Options{AllowInsecureAuth: true, OnError: func(_ context.Context, event *imapserver.ServerEvent) {
					diagnostics <- event.Err
				}})
				r4Append(w, "INBOX", `\Deleted`, "x")
				w.cmd("S", "SELECT INBOX")
				b.fail.Store(true)
				result := w.cmd("U", command)
				if !strings.HasPrefix(r4Tagged(result), "U OK") || strings.Contains(strings.Join(result, " "), "private") {
					t.Fatalf("completed release = %v", result)
				}
				if got := w.cmd("F", "FETCH 1 (UID)"); !strings.HasPrefix(r4Tagged(got), "F BAD") {
					t.Fatalf("selection survived release: %v", got)
				}
				if got := w.cmd("Q", "NOOP"); !strings.HasPrefix(r4Tagged(got), "Q OK") {
					t.Fatalf("connection did not survive release: %v", got)
				}
				select {
				case got := <-diagnostics:
					if !errors.Is(got, failure) {
						t.Errorf("diagnostic = %v, want %v", got, failure)
					}
				case <-time.After(time.Second):
					t.Fatal("cleanup failure did not reach OnError")
				}
				if got := b.unselects.Load(); got != 1 {
					t.Errorf("Unselect calls = %d", got)
				}
			})
		}
	}
}

func TestReview4UnauthenticateRefusalPreservesSelection(t *testing.T) {
	b := &r4UnselectBackend{Backend: r4Memory(), refuseUnauth: true}
	w := r4Open(t, b, nil)
	r4Append(w, "INBOX", "", "x")
	w.cmd("S", "SELECT INBOX")
	if got := w.cmd("U", "UNAUTHENTICATE"); !strings.HasPrefix(r4Tagged(got), "U NO [CANNOT]") {
		t.Fatalf("refusal = %v", got)
	}
	if got := w.cmd("F", "FETCH 1 (UID)"); !r4Has(got, `^\* 1 FETCH`) || !strings.HasPrefix(r4Tagged(got), "F OK") {
		t.Fatalf("refusal released selection: %v", got)
	}
	if got := b.unselects.Load(); got != 0 {
		t.Errorf("refusal called Unselect %d times", got)
	}
}
