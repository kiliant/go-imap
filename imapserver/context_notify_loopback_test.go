package imapserver_test

import (
	"context"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
	"github.com/kiliant/go-imap/imapserver/memory"
)

// Context updates under NOTIFY: RFC 5465 sections 3, 5.1 and 5.2 with RFC
// 5267 section 4.3. An ADDTO or REMOVEFROM belongs to the event that caused
// it. It is suppressed with that event, follows the FETCH or EXISTS that
// announces it, precedes the EXPUNGE that renumbers it away, and is never
// sent for a change the session has not applied yet.

// contextRegistrations are the three ways to register an updating context
// under tag C. removal and addition are what message 1 leaving and rejoining
// an UNSEEN result of messages 1 and 2 produce.
var contextRegistrations = []struct {
	name, command, removal, addition string
}{
	{
		name:     "search",
		command:  "SEARCH RETURN (UPDATE) UNSEEN",
		removal:  `^\* ESEARCH \(TAG "C"\) REMOVEFROM \(0 1\)$`,
		addition: `^\* ESEARCH \(TAG "C"\) ADDTO \(0 1\)$`,
	},
	{
		// A sorted context replaces its list from position one.
		name:     "sort",
		command:  "SORT RETURN (UPDATE) (ARRIVAL) US-ASCII UNSEEN",
		removal:  `^\* ESEARCH \(TAG "C"\) REMOVEFROM \(1 1:2\)$`,
		addition: `^\* ESEARCH \(TAG "C"\) ADDTO \(1 1:2\)$`,
	},
	{
		name:     "multisearch",
		command:  "ESEARCH IN (selected) RETURN (UPDATE) UNSEEN",
		removal:  `^\* ESEARCH \(TAG "C" MAILBOX INBOX UIDVALIDITY \d+\) UID REMOVEFROM \(0 1\)$`,
		addition: `^\* ESEARCH \(TAG "C" MAILBOX INBOX UIDVALIDITY \d+\) UID ADDTO \(0 1\)$`,
	},
}

const seenFetch = `^\* 1 FETCH \(FLAGS \([^)]*\\Seen`

// contextWantOrder checks that lines contain want in that order, and that
// every ESEARCH update sent is one of them.
func contextWantOrder(t *testing.T, lines []string, want ...string) {
	t.Helper()
	at, updates := 0, 0
	for _, re := range want {
		if strings.Contains(re, "ESEARCH") {
			updates++
		}
		x := regexp.MustCompile(re)
		for at < len(lines) && !x.MatchString(lines[at]) {
			at++
		}
		if at == len(lines) {
			t.Fatalf("want in order %q: %q", want, lines)
		}
		at++
	}
	sent := 0
	for _, line := range lines {
		if strings.HasPrefix(line, "* ESEARCH (TAG ") {
			sent++
		}
	}
	if sent != updates {
		t.Fatalf("want only the context updates %q: %q", want, lines)
	}
}

// A flag change NOTIFY suppresses must not reach the client through a context
// either. NOTIFY SET releases it: the FETCH first, then the context update.
func TestNotifySuppressedFlagChangeHoldsContextUpdate(t *testing.T) {
	policies := []struct{ name, command string }{
		{"none", "NOTIFY NONE"},
		{"selected-none", "NOTIFY SET (selected NONE)"},
		{"no-flagchange", "NOTIFY SET (selected (MessageNew MessageExpunge))"},
	}
	for _, policy := range policies {
		for _, registration := range contextRegistrations {
			t.Run(policy.name+"/"+registration.name, func(t *testing.T) {
				obs, other := r4SeedSelected(t, 2)
				sweepWant(t, obs.cmd("C", registration.command), "OK")
				sweepWant(t, obs.cmd("N", policy.command), "OK")
				sweepWant(t, other.cmd("F", `STORE 1 +FLAGS (\Seen)`), "OK")
				got := append(obs.quiet(300*time.Millisecond), obs.cmd("Q", "NOOP")...)
				if r4Has(got, `ESEARCH|FETCH`) {
					t.Fatalf("suppressed flag change reached the wire: %q", got)
				}
				got = obs.cmd("R", "NOTIFY SET (selected (MessageNew MessageExpunge FlagChange))")
				sweepWant(t, got, "OK")
				if registration.name == "sort" {
					contextWantOrder(t, got, seenFetch, registration.removal, `^\* ESEARCH \(TAG "C"\) ADDTO \(1 2\)$`)
				} else {
					contextWantOrder(t, got, seenFetch, registration.removal)
				}
			})
		}
	}
}

// With FlagChange registered, a flag change's FETCH precedes the context
// update it causes, removal and addition alike (RFC 5465 section 5.1).
func TestNotifyFlagChangeOrdersContextUpdateAfterFetch(t *testing.T) {
	for _, registration := range contextRegistrations {
		t.Run(registration.name, func(t *testing.T) {
			obs, other := r4SeedSelected(t, 2)
			sweepWant(t, obs.cmd("C", registration.command), "OK")
			sweepWant(t, obs.cmd("N", "NOTIFY SET (selected (MessageNew MessageExpunge FlagChange))"), "OK")
			sweepWant(t, other.cmd("F", `STORE 1 +FLAGS (\Seen)`), "OK")
			got := append(obs.quiet(300*time.Millisecond), obs.cmd("Q", "NOOP")...)
			if registration.name == "sort" {
				contextWantOrder(t, got, seenFetch, registration.removal, `^\* ESEARCH \(TAG "C"\) ADDTO \(1 2\)$`)
			} else {
				contextWantOrder(t, got, seenFetch, registration.removal)
			}
			sweepWant(t, other.cmd("F", `STORE 1 -FLAGS (\Seen)`), "OK")
			got = append(obs.quiet(300*time.Millisecond), obs.cmd("Q", "NOOP")...)
			if registration.name == "sort" {
				contextWantOrder(t, got, `^\* 1 FETCH \(FLAGS`, `^\* ESEARCH \(TAG "C"\) REMOVEFROM \(1 2\)$`, registration.addition)
			} else {
				contextWantOrder(t, got, `^\* 1 FETCH \(FLAGS`, registration.addition)
			}
		})
	}
}

// The client's own STORE is a command result, which NOTIFY never filters. Its
// context update is sent under suppression too, after the STORE's FETCH, and
// with .SILENT as well, where no FETCH reports the change.
func TestNotifySuppressionKeepsOwnStoreContextUpdate(t *testing.T) {
	for _, silent := range []string{"", ".SILENT"} {
		for _, registration := range contextRegistrations {
			t.Run(registration.name+silent, func(t *testing.T) {
				obs, _ := r4SeedSelected(t, 2)
				sweepWant(t, obs.cmd("C", registration.command), "OK")
				sweepWant(t, obs.cmd("N", "NOTIFY NONE"), "OK")
				got := append(obs.cmd("F", `STORE 1 +FLAGS`+silent+` (\Seen)`), obs.cmd("Q", "NOOP")...)
				want := []string{registration.removal}
				if silent == "" {
					want = append([]string{seenFetch}, want...)
				}
				if registration.name == "sort" {
					want = append(want, `^\* ESEARCH \(TAG "C"\) ADDTO \(1 2\)$`)
				}
				contextWantOrder(t, got, want...)
			})
		}
	}
}

// contextGate holds the observer's next context refresh between the drain
// that started it and its backend call, so the test can commit another
// session's change in that window. The refresh then sees backend state ahead
// of what the session has applied.
type contextGate struct {
	*memory.Backend
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

type contextGateSession struct {
	imapserver.Session
	gate *contextGate
}

type contextGateMailbox struct {
	imapserver.SelectedMailbox
	gate *contextGate
}

func newContextGate(store *memory.Backend) *contextGate {
	return &contextGate{Backend: store, entered: make(chan struct{}, 1), release: make(chan struct{})}
}

func (g *contextGate) Authenticate(ctx context.Context, info *imapserver.ConnInfo, credentials *imapserver.Credentials, options *imapserver.AuthenticateOptions) (imapserver.Session, error) {
	session, err := g.Backend.Authenticate(ctx, info, credentials, options)
	if err != nil {
		return nil, err
	}
	return &contextGateSession{session, g}, nil
}

func (s *contextGateSession) Select(ctx context.Context, name string, updater *imapserver.Updater, options *imapserver.SelectOptions) (*imapserver.SelectResult, error) {
	result, err := s.Session.Select(ctx, name, updater, options)
	if err == nil {
		result.Mailbox = &contextGateMailbox{result.Mailbox, s.gate}
	}
	return result, err
}

func (s *contextGateSession) Notify(ctx context.Context, updater *imapserver.SessionUpdater, config *imapserver.NotifyConfig, options *imapserver.NotifyOptions) error {
	return s.Session.(imapserver.NotifySession).Notify(ctx, updater, config, options)
}

func (m *contextGateMailbox) wait() {
	if m.gate.armed.CompareAndSwap(true, false) {
		m.gate.entered <- struct{}{}
		<-m.gate.release
	}
}

func (m *contextGateMailbox) Search(ctx context.Context, query *imapserver.SearchQuery, options *imapserver.SearchOptions) (*imapserver.SearchResult, error) {
	m.wait()
	return m.SelectedMailbox.Search(ctx, query, options)
}

func (m *contextGateMailbox) Sort(ctx context.Context, query *imapserver.SearchQuery, keys []imap.SortKeySpec, options *imapserver.SortOptions) ([]imap.UID, error) {
	m.wait()
	return m.SelectedMailbox.(imapserver.SortMailbox).Sort(ctx, query, keys, options)
}

// A refresh whose backend call already sees an expunge the session has not
// applied must not report it. Under NOTIFY NONE that would announce a
// suppressed removal of a message the client still sees; without NOTIFY the
// REMOVEFROM still travels with its EXPUNGE, immediately before it.
func TestContextRefreshWaitsForQueuedChanges(t *testing.T) {
	registrations := []struct{ name, command, removal string }{
		{"search", "SEARCH RETURN (UPDATE) ALL", `^\* ESEARCH \(TAG "C"\) REMOVEFROM \(0 1\)$`},
		{"sort", "SORT RETURN (UPDATE) (ARRIVAL) US-ASCII ALL", `^\* ESEARCH \(TAG "C"\) REMOVEFROM \(1 1\)$`},
	}
	for _, notify := range []string{"", "NOTIFY NONE"} {
		for _, registration := range registrations {
			name := registration.name + "/plain"
			if notify != "" {
				name = registration.name + "/notify-none"
			}
			t.Run(name, func(t *testing.T) {
				store := r4Memory()
				gate := newContextGate(store)
				obs, other := r4Open(t, gate, nil), r4Open(t, store, nil)
				for range 2 {
					r4Append(other, "INBOX", "", "m")
				}
				sweepWant(t, obs.cmd("S", "SELECT INBOX"), "OK")
				sweepWant(t, other.cmd("S", "SELECT INBOX"), "OK")
				sweepWant(t, obs.cmd("C", registration.command), "OK")
				if notify != "" {
					sweepWant(t, obs.cmd("N", notify), "OK")
				}
				gate.armed.Store(true)
				sweepWant(t, other.cmd("D", `STORE 1 +FLAGS.SILENT (\Deleted)`), "OK")
				obs.write("Q NOOP\r\n")
				select {
				case <-gate.entered:
				case <-time.After(5 * time.Second):
					t.Fatal("the refresh never reached the backend")
				}
				sweepWant(t, other.cmd("X", "EXPUNGE"), "OK")
				close(gate.release)
				got := append(obs.until("Q"), obs.cmd("Z", "NOOP")...)
				if notify == "" {
					r, e := r4Index(got, registration.removal), r4Index(got, `^\* 1 EXPUNGE$`)
					if r < 0 || e != r+1 {
						t.Fatalf("want the REMOVEFROM immediately before its EXPUNGE: %q", got)
					}
					return
				}
				if r4Has(got, `REMOVEFROM|EXPUNGE`) {
					t.Fatalf("suppressed expunge reached the wire: %q", got)
				}
				if view := obs.cmd("V", "FETCH 1:* (UID)"); !r4Has(view, `^\* 2 FETCH \(UID 2\)$`) {
					t.Fatalf("message 1 left the client view: %q", view)
				}
				got = obs.cmd("R", "NOTIFY SET (selected (MessageNew MessageExpunge))")
				contextWantOrder(t, got, registration.removal, `^\* 1 EXPUNGE$`)
			})
		}
	}
}

// Under SELECTED-DELAYED a flag change is reported at once, with its context
// update after the FETCH, while an expunge waits for a command that permits
// it. So does the REMOVEFROM for the expunged match, which then precedes the
// EXPUNGE (RFC 5465 section 6.1.2, RFC 5267 section 4.3.4).
func TestNotifySelectedDelayedContextUpdates(t *testing.T) {
	obs, other := r4SeedSelected(t, 2)
	sweepWant(t, obs.cmd("A", "SEARCH RETURN (UPDATE) ALL"), "OK")
	sweepWant(t, obs.cmd("U", "SEARCH RETURN (UPDATE) UNDELETED"), "OK")
	sweepWant(t, obs.cmd("S", "SORT RETURN (UPDATE) (ARRIVAL) US-ASCII ALL"), "OK")
	sweepWant(t, obs.cmd("N", "NOTIFY SET (selected-delayed (MessageNew MessageExpunge FlagChange))"), "OK")
	sweepWant(t, other.cmd("D", `STORE 1 +FLAGS (\Deleted)`), "OK")
	got := append(obs.quiet(300*time.Millisecond), obs.cmd("Q", "NOOP")...)
	contextWantOrder(t, got, `^\* 1 FETCH \(FLAGS \([^)]*\\Deleted`, `^\* ESEARCH \(TAG "U"\) REMOVEFROM \(0 1\)$`)
	sweepWant(t, other.cmd("X", "EXPUNGE"), "OK")
	if got := obs.quiet(300 * time.Millisecond); r4Has(got, `EXPUNGE|ESEARCH`) {
		t.Fatalf("a delayed expunge was reported between commands: %q", got)
	}
	got = obs.cmd("Q", "NOOP")
	contextWantOrder(t, got,
		`^\* ESEARCH \(TAG "A"\) REMOVEFROM \(0 1\)$`,
		`^\* ESEARCH \(TAG "S"\) REMOVEFROM \(1 1\)$`,
		`^\* 1 EXPUNGE$`)
}

// NOTIFY SET releases a suppressed expunge and a suppressed flag change in one
// drain. The expunged match leaves before the EXPUNGE, in the numbering the
// client held; the flag change's update follows its FETCH, in the numbering
// after the EXPUNGE.
func TestNotifyReleaseOrdersMixedContextUpdates(t *testing.T) {
	registrations := []struct {
		name, command string
		want          []string
	}{
		{"search", "SEARCH RETURN (UPDATE) UNSEEN", []string{
			`^\* ESEARCH \(TAG "C"\) REMOVEFROM \(0 1\)$`,
			`^\* 1 EXPUNGE$`,
			seenFetch,
			`^\* ESEARCH \(TAG "C"\) REMOVEFROM \(0 1\)$`,
		}},
		{"sort", "SORT RETURN (UPDATE) (ARRIVAL) US-ASCII UNSEEN", []string{
			`^\* ESEARCH \(TAG "C"\) REMOVEFROM \(1 1\)$`,
			`^\* 1 EXPUNGE$`,
			seenFetch,
			`^\* ESEARCH \(TAG "C"\) REMOVEFROM \(1 1:2\)$`,
			`^\* ESEARCH \(TAG "C"\) ADDTO \(1 2\)$`,
		}},
		{"multisearch", "ESEARCH IN (selected) RETURN (UPDATE) UNSEEN", []string{
			`^\* ESEARCH \(TAG "C" MAILBOX INBOX UIDVALIDITY \d+\) UID REMOVEFROM \(0 1\)$`,
			`^\* 1 EXPUNGE$`,
			seenFetch,
			`^\* ESEARCH \(TAG "C" MAILBOX INBOX UIDVALIDITY \d+\) UID REMOVEFROM \(0 2\)$`,
		}},
	}
	for _, registration := range registrations {
		t.Run(registration.name, func(t *testing.T) {
			obs, other := r4SeedSelected(t, 3)
			sweepWant(t, obs.cmd("C", registration.command), "OK")
			sweepWant(t, obs.cmd("N", "NOTIFY NONE"), "OK")
			sweepWant(t, other.cmd("D", `STORE 1 +FLAGS.SILENT (\Deleted)`), "OK")
			sweepWant(t, other.cmd("X", "EXPUNGE"), "OK")
			sweepWant(t, other.cmd("F", `UID STORE 2 +FLAGS.SILENT (\Seen)`), "OK")
			got := append(obs.quiet(300*time.Millisecond), obs.cmd("Q", "NOOP")...)
			if r4Has(got, `ESEARCH|FETCH|EXPUNGE`) {
				t.Fatalf("suppressed changes reached the wire: %q", got)
			}
			got = obs.cmd("R", "NOTIFY SET (selected (MessageNew MessageExpunge FlagChange))")
			contextWantOrder(t, got, registration.want...)
		})
	}
}
