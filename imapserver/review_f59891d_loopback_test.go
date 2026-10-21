package imapserver_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
	"github.com/kiliant/go-imap/imapserver/memory"
)

func TestReviewGUIDOnlySelection(t *testing.T) {
	for _, kind := range []string{"memory", "maildir"} {
		t.Run(kind, func(t *testing.T) {
			b := r4Backend(t, kind)
			review6fFill(t, b, 3)
			seed := r4Open(t, b, nil)
			seed.cmd("S", "SELECT INBOX")
			seed.cmd("D", `UID STORE 1 +FLAGS.SILENT (\Deleted)`)
			seed.cmd("X", "UID EXPUNGE 1")
			// Preserve ordinary rev1 UNSEEN while UIDs differ from sequences.
			if got := seed.cmd("S", "EXAMINE INBOX"); !r4Has(got, `\[UNSEEN 1\]`) {
				t.Fatal(got)
			}
			w := r4Open(t, b, nil)
			w.cmd("E", "ENABLE UIDONLY QRESYNC")
			for _, command := range []string{"SELECT INBOX", "EXAMINE INBOX", "SELECT INBOX (QRESYNC (1 1 1:9))", "EXAMINE INBOX (QRESYNC (1 1 1:9))"} {
				got := w.cmd("S", command)
				sweepWant(t, got, "OK")
				if r4Has(got, `\[UNSEEN `) {
					t.Fatalf("sequence-number UNSEEN under UIDONLY: %q", got)
				}
				if strings.HasPrefix(command, "EXAMINE") {
					for _, store := range []string{`STORE 1 +FLAGS (\Seen)`, `STORE 1 (UNCHANGEDSINCE 1) +FLAGS (\Seen)`, `STORE $ +FLAGS (\Seen)`} {
						sweepWant(t, w.cmd("T", store), "BAD [UIDREQUIRED]")
					}
					sweepWant(t, w.cmd("T", `UID STORE 2 +FLAGS (\Seen)`), "NO [READ-ONLY]")
				}
			}
		})
	}
}

func reviewGClock(t *testing.T, w *r4Wire) uint64 {
	t.Helper()
	got := w.cmd("H", "STATUS INBOX (HIGHESTMODSEQ)")
	for _, line := range got {
		if i := strings.Index(line, "HIGHESTMODSEQ "); i >= 0 {
			var clock uint64
			if _, err := fmt.Sscanf(line[i:], "HIGHESTMODSEQ %d", &clock); err == nil && clock != 0 {
				return clock
			}
		}
	}
	t.Fatalf("missing clock: %q", got)
	return 0
}

func TestReviewGFetchVanishedHistory(t *testing.T) {
	for _, kind := range []string{"memory", "maildir"} {
		t.Run(kind, func(t *testing.T) {
			b := r4Backend(t, kind)
			review6fFill(t, b, 6)
			a := r4Open(t, b, nil)
			a.cmd("E", "ENABLE QRESYNC")
			a.cmd("S", "SELECT INBOX")
			a.cmd("D", `UID STORE 6 +FLAGS.SILENT (\Deleted)`)
			a.cmd("X", "UID EXPUNGE 6")
			clock := reviewGClock(t, a)
			a.cmd("D", `UID STORE 2,5 +FLAGS.SILENT (\Deleted)`)
			a.cmd("X", "UID EXPUNGE 2,5")
			a.cmd("D", `UID STORE 4 +FLAGS.SILENT (\Flagged)`)
			a.cmd("L", "LOGOUT")
			for _, uidOnly := range []bool{false, true} {
				w := r4Open(t, b, nil)
				enable := "ENABLE QRESYNC"
				if uidOnly {
					enable += " UIDONLY"
				}
				w.cmd("E", enable)
				w.cmd("S", "EXAMINE INBOX")
				for _, tc := range []struct {
					set, extra, dead string
					fetched          bool
				}{
					{"1:10", "", "2,5", true}, {"10:1", "", "2,5", true},
					{"2", "", "2", false}, {"5", "", "5", false}, {"6", "", "", false},
					{"1:*", "", "2", true}, {"1:10", " PARTIAL 1:1", "2,5", false},
					{"1:10", " PARTIAL -1:-1", "2,5", true}, {"1:10", " PARTIAL 50:60", "2,5", false},
				} {
					got := w.cmd("F", fmt.Sprintf("UID FETCH %s (FLAGS) (CHANGEDSINCE %d VANISHED%s)", tc.set, clock, tc.extra))
					sweepWant(t, got, "OK")
					deadAt, fetchAt := -1, -1
					for i, line := range got {
						if strings.HasPrefix(line, "* VANISHED (EARLIER) ") {
							if deadAt >= 0 || line != "* VANISHED (EARLIER) "+tc.dead {
								t.Fatalf("history mismatch: %q", got)
							}
							deadAt = i
						}
						if strings.HasPrefix(line, "* ") && (strings.Contains(line, " FETCH ") || strings.Contains(line, " UIDFETCH ")) {
							fetchAt = i
							if uidOnly && !strings.HasPrefix(line, "* 4 UIDFETCH ") || !uidOnly && !strings.Contains(line, "UID 4") {
								t.Fatal(got)
							}
						}
					}
					if (deadAt >= 0) != (tc.dead != "") || (fetchAt >= 0) != tc.fetched || deadAt >= 0 && fetchAt >= 0 && deadAt > fetchAt {
						t.Fatalf("history/fetch ordering: %q", got)
					}
				}
				// Saving an empty result must not turn KnownUIDs into unrestricted.
				w.cmd("Q", "UID SEARCH RETURN (SAVE) UID 99")
				got := w.cmd("F", fmt.Sprintf("UID FETCH $ (FLAGS) (CHANGEDSINCE %d VANISHED)", clock))
				if r4Has(got, `^\* VANISHED`) {
					t.Fatal(got)
				}
			}
			// Explicit history requests still work when no messages survive.
			w := r4Open(t, b, nil)
			w.cmd("E", "ENABLE QRESYNC")
			w.cmd("S", "SELECT INBOX")
			w.cmd("D", `UID STORE 1:* +FLAGS.SILENT (\Deleted)`)
			w.cmd("X", "UID EXPUNGE 1:10")
			got := w.cmd("F", fmt.Sprintf("UID FETCH 2,5 (FLAGS) (CHANGEDSINCE %d VANISHED)", clock))
			if !r4Has(got, `^\* VANISHED \(EARLIER\) 2,5$`) {
				t.Fatal(got)
			}
		})
	}
}

type reviewGBackend struct {
	*memory.Backend
	calls    atomic.Int32
	noModSeq bool
	resync   func(context.Context, *imapserver.QResyncSelect) (*imapserver.QResyncResult, error)
}
type reviewGSession struct {
	imapserver.Session
	owner *reviewGBackend
}
type reviewGMailbox struct {
	imapserver.SelectedMailbox
	owner *reviewGBackend
}

func (b *reviewGBackend) Authenticate(ctx context.Context, i *imapserver.ConnInfo, cr *imapserver.Credentials, o *imapserver.AuthenticateOptions) (imapserver.Session, error) {
	s, err := b.Backend.Authenticate(ctx, i, cr, o)
	if err != nil {
		return nil, err
	}
	return &reviewGSession{Session: s, owner: b}, nil
}
func (s *reviewGSession) Select(ctx context.Context, name string, u *imapserver.Updater, o *imapserver.SelectOptions) (*imapserver.SelectResult, error) {
	r, err := s.Session.Select(ctx, name, u, o)
	if err != nil {
		return nil, err
	}
	r.Mailbox = &reviewGMailbox{SelectedMailbox: r.Mailbox, owner: s.owner}
	if s.owner.noModSeq {
		r.Snapshot.Status.NoModSeq = true
		r.Snapshot.Status.HighestModSeq = 0
	}
	return r, nil
}
func (m *reviewGMailbox) Resync(ctx context.Context, p *imapserver.QResyncSelect, o *imapserver.QResyncOptions) (*imapserver.QResyncResult, error) {
	m.owner.calls.Add(1)
	if m.owner.resync != nil {
		return m.owner.resync(ctx, p)
	}
	return m.SelectedMailbox.(imapserver.QResyncMailbox).Resync(ctx, p, o)
}

func TestReviewGFetchVanishedRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, enable, command string
		noModSeq, filter      bool
	}{
		{"plain form", "ENABLE QRESYNC", "FETCH 1 (FLAGS) (CHANGEDSINCE 1 VANISHED)", false, false},
		{"not enabled", "", "UID FETCH 1 (FLAGS) (CHANGEDSINCE 1 VANISHED)", false, false},
		{"filtered", "ENABLE QRESYNC", "UID FETCH 1 (FLAGS) (CHANGEDSINCE 1 VANISHED)", false, true},
		{"nomodseq", "ENABLE QRESYNC", "UID FETCH 1 (FLAGS) (CHANGEDSINCE 1 VANISHED)", true, false},
		{"no changed-since", "ENABLE QRESYNC", "UID FETCH 1 (FLAGS) (VANISHED)", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &reviewGBackend{Backend: r4Memory(), noModSeq: tc.noModSeq}
			review6fFill(t, b, 1)
			var backend imapserver.Backend = b
			if tc.filter {
				backend = &reviewGFilter{reviewGBackend: b}
			}
			w := r4Open(t, backend, nil)
			if tc.enable != "" {
				w.cmd("E", tc.enable)
			}
			w.cmd("S", "SELECT INBOX")
			sweepWant(t, w.cmd("F", tc.command), "BAD")
			if b.calls.Load() != 0 {
				t.Fatal("invalid VANISHED reached Resync")
			}
			sweepWant(t, w.cmd("N", "UID FETCH 1 (FLAGS)"), "OK")
		})
	}
}

type reviewGFilter struct{ *reviewGBackend }

func (*reviewGFilter) AllowCapability(_ context.Context, name string, _ *imapserver.CapabilityFilterOptions) bool {
	return name != "QRESYNC"
}

func TestReviewGFetchVanishedBackendErrors(t *testing.T) {
	for _, mode := range []string{"failure", "deadline", "nil"} {
		t.Run(mode, func(t *testing.T) {
			b := &reviewGBackend{Backend: r4Memory()}
			b.resync = func(ctx context.Context, p *imapserver.QResyncSelect) (*imapserver.QResyncResult, error) {
				if p.UIDValidity != 1 || p.ModSeq != 1 || !p.KnownUIDs.Equal(imap.UIDSetRange(1, 10)) || !p.SeqMatchSeqNums.IsEmpty() {
					t.Errorf("wrong Resync arguments: %#v", p)
				}
				switch mode {
				case "failure":
					return nil, errors.New("private resync failure")
				case "deadline":
					<-ctx.Done()
					return nil, ctx.Err()
				default:
					return nil, nil
				}
			}
			review6fFill(t, b, 1)
			w := r4Open(t, b, &imapserver.Options{AllowInsecureAuth: true, Limits: imapserver.Limits{CommandTimeout: 75 * time.Millisecond}})
			w.cmd("E", "ENABLE QRESYNC")
			w.cmd("S", "SELECT INBOX")
			for i := 0; i < 2; i++ {
				got := w.cmd("F", "UID FETCH 1:10 (FLAGS) (CHANGEDSINCE 1 VANISHED PARTIAL 9:10)")
				want := "OK"
				if mode == "failure" {
					want = "NO [SERVERBUG]"
				}
				if mode == "deadline" {
					want = "NO [UNAVAILABLE]"
				}
				sweepWant(t, got, want)
				if r4Has(got, `^\* .*FETCH `) {
					t.Fatal(got)
				}
				sweepWant(t, w.cmd("N", "UID FETCH 1 (FLAGS)"), "OK")
			}
			if b.calls.Load() != 2 {
				t.Fatalf("Resync call count: %d", b.calls.Load())
			}
		})
	}
}
