package imapserver_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

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
			remove := `* ESEARCH (TAG "REG")` + marker + " REMOVEFROM (1 3,2,1)"
			added := "2,1"
			if uidMode {
				added = "3,1"
			}
			add := `* ESEARCH (TAG "REG")` + marker + " ADDTO (1 " + added + ")"
			r, e, a := slices.Index(lines, remove), slices.Index(lines, "* 2 EXPUNGE"), slices.Index(lines, add)
			if r < 0 || e <= r || a <= e {
				t.Fatalf("ordered context must remove before EXPUNGE and add after it: %q", lines)
			}
			raw := "Subject: new\r\n\r\nnew body\r\n"
			lines = w.cmd("A", fmt.Sprintf("APPEND INBOX \"04-Jan-2024 10:00:00 +0000\" {%d+}\r\n%s", len(raw), raw))
			added = "3,2,1"
			if uidMode {
				added = "4,3,1"
			}
			add = `* ESEARCH (TAG "REG")` + marker + " ADDTO (1 " + added + ")"
			if exists, addAt := slices.Index(lines, "* 3 EXISTS"), slices.Index(lines, add); exists < 0 || addAt <= exists {
				t.Fatalf("context addition must preserve full sort order after EXISTS: %q", lines)
			}
			sweepWant(t, w.cmd("C", "CANCELUPDATE REG"), "OK")
			lines = w.cmd("A", fmt.Sprintf("APPEND INBOX {%d+}\r\n%s", len(raw), raw))
			if strings.Contains(strings.Join(lines, "\n"), `ESEARCH (TAG "REG")`) {
				t.Fatalf("cancelled context still updated: %q", lines)
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
