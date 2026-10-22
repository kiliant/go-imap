package imapserver_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
	"github.com/kiliant/go-imap/imapserver/memory"
)

// Sorted contexts must retain the complete ordered result even when the
// initial response returns only a PARTIAL window (RFC 5267 section 4.3).
func TestSortContextUpdates(t *testing.T) {
	for _, uidMode := range []bool{false, true} {
		t.Run(fmt.Sprint(uidMode), func(t *testing.T) {
			w := r4Open(t, r4Memory(), nil)
			for i := 1; i <= 3; i++ {
				raw := fmt.Sprintf("Subject: m%d\r\n\r\nbody\r\n", i)
				sweepWant(t, w.cmd("A", fmt.Sprintf("APPEND INBOX \"0%d-Jan-2024 10:00:00 +0000\" {%d+}\r\n%s", i, len(raw), raw)), "OK")
			}
			sweepWant(t, w.cmd("S", "SELECT INBOX"), "OK")
			prefix, marker := "", ""
			if uidMode {
				prefix, marker = "UID ", " UID"
			}
			sweepWant(t, w.cmd("REG", prefix+"SORT RETURN (UPDATE PARTIAL 2:2 COUNT) (REVERSE ARRIVAL) US-ASCII ALL"), "OK")
			sweepWant(t, w.cmd("D", "UID STORE 2 +FLAGS (\\Deleted)"), "OK")
			lines := w.cmd("E", "UID EXPUNGE 2")
			// UID 2, message 2, held position 2 of 3,2,1. The rest keeps its
			// order, so nothing else is sent.
			remove := `* ESEARCH (TAG "REG")` + marker + " REMOVEFROM (2 2)"
			r, e := slices.Index(lines, remove), slices.Index(lines, "* 2 EXPUNGE")
			if r < 0 || e <= r || r4Has(lines, `ADDTO|REMOVEFROM \(1 `) {
				t.Fatalf("ordered context must remove the expunged position before EXPUNGE, and nothing more: %q", lines)
			}
			raw := "Subject: new\r\n\r\nnew body\r\n"
			lines = w.cmd("A", fmt.Sprintf("APPEND INBOX \"04-Jan-2024 10:00:00 +0000\" {%d+}\r\n%s", len(raw), raw))
			// UID 4, message 3, sorts first in REVERSE ARRIVAL.
			added := "3"
			if uidMode {
				added = "4"
			}
			add := `* ESEARCH (TAG "REG")` + marker + " ADDTO (1 " + added + ")"
			if exists, addAt := slices.Index(lines, "* 3 EXISTS"), slices.Index(lines, add); exists < 0 || addAt <= exists || r4Has(lines, "REMOVEFROM") {
				t.Fatalf("the new message must enter at its own position after EXISTS: %q", lines)
			}
			sweepWant(t, w.cmd("C", "CANCELUPDATE REG"), "OK")
			lines = w.cmd("A", fmt.Sprintf("APPEND INBOX {%d+}\r\n%s", len(raw), raw))
			if strings.Contains(strings.Join(lines, "\n"), `ESEARCH (TAG "REG")`) {
				t.Fatalf("cancelled context still updated: %q", lines)
			}
		})
	}
}

// Expunged entries of a sorted context leave at their own positions before the
// EXPUNGE responses. Adjacent entries share a pair, and pairs run from the
// highest position down, so each stays valid as a client processes them in
// order (RFC 5267 section 4.3.4).
func TestSortContextExpungeRuns(t *testing.T) {
	for order, want := range map[string]string{
		// 1 2 3 4 5: 5 at position 5, then 2 and 3 from position 2.
		"ARRIVAL": `* ESEARCH (TAG "REG") REMOVEFROM (5 5 2 2:3)`,
		// 5 4 3 2 1: 3 and 2 from position 3, then 5 at position 1.
		"REVERSE ARRIVAL": `* ESEARCH (TAG "REG") REMOVEFROM (3 3,2 1 5)`,
	} {
		t.Run(order, func(t *testing.T) {
			w := r4Open(t, r4Memory(), nil)
			for i := 1; i <= 5; i++ {
				r4Append(w, "INBOX", "", fmt.Sprint("m", i))
			}
			sweepWant(t, w.cmd("S", "SELECT INBOX"), "OK")
			sweepWant(t, w.cmd("REG", "SORT RETURN (UPDATE) ("+order+") US-ASCII ALL"), "OK")
			sweepWant(t, w.cmd("D", `UID STORE 2,3,5 +FLAGS.SILENT (\Deleted)`), "OK")
			lines := w.cmd("E", "EXPUNGE")
			r, e := slices.Index(lines, want), r4Index(lines, `EXPUNGE$`)
			if r < 0 || e <= r || strings.Count(strings.Join(lines, "\n"), "ESEARCH") != 1 {
				t.Fatalf("want only %s before the EXPUNGE responses: %q", want, lines)
			}
		})
	}
}

func TestContextUpdateTagReuse(t *testing.T) {
	w := r4Open(t, r4Memory(), nil)
	sweepWant(t, w.cmd("S", "SELECT INBOX"), "OK")
	sweepWant(t, w.cmd("REG", "UID SEARCH RETURN (UPDATE) ALL"), "OK")
	for _, command := range []string{
		"UID SEARCH RETURN (ALL UPDATE) SEEN",
		"UID SORT RETURN (ALL UPDATE) (ARRIVAL) US-ASCII SEEN",
		"ESEARCH IN (selected) RETURN (ALL UPDATE) SEEN",
	} {
		if lines := w.cmd("REG", command); !strings.HasPrefix(r4Tagged(lines), "REG BAD ") {
			t.Fatalf("active context tag reused by %s: %q", command, lines)
		}
	}
	// A rejected replacement must leave the original ALL query intact.
	raw := "Subject: unseen\r\n\r\nbody\r\n"
	lines := w.cmd("A", fmt.Sprintf("APPEND INBOX {%d+}\r\n%s", len(raw), raw))
	if !slices.Contains(lines, `* ESEARCH (TAG "REG") UID ADDTO (0 1)`) {
		t.Fatalf("rejected tag reuse changed the registration: %q", lines)
	}
	sweepWant(t, w.cmd("C", "CANCELUPDATE REG"), "OK")
	sweepWant(t, w.cmd("REG", "UID SORT RETURN (ALL UPDATE) (ARRIVAL) US-ASCII ALL"), "OK")
	sweepWant(t, w.cmd("U", "UNSELECT"), "OK")
	sweepWant(t, w.cmd("S", "SELECT INBOX"), "OK")
	sweepWant(t, w.cmd("REG", "UID SEARCH RETURN (UPDATE) ALL"), "OK")
}

type contextFailureBackend struct{ *memory.Backend }
type contextFailureSession struct{ imapserver.Session }
type contextFailureMailbox struct {
	imapserver.SelectedMailbox
	calls int
}

func (b *contextFailureBackend) Authenticate(ctx context.Context, info *imapserver.ConnInfo, credentials *imapserver.Credentials, options *imapserver.AuthenticateOptions) (imapserver.Session, error) {
	session, err := b.Backend.Authenticate(ctx, info, credentials, options)
	if err != nil {
		return nil, err
	}
	return &contextFailureSession{session}, nil
}

func (s *contextFailureSession) Select(ctx context.Context, name string, updater *imapserver.Updater, options *imapserver.SelectOptions) (*imapserver.SelectResult, error) {
	result, err := s.Session.Select(ctx, name, updater, options)
	if err == nil {
		result.Mailbox = &contextFailureMailbox{SelectedMailbox: result.Mailbox}
	}
	return result, err
}

func (m *contextFailureMailbox) Search(ctx context.Context, query *imapserver.SearchQuery, options *imapserver.SearchOptions) (*imapserver.SearchResult, error) {
	m.calls++
	if m.calls > 1 {
		return nil, errors.New("context refresh failed")
	}
	return m.SelectedMailbox.Search(ctx, query, options)
}

func TestContextRefreshFailureIsNoUpdate(t *testing.T) {
	// ']' is valid in a command tag and remains safe inside NOUPDATE's quoted
	// tag. The generic response-code argument writer deliberately refuses it.
	for _, tag := range []string{"REG", "REG]"} {
		t.Run(tag, func(t *testing.T) {
			w := r4Open(t, &contextFailureBackend{r4Memory()}, nil)
			sweepWant(t, w.cmd("S", "SELECT INBOX"), "OK")
			sweepWant(t, w.cmd(tag, "UID SEARCH RETURN (ALL UPDATE) ALL"), "OK")
			raw := "Subject: update\r\n\r\nbody\r\n"
			lines := w.cmd("A", fmt.Sprintf("APPEND INBOX {%d+}\r\n%s", len(raw), raw))
			if want := fmt.Sprintf("* NO [NOUPDATE %q] search context could not be refreshed", tag); !slices.Contains(lines, want) {
				t.Fatalf("refresh failure = %q, want %q", lines, want)
			}
			lines = w.cmd("A", fmt.Sprintf("APPEND INBOX {%d+}\r\n%s", len(raw), raw))
			if strings.Contains(strings.Join(lines, "\n"), "NOUPDATE") {
				t.Fatalf("failed context was not dropped: %q", lines)
			}
		})
	}
}

// slowRefreshBackend stalls the armed kind of context refresh, recording the
// deadline the framework gave it. It waits for that context to end or, when
// late, ignores cancellation and returns a real result after the deadline.
type slowRefreshBackend struct {
	*memory.Backend
	kind     string
	late     bool
	armed    atomic.Bool
	observed chan slowRefresh
}

type slowRefresh struct {
	deadline  bool
	remaining time.Duration
	err       error
}

type slowRefreshSession struct {
	imapserver.Session
	backend *slowRefreshBackend
}

type slowRefreshMailbox struct {
	imapserver.SelectedMailbox
	backend *slowRefreshBackend
}

func (b *slowRefreshBackend) Authenticate(ctx context.Context, info *imapserver.ConnInfo, credentials *imapserver.Credentials, options *imapserver.AuthenticateOptions) (imapserver.Session, error) {
	session, err := b.Backend.Authenticate(ctx, info, credentials, options)
	if err != nil {
		return nil, err
	}
	return &slowRefreshSession{session, b}, nil
}

func (s *slowRefreshSession) Select(ctx context.Context, name string, updater *imapserver.Updater, options *imapserver.SelectOptions) (*imapserver.SelectResult, error) {
	result, err := s.Session.Select(ctx, name, updater, options)
	if err == nil {
		result.Mailbox = &slowRefreshMailbox{result.Mailbox, s.backend}
	}
	return result, err
}

func (m *slowRefreshMailbox) block(ctx context.Context, kind string) bool {
	if kind != m.backend.kind || !m.backend.armed.Load() {
		return false
	}
	deadline, ok := ctx.Deadline()
	got := slowRefresh{deadline: ok, remaining: time.Until(deadline)}
	if m.backend.late {
		time.Sleep(got.remaining + 50*time.Millisecond)
		got.err = ctx.Err()
	} else {
		select {
		case <-ctx.Done():
			got.err = ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	m.backend.observed <- got
	return true
}

func (m *slowRefreshMailbox) Search(ctx context.Context, query *imapserver.SearchQuery, options *imapserver.SearchOptions) (*imapserver.SearchResult, error) {
	if m.block(ctx, "SEARCH") {
		if m.backend.late {
			return m.SelectedMailbox.Search(context.WithoutCancel(ctx), query, options)
		}
		return nil, errors.New("refresh blocked")
	}
	return m.SelectedMailbox.Search(ctx, query, options)
}

func (m *slowRefreshMailbox) Sort(ctx context.Context, query *imapserver.SearchQuery, keys []imap.SortKeySpec, options *imapserver.SortOptions) ([]imap.UID, error) {
	if m.block(ctx, "SORT") {
		if m.backend.late {
			return m.SelectedMailbox.(imapserver.SortMailbox).Sort(context.WithoutCancel(ctx), query, keys, options)
		}
		return nil, errors.New("refresh blocked")
	}
	return m.SelectedMailbox.(imapserver.SortMailbox).Sort(ctx, query, keys, options)
}

// A refresh runs in an update drain, where no command context applies.
// Limits.CommandTimeout must still bound it: a stalled refresh is cancelled at
// the deadline, and one that ignores cancellation and succeeds late is a miss
// too. Either way its context is dropped with NOUPDATE, and a context
// refreshed before it keeps updating on a connection that stays usable.
func TestContextRefreshHonoursCommandTimeout(t *testing.T) {
	const timeout = 500 * time.Millisecond
	commands := map[string]string{
		"SEARCH": "UID SEARCH RETURN (ALL UPDATE) ALL",
		"SORT":   "UID SORT RETURN (ALL UPDATE) (ARRIVAL) US-ASCII ALL",
	}
	for _, late := range []bool{false, true} {
		for slow, healthy := range map[string]string{"SEARCH": "SORT", "SORT": "SEARCH"} {
			t.Run(fmt.Sprintf("%s/late=%v", slow, late), func(t *testing.T) {
				b := &slowRefreshBackend{Backend: r4Memory(), kind: slow, late: late, observed: make(chan slowRefresh, 4)}
				w := r4Open(t, b, &imapserver.Options{AllowInsecureAuth: true, Limits: imapserver.Limits{CommandTimeout: timeout}})
				sweepWant(t, w.cmd("S", "SELECT INBOX"), "OK")
				sweepWant(t, w.cmd("OK", commands[healthy]), "OK")
				sweepWant(t, w.cmd("SLOW", commands[slow]), "OK")
				b.armed.Store(true)
				raw := "Subject: update\r\n\r\nbody\r\n"
				lines := w.cmd("A", fmt.Sprintf("APPEND INBOX {%d+}\r\n%s", len(raw), raw))
				select {
				case got := <-b.observed:
					if !got.deadline || got.remaining > timeout {
						t.Fatalf("%s refresh deadline = %v (set %v), want within %v", slow, got.remaining, got.deadline, timeout)
					}
					if got.err == nil {
						t.Fatalf("%s refresh was not cancelled at the deadline", slow)
					}
				default:
					t.Fatalf("%s refresh did not reach the backend: %q", slow, lines)
				}
				sweepWant(t, lines, "OK")
				joined := strings.Join(lines, "\n")
				if want := fmt.Sprintf(`* NO [NOUPDATE "SLOW"] %s context could not be refreshed`, strings.ToLower(slow)); !strings.Contains(joined, want) {
					t.Fatalf("timed-out refresh = %q, want %q", lines, want)
				}
				if !strings.Contains(joined, `* ESEARCH (TAG "OK") UID ADDTO`) {
					t.Fatalf("context refreshed before the stall lost its update: %q", lines)
				}
				sweepWant(t, w.cmd("N", "NOOP"), "OK")
				lines = w.cmd("A", fmt.Sprintf("APPEND INBOX {%d+}\r\n%s", len(raw), raw))
				sweepWant(t, lines, "OK")
				joined = strings.Join(lines, "\n")
				if strings.Contains(joined, "NOUPDATE") || !strings.Contains(joined, `* ESEARCH (TAG "OK") UID ADDTO`) {
					t.Fatalf("after the timeout, want only the surviving context updated: %q", lines)
				}
			})
		}
	}
}
