package imapserver_test

import (
	"context"
	"regexp"
	"slices"
	"strconv"
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
		// A sorted context names the position, here the first.
		name:     "sort",
		command:  "SORT RETURN (UPDATE) (ARRIVAL) US-ASCII UNSEEN",
		removal:  `^\* ESEARCH \(TAG "C"\) REMOVEFROM \(1 1\)$`,
		addition: `^\* ESEARCH \(TAG "C"\) ADDTO \(1 1\)$`,
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
				contextWantOrder(t, got, seenFetch, registration.removal)
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
			contextWantOrder(t, got, seenFetch, registration.removal)
			sweepWant(t, other.cmd("F", `STORE 1 -FLAGS (\Seen)`), "OK")
			got = append(obs.quiet(300*time.Millisecond), obs.cmd("Q", "NOOP")...)
			contextWantOrder(t, got, `^\* 1 FETCH \(FLAGS`, registration.addition)
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
				contextWantOrder(t, got, want...)
			})
		}
	}
}

// The client's own APPEND is announced under NOTIFY NONE, and so is its
// context update, after the EXISTS.
func TestNotifySuppressionKeepsOwnAppendContextUpdate(t *testing.T) {
	additions := map[string]string{
		"search":      `^\* ESEARCH \(TAG "C"\) ADDTO \(0 3\)$`,
		"sort":        `^\* ESEARCH \(TAG "C"\) ADDTO \(3 3\)$`,
		"multisearch": `^\* ESEARCH \(TAG "C" MAILBOX INBOX UIDVALIDITY \d+\) UID ADDTO \(0 3\)$`,
	}
	for _, registration := range contextRegistrations {
		t.Run(registration.name, func(t *testing.T) {
			obs, _ := r4SeedSelected(t, 2)
			sweepWant(t, obs.cmd("C", registration.command), "OK")
			sweepWant(t, obs.cmd("N", "NOTIFY NONE"), "OK")
			got := append(r4Append(obs, "INBOX", "", "own"), obs.cmd("Q", "NOOP")...)
			contextWantOrder(t, got, `^\* 3 EXISTS$`, additions[registration.name])
		})
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
			`^\* ESEARCH \(TAG "C"\) REMOVEFROM \(1 1\)$`,
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

// A sorted context cannot order a new match against a message the backend has
// removed while the client still sees it, here because NOTIFY NONE withholds
// the EXPUNGE. A match that would enter next to that message waits for it to
// leave (RFC 5267 section 4.3.2 keeps the requested order); the context's
// other updates do not wait.
func TestNotifySortedContextWaitsBesideWithheldRemoval(t *testing.T) {
	obs, other := r4SeedSelected(t, 3)
	sweepWant(t, obs.cmd("C", "SORT RETURN (UPDATE) (ARRIVAL) US-ASCII UNSEEN"), "OK")
	sweepWant(t, obs.cmd("N", "NOTIFY NONE"), "OK")
	sweepWant(t, other.cmd("D", `STORE 3 +FLAGS.SILENT (\Deleted)`), "OK")
	sweepWant(t, other.cmd("X", "EXPUNGE"), "OK")
	got := append(r4Append(obs, "INBOX", "", "new"), obs.cmd("Q", "NOOP")...)
	if !r4Has(got, `^\* 4 EXISTS$`) || r4Has(got, "ESEARCH") {
		t.Fatalf("want the own APPEND's EXISTS and no update beside the withheld removal: %q", got)
	}
	got = append(obs.cmd("F", `STORE 1 +FLAGS (\Seen)`), obs.cmd("Q", "NOOP")...)
	contextWantOrder(t, got, seenFetch, `^\* ESEARCH \(TAG "C"\) REMOVEFROM \(1 1\)$`)
	got = obs.cmd("R", "NOTIFY SET (selected (MessageNew MessageExpunge FlagChange))")
	contextWantOrder(t, got,
		`^\* ESEARCH \(TAG "C"\) REMOVEFROM \(2 3\)$`,
		`^\* 3 EXPUNGE$`,
		`^\* ESEARCH \(TAG "C"\) ADDTO \(2 3\)$`)
}

// comparatorContext registers C over UIDs 1 "a", 2 "B" and 3 "c", which are
// unseen, and 4 "0", which is seen. The context is 1 2 3 under the default
// i;unicode-casemap and 2 1 3 under i;octet; "0" sorts first under both.
func comparatorContext(t *testing.T) (obs, other *r4Wire) {
	t.Helper()
	store := r4Memory()
	obs, other = r4Open(t, store, nil), r4Open(t, store, nil)
	r4Append(other, "INBOX", "", "a")
	r4Append(other, "INBOX", "", "B")
	r4Append(other, "INBOX", "", "c")
	r4Append(other, "INBOX", `\Seen`, "0")
	sweepWant(t, obs.cmd("S", "SELECT INBOX"), "OK")
	sweepWant(t, other.cmd("S", "SELECT INBOX"), "OK")
	sweepWant(t, obs.cmd("C", "UID SORT RETURN (UPDATE) (SUBJECT) UTF-8 UNSEEN"), "OK")
	return obs, other
}

// While NOTIFY NONE withholds UID 3's EXPUNGE, the client's own STORE makes
// UID 4 match. It sorts first, outside UID 3's neighbours, so it enters at
// once. After a COMPARATOR change that reorders the context, the backend's
// order no longer places UID 3 by its neighbours: nothing enters and the old
// order stays until NOTIFY SET reports the EXPUNGE, and the reorder then
// replaces the list from position one.
func TestSortedContextComparatorChangeBesideWithheldRemoval(t *testing.T) {
	for _, comparator := range []string{"", "i;octet"} {
		t.Run("comparator="+comparator, func(t *testing.T) {
			obs, other := comparatorContext(t)
			sweepWant(t, obs.cmd("N", "NOTIFY NONE"), "OK")
			sweepWant(t, other.cmd("D", `UID STORE 3 +FLAGS.SILENT (\Deleted)`), "OK")
			sweepWant(t, other.cmd("X", "EXPUNGE"), "OK")
			if got := append(obs.quiet(300*time.Millisecond), obs.cmd("Q", "NOOP")...); r4Has(got, "ESEARCH|EXPUNGE") {
				t.Fatalf("suppressed expunge reached the wire: %q", got)
			}
			if comparator != "" {
				sweepWant(t, obs.cmd("CO", "COMPARATOR "+comparator), "OK")
			}
			got := append(obs.cmd("O", `UID STORE 4 -FLAGS (\Seen)`), obs.cmd("Q", "NOOP")...)
			if comparator == "" {
				contextWantOrder(t, got, `^\* \d+ FETCH`, `^\* ESEARCH \(TAG "C"\) UID ADDTO \(1 4\)$`)
				return
			}
			contextWantOrder(t, got, `^\* \d+ FETCH`)
			got = obs.cmd("R", "NOTIFY SET (selected (MessageNew MessageExpunge FlagChange))")
			contextWantOrder(t, got,
				`^\* ESEARCH \(TAG "C"\) UID REMOVEFROM \(3 3\)$`,
				`^\* 3 EXPUNGE$`,
				`^\* ESEARCH \(TAG "C"\) UID REMOVEFROM \(1 1:2\)$`,
				`^\* ESEARCH \(TAG "C"\) UID ADDTO \(1 4,2,1\)$`)
		})
	}
}

// Without a withheld removal, a reorder after a COMPARATOR change is sent at
// the next refresh, as a replacement of the list from position one.
func TestSortedContextComparatorReorder(t *testing.T) {
	obs, other := comparatorContext(t)
	sweepWant(t, obs.cmd("CO", "COMPARATOR i;octet"), "OK")
	sweepWant(t, other.cmd("F", `UID STORE 4 -FLAGS.SILENT (\Seen)`), "OK")
	got := append(obs.quiet(300*time.Millisecond), obs.cmd("Q", "NOOP")...)
	contextWantOrder(t, got,
		`^\* ESEARCH \(TAG "C"\) UID REMOVEFROM \(1 1:3\)$`,
		`^\* ESEARCH \(TAG "C"\) UID ADDTO \(1 4,2,1,3\)$`)
}

// UIDs 1 "a", 2 "b" and 3 "C" are unseen and 4 "D" is seen; UID 3's EXPUNGE
// is withheld when the client's own STORE makes UID 4 match. "D" follows "C"
// under both comparators, but only the default order puts it between UID 3's
// neighbours; i;octet sorts "C" and "D" before "a" and "b". A COMPARATOR
// command leaves UID 3's place in the context saying nothing about the new
// order, so UID 4 waits for the EXPUNGE there too instead of being placed
// on the wrong side of UID 3.
func TestSortedContextComparatorCommandHoldsMatches(t *testing.T) {
	for _, comparator := range []string{"", "i;octet"} {
		t.Run("comparator="+comparator, func(t *testing.T) {
			store := r4Memory()
			obs, other := r4Open(t, store, nil), r4Open(t, store, nil)
			for _, subject := range []string{"a", "b", "C"} {
				r4Append(other, "INBOX", "", subject)
			}
			r4Append(other, "INBOX", `\Seen`, "D")
			sweepWant(t, obs.cmd("S", "SELECT INBOX"), "OK")
			sweepWant(t, other.cmd("S", "SELECT INBOX"), "OK")
			sweepWant(t, obs.cmd("C", "UID SORT RETURN (UPDATE) (SUBJECT) UTF-8 UNSEEN"), "OK")
			sweepWant(t, obs.cmd("N", "NOTIFY NONE"), "OK")
			sweepWant(t, other.cmd("D", `UID STORE 3 +FLAGS.SILENT (\Deleted)`), "OK")
			sweepWant(t, other.cmd("X", "EXPUNGE"), "OK")
			if got := append(obs.quiet(300*time.Millisecond), obs.cmd("Q", "NOOP")...); r4Has(got, "ESEARCH|EXPUNGE") {
				t.Fatalf("suppressed expunge reached the wire: %q", got)
			}
			added := `^\* ESEARCH \(TAG "C"\) UID ADDTO \(3 4\)$`
			if comparator != "" {
				sweepWant(t, obs.cmd("CO", "COMPARATOR "+comparator), "OK")
				added = `^\* ESEARCH \(TAG "C"\) UID ADDTO \(1 4\)$`
			}
			got := append(obs.cmd("O", `UID STORE 4 -FLAGS (\Seen)`), obs.cmd("Q", "NOOP")...)
			contextWantOrder(t, got, `^\* \d+ FETCH`)
			got = obs.cmd("R", "NOTIFY SET (selected (MessageNew MessageExpunge FlagChange))")
			contextWantOrder(t, got,
				`^\* ESEARCH \(TAG "C"\) UID REMOVEFROM \(3 3\)$`,
				`^\* 3 EXPUNGE$`,
				added)
		})
	}
}

// sortedContextModel is a client's copy of a sorted context, kept by applying
// every REMOVEFROM, ADDTO and EXPUNGE in the order they arrive (RFC 5267
// sections 4.3.2 to 4.3.4).
type sortedContextModel struct {
	t    *testing.T
	uid  bool
	list []uint32
}

var (
	sortedContextUpdate = regexp.MustCompile(`^\* ESEARCH \(TAG "C"\)(?: UID)? (REMOVEFROM|ADDTO) \((.*)\)$`)
	sortedContextResult = regexp.MustCompile(`^\* SORT((?: \d+)*)$`)
	expungeResponse     = regexp.MustCompile(`^\* (\d+) EXPUNGE$`)
)

func contextNumbers(t *testing.T, set string) []uint32 {
	t.Helper()
	var numbers []uint32
	for _, item := range strings.Split(set, ",") {
		low, high, isRange := strings.Cut(item, ":")
		from, err := strconv.ParseUint(low, 10, 32)
		to := from
		if err == nil && isRange {
			to, err = strconv.ParseUint(high, 10, 32)
		}
		if err != nil {
			t.Fatalf("bad update set %q", set)
		}
		for n := from; ; {
			numbers = append(numbers, uint32(n))
			if n == to {
				break
			}
			if n < to {
				n++
			} else {
				n--
			}
		}
	}
	return numbers
}

func sortResult(lines []string) ([]uint32, bool) {
	for _, line := range lines {
		if g := sortedContextResult.FindStringSubmatch(line); g != nil {
			var numbers []uint32
			for _, field := range strings.Fields(g[1]) {
				n, _ := strconv.ParseUint(field, 10, 32)
				numbers = append(numbers, uint32(n))
			}
			return numbers, true
		}
	}
	return nil, false
}

func (m *sortedContextModel) apply(lines []string) {
	m.t.Helper()
	for _, line := range lines {
		if g := sortedContextUpdate.FindStringSubmatch(line); g != nil {
			fields := strings.Fields(g[2])
			if len(fields)%2 != 0 {
				m.t.Fatalf("odd update pairs: %s", line)
			}
			for i := 0; i < len(fields); i += 2 {
				position, _ := strconv.Atoi(fields[i])
				items := contextNumbers(m.t, fields[i+1])
				at := position - 1
				switch {
				case position < 1:
					m.t.Fatalf("sorted update without a position: %s", line)
				case g[1] == "ADDTO" && at <= len(m.list):
					m.list = slices.Insert(m.list, at, items...)
				case g[1] == "REMOVEFROM" && at+len(items) <= len(m.list) && slices.Equal(m.list[at:at+len(items)], items):
					m.list = slices.Delete(m.list, at, at+len(items))
				default:
					m.t.Fatalf("%s does not apply to %v", line, m.list)
				}
			}
			continue
		}
		if g := expungeResponse.FindStringSubmatch(line); g != nil && !m.uid {
			n64, _ := strconv.ParseUint(g[1], 10, 32)
			n := uint32(n64)
			if slices.Contains(m.list, n) {
				m.t.Fatalf("EXPUNGE %d arrived before its REMOVEFROM: %v", n, m.list)
			}
			for i := range m.list {
				if m.list[i] > n {
					m.list[i]--
				}
			}
		}
	}
}

// check compares the model with fresh SORT commands, which are drain points
// themselves. The model must hold the backend's messages in the backend's
// order at every step. A strict check also requires it to equal the result,
// apart from the messages with the UIDs in withheld.
func (m *sortedContextModel) check(w *r4Wire, sort, label string, strict bool, withheld ...uint32) {
	m.t.Helper()
	result := func(command string) []uint32 {
		lines := w.cmd("V", command)
		m.apply(lines)
		numbers, ok := sortResult(lines)
		if !ok {
			m.t.Fatalf("%s: no SORT result: %q", label, lines)
		}
		return numbers
	}
	order := result(sort + " ALL")
	want := result(sort + " UNSEEN")
	var except []uint32
	for _, uid := range withheld {
		if m.uid {
			except = append(except, uid)
			continue
		}
		lines := w.cmd("V", "SEARCH UID "+strconv.FormatUint(uint64(uid), 10))
		m.apply(lines)
		for _, line := range lines {
			if seq, ok := strings.CutPrefix(line, "* SEARCH "); ok {
				n, _ := strconv.ParseUint(seq, 10, 32)
				except = append(except, uint32(n))
			}
		}
	}
	at := make(map[uint32]int, len(order))
	for i, n := range order {
		at[n] = i
	}
	last := -1
	for _, n := range m.list {
		if i, held := at[n]; held {
			if i <= last {
				m.t.Fatalf("%s: context %v is out of the requested order %v", label, m.list, order)
			}
			last = i
		}
	}
	without := func(list []uint32) []uint32 {
		return slices.DeleteFunc(slices.Clone(list), func(n uint32) bool { return slices.Contains(except, n) })
	}
	if strict && !slices.Equal(without(m.list), without(want)) {
		m.t.Fatalf("%s: context %v, want %v apart from %v", label, m.list, want, except)
	}
}

// A sorted context under NOTIFY, followed by a client's model of it. Withheld
// flag changes and expunges, arrivals and the client's own changes mix. Every
// update must apply, keep the requested order and precede the EXPUNGE that
// renumbers it away, and after NOTIFY SET the context is the SORT result.
func TestNotifySortedContextModel(t *testing.T) {
	policies := []string{"NOTIFY SET (selected (MessageNew MessageExpunge))", "NOTIFY NONE"}
	for _, key := range []string{"ARRIVAL", "REVERSE ARRIVAL", "SUBJECT"} {
		for _, uid := range []bool{false, true} {
			for _, policy := range policies {
				t.Run(key+"/uid="+strconv.FormatBool(uid)+"/"+policy[7:11], func(t *testing.T) {
					store := r4Memory()
					obs, other := r4Open(t, store, nil), r4Open(t, store, nil)
					for _, subject := range []string{"e", "a", "d", "b", "c"} {
						r4Append(other, "INBOX", "", subject)
					}
					sweepWant(t, obs.cmd("S", "SELECT INBOX"), "OK")
					sweepWant(t, other.cmd("S", "SELECT INBOX"), "OK")
					sort := "SORT (" + key + ") US-ASCII"
					if uid {
						sort = "UID " + sort
					}
					m := &sortedContextModel{t: t, uid: uid}
					m.list, _ = sortResult(obs.cmd("P", sort+" UNSEEN"))
					sweepWant(t, obs.cmd("C", strings.Replace(sort, "SORT", "SORT RETURN (UPDATE)", 1)+" UNSEEN"), "OK")
					sweepWant(t, obs.cmd("N", policy), "OK")
					// With removals reported as they happen, only UID 2's
					// withheld flag change may keep the context from the
					// result. Under NONE, withheld removals can also hold
					// back an addition whose place next to them is unknown.
					step := func(label string, lines []string) {
						t.Helper()
						m.apply(append(lines, obs.quiet(200*time.Millisecond)...))
						m.apply(obs.cmd("Q", "NOOP"))
						m.check(obs, sort, label, policy != "NOTIFY NONE", 2)
					}
					sweepWant(t, other.cmd("F", `UID STORE 2 +FLAGS (\Seen)`), "OK")
					step("foreign flag", nil)
					r4Append(other, "INBOX", "", "ab")
					step("arrival", nil)
					step("own flag", obs.cmd("O", `UID STORE 4 +FLAGS (\Seen)`))
					sweepWant(t, other.cmd("D", `UID STORE 5 +FLAGS.SILENT (\Deleted)`), "OK")
					sweepWant(t, other.cmd("X", "EXPUNGE"), "OK")
					step("foreign expunge", nil)
					step("own append", r4Append(obs, "INBOX", "", "ac"))
					step("own unflag", obs.cmd("O", `UID STORE 4 -FLAGS (\Seen)`))
					m.apply(obs.cmd("R", "NOTIFY SET (selected (MessageNew MessageExpunge FlagChange))"))
					m.check(obs, sort, "released", true)
				})
			}
		}
	}
}
