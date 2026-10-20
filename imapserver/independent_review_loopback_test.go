package imapserver_test

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
	"github.com/kiliant/go-imap/imapserver/backendtest"
	"github.com/kiliant/go-imap/imapserver/memory"
)

// Regressions for the independent T33 review of 2631f2c.

func TestIndependentNotifyNoneWatch(t *testing.T) {
	for _, groups := range []string{
		"(mailboxes other NONE)",
		"(personal NONE)",
		"(mailboxes other NONE) (personal NONE)",
	} {
		t.Run(groups, func(t *testing.T) {
			b := reviewBackend(t, "memory")
			client, reader := reviewRawSession(t, b, nil)
			mutator, mutations := reviewRawSession(t, b, nil)
			reviewCommand(t, mutator, mutations, "C", "CREATE other", "OK")
			lines := reviewCommand(t, client, reader, "N", "NOTIFY SET STATUS "+groups, "OK")
			raw := "Subject: x\r\n\r\nbody"
			reviewCommand(t, mutator, mutations, "A", fmt.Sprintf("APPEND other {%d+}\r\n%s", len(raw), raw), "OK")
			lines = append(lines, reviewCommand(t, client, reader, "Q", "NOOP", "OK")...)
			if strings.Contains(strings.Join(lines, ""), "* STATUS ") {
				t.Fatalf("NONE watch delivered STATUS: %q", lines)
			}
		})
	}
}

type countingNotifyBackend struct {
	*memory.Backend
	calls atomic.Int32
}

type countingNotifySession struct {
	imapserver.Session
	imapserver.NotifySession
	calls *atomic.Int32
}

func (b *countingNotifyBackend) Authenticate(ctx context.Context, info *imapserver.ConnInfo, credentials *imapserver.Credentials, options *imapserver.AuthenticateOptions) (imapserver.Session, error) {
	s, err := b.Backend.Authenticate(ctx, info, credentials, options)
	if err != nil {
		return nil, err
	}
	return &countingNotifySession{Session: s, NotifySession: s.(imapserver.NotifySession), calls: &b.calls}, nil
}

func (s *countingNotifySession) Notify(ctx context.Context, updater *imapserver.SessionUpdater, config *imapserver.NotifyConfig, options *imapserver.NotifyOptions) error {
	s.calls.Add(1)
	return s.NotifySession.Notify(ctx, updater, config, options)
}

func TestIndependentNotifyWire(t *testing.T) {
	b := &countingNotifyBackend{Backend: memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}})}
	client, reader := reviewRawSession(t, b, nil)
	for _, event := range []string{"MailboxName", "SubscriptionChange", "AnnotationChange", "NoSuchEvent"} {
		lines := reviewCommand(t, client, reader, "N", "NOTIFY SET (personal ("+event+"))", "NO")
		if !strings.Contains(strings.Join(lines, ""), "[BADEVENT (MessageNew MessageExpunge FlagChange)]") {
			t.Errorf("%s refusal did not list all supported events: %q", event, lines)
		}
	}
	// A legal fetch-att list, including nested header fields, is a refusal
	// of unsupported behavior. It must never reach Notify on the backend.
	before := b.calls.Load()
	for _, group := range []string{
		"(selected (MessageNew (UID FLAGS) MessageExpunge))",
		"(selected-delayed (MessageNew (UID BODY.PEEK[HEADER.FIELDS (SUBJECT FROM)]) MessageExpunge))",
	} {
		lines := reviewCommand(t, client, reader, "N", "NOTIFY SET "+group, "NO")
		wire := strings.Join(lines, "")
		if !strings.Contains(wire, "[CANNOT]") || !strings.Contains(wire, "MessageNew fetch attributes are unsupported") {
			t.Errorf("fetch attribute refusal = %q", lines)
		}
	}
	if got := b.calls.Load(); got != before {
		t.Errorf("fetch attributes reached backend: Notify calls = %d, was %d", got, before)
	}
	reviewCommand(t, client, reader, "Q", "NOOP", "OK")
}

func TestIndependentNotifyMutations(t *testing.T) {
	raw := "Subject: x\r\n\r\nbody"
	appendOther := fmt.Sprintf("APPEND other {%d+}\r\n%s", len(raw), raw)
	replace := fmt.Sprintf("REPLACE 1 dest {%d+}\r\n%s", len(raw), raw)
	for _, tc := range []struct {
		name, events, command, mailbox, want string
	}{
		{"append", "MessageNew", appendOther, "other", "MESSAGES 2"},
		{"multiappend", "MessageNew", appendOther + fmt.Sprintf(" {%d+}\r\n%s", len(raw), raw), "other", "MESSAGES 3"},
		{"copy", "MessageNew", "COPY 1 dest", "dest", "MESSAGES 1"},
		{"move-add", "MessageNew", "MOVE 1 dest", "dest", "MESSAGES 1"},
		{"move-remove", "MessageExpunge", "MOVE 1 dest", "other", "MESSAGES 0"},
		{"replace-add", "MessageNew", replace, "dest", "MESSAGES 1"},
		{"replace-remove", "MessageExpunge", replace, "other", "MESSAGES 0"},
		{"store", "FlagChange", `STORE 1 +FLAGS (\Flagged)`, "other", "HIGHESTMODSEQ "},
		{"condstore", "FlagChange", `STORE 1 (UNCHANGEDSINCE 100) +FLAGS (\Flagged)`, "other", "HIGHESTMODSEQ "},
		{"fetch-seen", "FlagChange", "FETCH 1 BODY[]", "other", "UNSEEN 0"},
		{"expunge", "MessageExpunge", "EXPUNGE", "other", "MESSAGES 0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := reviewBackend(t, "memory")
			client, reader := reviewRawSession(t, b, nil)
			mutator, mutations := reviewRawSession(t, b, nil)
			for _, mailbox := range []string{"other", "dest"} {
				reviewCommand(t, mutator, mutations, "C", "CREATE "+mailbox, "OK")
			}
			reviewCommand(t, mutator, mutations, "A", appendOther, "OK")
			reviewCommand(t, mutator, mutations, "S", "SELECT other", "OK")
			if tc.name == "expunge" {
				reviewCommand(t, mutator, mutations, "D", `STORE 1 +FLAGS (\Deleted)`, "OK")
			}
			reviewCommand(t, client, reader, "E", "ENABLE CONDSTORE", "OK")
			// The first group must not shadow the event in the second group.
			groups := "(personal NONE) (mailboxes " + tc.mailbox + " (" + tc.events + "))"
			reviewCommand(t, client, reader, "N", "NOTIFY SET "+groups, "OK")
			reviewCommand(t, mutator, mutations, "M", tc.command, "OK")
			lines := reviewCommand(t, client, reader, "Q", "NOOP", "OK")
			var statuses []string
			for _, line := range lines {
				if strings.HasPrefix(line, "* STATUS ") {
					statuses = append(statuses, line)
				}
			}
			if len(statuses) != 1 || !strings.Contains(statuses[0], tc.mailbox) || !strings.Contains(statuses[0], tc.want) {
				t.Fatalf("mutation STATUS = %q, want one for %s with %s", statuses, tc.mailbox, tc.want)
			}
		})
	}
}

func TestIndependentNotifyEventUnion(t *testing.T) {
	b := reviewBackend(t, "memory")
	client, reader := reviewRawSession(t, b, nil)
	mutator, mutations := reviewRawSession(t, b, nil)
	reviewCommand(t, mutator, mutations, "C", "CREATE other", "OK")
	raw := "Subject: x\r\n\r\nbody"
	appendOther := fmt.Sprintf("APPEND other {%d+}\r\n%s", len(raw), raw)
	reviewCommand(t, mutator, mutations, "A", appendOther, "OK")
	reviewCommand(t, mutator, mutations, "S", "SELECT other", "OK")
	// A matching MessageNew group must not cause a FlagChange delivery.
	reviewCommand(t, client, reader, "N", "NOTIFY SET (personal (MessageNew))", "OK")
	reviewCommand(t, mutator, mutations, "M", `STORE 1 +FLAGS (\Flagged)`, "OK")
	lines := reviewCommand(t, client, reader, "Q", "NOOP", "OK")
	if strings.Contains(strings.Join(lines, ""), "* STATUS ") {
		t.Fatalf("MessageNew-only watch delivered FlagChange: %q", lines)
	}
	// NONE contributes nothing, and the union includes events from both of
	// the other groups even though all three groups match the same mailbox.
	reviewCommand(t, client, reader, "N", "NOTIFY SET (personal NONE) (personal (MessageNew)) (mailboxes other (FlagChange))", "OK")
	for _, command := range []string{`STORE 1 -FLAGS (\Flagged)`, appendOther} {
		reviewCommand(t, mutator, mutations, "M", command, "OK")
		lines := reviewCommand(t, client, reader, "Q", "NOOP", "OK")
		if !strings.Contains(strings.Join(lines, ""), "* STATUS ") {
			t.Fatalf("union lost an event from %s: %q", command, lines)
		}
	}
}

func TestIndependentNotifySubscribed(t *testing.T) {
	b := reviewBackend(t, "memory")
	client, reader := reviewRawSession(t, b, nil)
	mutator, mutations := reviewRawSession(t, b, nil)
	reviewCommand(t, mutator, mutations, "C", "CREATE other", "OK")
	raw := "Subject: x\r\n\r\nbody"
	reviewCommand(t, client, reader, "N", "NOTIFY SET (subscribed (MessageNew MessageExpunge))", "OK")
	for _, subscribed := range []bool{false, true, false} {
		verb := "UNSUBSCRIBE"
		if subscribed {
			verb = "SUBSCRIBE"
		}
		reviewCommand(t, mutator, mutations, "U", verb+" other", "OK")
		reviewCommand(t, mutator, mutations, "A", fmt.Sprintf("APPEND other {%d+}\r\n%s", len(raw), raw), "OK")
		lines := reviewCommand(t, client, reader, "Q", "NOOP", "OK")
		if got := strings.Contains(strings.Join(lines, ""), "* STATUS "); got != subscribed {
			t.Fatalf("APPEND with subscribed=%v: %q", subscribed, lines)
		}
		// Replacing the registration also checks the STATUS indicator.
		lines = reviewCommand(t, client, reader, "N", "NOTIFY SET STATUS (subscribed (MessageNew MessageExpunge))", "OK")
		if got := strings.Contains(strings.Join(lines, ""), "* STATUS "); got != subscribed {
			t.Fatalf("STATUS indicator with subscribed=%v: %q", subscribed, lines)
		}
	}
}

func TestIndependentNotifySelectedHasNoStatus(t *testing.T) {
	for _, groups := range []string{
		"(personal (MessageNew MessageExpunge))",
		"(mailboxes INBOX (MessageNew MessageExpunge))",
		"(selected (MessageNew MessageExpunge))",
		"(selected-delayed (MessageNew MessageExpunge))",
		"(selected NONE) (personal (MessageNew MessageExpunge))",
	} {
		t.Run(groups, func(t *testing.T) {
			b := reviewBackend(t, "memory")
			client, reader := reviewRawSession(t, b, nil)
			mutator, mutations := reviewRawSession(t, b, nil)
			reviewCommand(t, client, reader, "S", "SELECT INBOX", "OK")
			lines := reviewCommand(t, client, reader, "N", "NOTIFY SET STATUS "+groups, "OK")
			raw := "Subject: x\r\n\r\nbody"
			reviewCommand(t, mutator, mutations, "A", fmt.Sprintf("APPEND INBOX {%d+}\r\n%s", len(raw), raw), "OK")
			lines = append(lines, reviewCommand(t, client, reader, "Q", "NOOP", "OK")...)
			wire := strings.Join(lines, "")
			if strings.Contains(wire, "* STATUS ") {
				t.Fatalf("selected mailbox delivered STATUS: %q", lines)
			}
			if strings.HasPrefix(groups, "(selected (") || strings.HasPrefix(groups, "(selected-delayed (") {
				if !strings.Contains(wire, "* 1 EXISTS") {
					t.Fatalf("selected updater lost the addition: %q", lines)
				}
			}
		})
	}
}

type activationBackend struct {
	*memory.Backend
	fail    *bool
	lookups *[]activationLookup
}

// activationLookup records the context of one CONDSTORE activation lookup.
type activationLookup struct {
	left        time.Duration
	hasDeadline bool
}

// activationSession records each CONDSTORE activation lookup of the selected
// mailbox, a STATUS (HIGHESTMODSEQ) for INBOX, and fails it when fail is set.
type activationSession struct {
	imapserver.Session
	imapserver.MultiSearchSession
	fail    *bool
	lookups *[]activationLookup
}

func (b *activationBackend) Authenticate(ctx context.Context, info *imapserver.ConnInfo, credentials *imapserver.Credentials, options *imapserver.AuthenticateOptions) (imapserver.Session, error) {
	s, err := b.Backend.Authenticate(ctx, info, credentials, options)
	if err != nil {
		return nil, err
	}
	return &activationSession{Session: s, MultiSearchSession: s.(imapserver.MultiSearchSession), fail: b.fail, lookups: b.lookups}, nil
}

func (s *activationSession) Status(ctx context.Context, name string, options *imapserver.StatusOptions) (*imap.StatusData, error) {
	if name == "INBOX" && options != nil && len(options.Items) == 1 && options.Items[0] == imap.StatusItemHighestModSeq {
		deadline, ok := ctx.Deadline()
		*s.lookups = append(*s.lookups, activationLookup{left: time.Until(deadline), hasDeadline: ok})
		if *s.fail {
			return nil, fmt.Errorf("private database failure: password=do-not-disclose")
		}
	}
	return s.Session.Status(ctx, name, options)
}

// TestCondStoreActivationFailureStaysPrivate (A2, B1): a backend failure while
// a CONDSTORE enabling command looks up the selected mailbox's clock follows
// the backend error mapping, never revealing its text, and the lookup is bound
// by Limits.CommandTimeout. A failed activation does not record CONDSTORE as
// enabled, so the next enabling command still reports HIGHESTMODSEQ.
func TestCondStoreActivationFailureStaysPrivate(t *testing.T) {
	for _, command := range []string{
		"STATUS other (HIGHESTMODSEQ)",
		`LIST "" "other" RETURN (STATUS (HIGHESTMODSEQ))`,
		"ESEARCH IN (mailboxes other) MODSEQ 1",
		"FETCH 1 (MODSEQ)",
		"SEARCH MODSEQ 1",
		`STORE 1 (UNCHANGEDSINCE 100) +FLAGS (\Flagged)`,
	} {
		t.Run(strings.Fields(command)[0], func(t *testing.T) {
			fail := false
			var lookups []activationLookup
			b := &activationBackend{Backend: memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}}), fail: &fail, lookups: &lookups}
			client, reader := reviewRawSession(t, b, &imapserver.Options{AllowInsecureAuth: true, Limits: imapserver.Limits{CommandTimeout: time.Second}})
			reviewCommand(t, client, reader, "C", "CREATE other", "OK")
			raw := "Subject: x\r\n\r\nbody\r\n"
			reviewCommand(t, client, reader, "P", fmt.Sprintf("APPEND INBOX {%d+}\r\n%s", len(raw), raw), "OK")
			reviewCommand(t, client, reader, "S", "SELECT INBOX", "OK")
			fail, lookups = true, nil
			lines := reviewCommand(t, client, reader, "T", command, "NO")
			wire := strings.Join(lines, "")
			if strings.Contains(wire, "password") || strings.Contains(wire, "private") || !strings.Contains(wire, "[SERVERBUG]") {
				t.Errorf("activation failure = %q, want a generic NO [SERVERBUG]", wire)
			}
			if len(lookups) != 1 || !lookups[0].hasDeadline || lookups[0].left > time.Second {
				t.Errorf("activation lookups = %+v, want one bound by the 1s CommandTimeout", lookups)
			}
			fail = false
			lines = reviewCommand(t, client, reader, "T", command, "OK")
			if !strings.Contains(strings.Join(lines, ""), "* OK [HIGHESTMODSEQ ") {
				t.Errorf("retry after a failed activation did not report HIGHESTMODSEQ: %q", lines)
			}
		})
	}
}

// optionalResultsBackend forwards the mandatory contract of memory while
// returning the nil APPEND and COPY results a backend that does not witness
// UIDPLUS may return. It hides every optional interface and witness.
type optionalResultsBackend struct{ inner *memory.Backend }
type optionalResultsSession struct{ imapserver.Session }
type optionalResultsSelected struct{ imapserver.SelectedMailbox }

func (b *optionalResultsBackend) Authenticate(ctx context.Context, info *imapserver.ConnInfo, credentials *imapserver.Credentials, options *imapserver.AuthenticateOptions) (imapserver.Session, error) {
	s, err := b.inner.Authenticate(ctx, info, credentials, options)
	if err != nil {
		return nil, err
	}
	return &optionalResultsSession{s}, nil
}

func (s *optionalResultsSession) Append(ctx context.Context, name string, r io.Reader, options *imapserver.AppendOptions) (*imap.AppendData, error) {
	_, err := s.Session.Append(ctx, name, r, options)
	return nil, err
}

func (s *optionalResultsSession) Select(ctx context.Context, name string, u *imapserver.Updater, options *imapserver.SelectOptions) (*imapserver.SelectResult, error) {
	r, err := s.Session.Select(ctx, name, u, options)
	if err == nil {
		r.Mailbox = &optionalResultsSelected{r.Mailbox}
	}
	return r, err
}

func (s *optionalResultsSelected) Copy(ctx context.Context, uids imap.UIDSet, name string, options *imapserver.CopyOptions) (*imap.CopyData, error) {
	_, err := s.SelectedMailbox.Copy(ctx, uids, name, options)
	return nil, err
}

type sha1OnlyBackend struct{ *memory.Backend }

func (*sha1OnlyBackend) SupportsCapability(_ context.Context, name string, _ *imapserver.CapabilitySupportOptions) bool {
	return name == "AUTH=SCRAM-SHA-1"
}

func (b *sha1OnlyBackend) SCRAMCredentials(ctx context.Context, mechanism, username string, options *imapserver.SCRAMCredentialsOptions) (*imapserver.SCRAMStoredCredentials, error) {
	if mechanism != "SCRAM-SHA-1" {
		return nil, fmt.Errorf("unsupported mechanism %s", mechanism)
	}
	return b.Backend.SCRAMCredentials(ctx, mechanism, username, options)
}

type zeroClockBackend struct{ *memory.Backend }
type zeroClockSession struct{ imapserver.Session }
type zeroClockSelected struct {
	imapserver.SelectedMailbox
	condStore imapserver.CondStoreMailbox
}

// SupportsCapability withdraws the witnesses whose selected-mailbox interfaces
// zeroClockSelected does not forward.
func (b *zeroClockBackend) SupportsCapability(ctx context.Context, name string, options *imapserver.CapabilitySupportOptions) bool {
	switch name {
	case "MOVE", "REPLACE", "QRESYNC", "SORT", "SORT=DISPLAY", "THREAD=ORDEREDSUBJECT", "THREAD=REFERENCES":
		return false
	}
	return b.Backend.SupportsCapability(ctx, name, options)
}

func (b *zeroClockBackend) Authenticate(ctx context.Context, info *imapserver.ConnInfo, credentials *imapserver.Credentials, options *imapserver.AuthenticateOptions) (imapserver.Session, error) {
	s, err := b.Backend.Authenticate(ctx, info, credentials, options)
	if err != nil {
		return nil, err
	}
	return &zeroClockSession{s}, nil
}

func (s *zeroClockSession) Select(ctx context.Context, name string, u *imapserver.Updater, options *imapserver.SelectOptions) (*imapserver.SelectResult, error) {
	r, err := s.Session.Select(ctx, name, u, options)
	if err == nil {
		r.Mailbox = &zeroClockSelected{SelectedMailbox: r.Mailbox, condStore: r.Mailbox.(imapserver.CondStoreMailbox)}
	}
	return r, err
}

// StoreCondStore leaves the documented optional HighestModSeq at zero.
func (s *zeroClockSelected) StoreCondStore(ctx context.Context, w *imapserver.FetchWriter, uids imap.UIDSet, flags *imapserver.StoreFlags, options *imapserver.StoreOptions) (*imapserver.CondStoreResult, error) {
	result, err := s.condStore.StoreCondStore(ctx, w, uids, flags, options)
	if result != nil {
		result.HighestModSeq = 0
	}
	return result, err
}

// TestConformanceRespectsOptionalObligations (B2): backendtest checks only
// what the contract requires of a backend. APPEND and COPY results are
// optional without UIDPLUS, SCRAM is checked per witnessed mechanism family,
// and CondStoreResult.HighestModSeq may be zero.
func TestConformanceRespectsOptionalObligations(t *testing.T) {
	for name, backend := range map[string]func() imapserver.Backend{
		"nil-append-copy": func() imapserver.Backend {
			return &optionalResultsBackend{memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}})}
		},
		"scram-sha1-only": func() imapserver.Backend {
			return &sha1OnlyBackend{memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}})}
		},
		"condstore-zero-clock": func() imapserver.Backend {
			return &zeroClockBackend{memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}})}
		},
	} {
		t.Run(name, func(t *testing.T) {
			backendtest.Run(t, &backendtest.Harness{New: func(context.Context, *backendtest.InstanceOptions) (*backendtest.Instance, error) {
				return &backendtest.Instance{Backend: backend(), Credentials: imapserver.Credentials{Mechanism: "PLAIN", Username: "alice", Password: "secret"}}, nil
			}})
		})
	}
}

func memorySession(t *testing.T) imapserver.Session {
	t.Helper()
	b := memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}})
	s, err := b.Authenticate(t.Context(), &imapserver.ConnInfo{}, &imapserver.Credentials{Mechanism: "PLAIN", Username: "alice", Password: "secret"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background(), nil) })
	return s
}

// TestMemoryFetchFailureLeavesNoUnpublishedSeen (B4): a FETCH that fails on a
// later message stores no implicit \Seen change it does not also publish.
func TestMemoryFetchFailureLeavesNoUnpublishedSeen(t *testing.T) {
	s := memorySession(t)
	for _, raw := range []string{
		"Content-Type: multipart/mixed; boundary=x\r\n\r\n--x\r\nContent-Type: text/plain\r\n\r\none\r\n--x\r\nContent-Type: text/plain\r\n\r\ntwo\r\n--x--\r\n",
		"Subject: single\r\n\r\nsingle",
	} {
		if _, err := s.Append(t.Context(), "INBOX", strings.NewReader(raw), nil); err != nil {
			t.Fatal(err)
		}
	}
	pushes := 0
	r, err := s.Select(t.Context(), "INBOX", &imapserver.Updater{PushFunc: func(*imapserver.UpdateBatch) error { pushes++; return nil }}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Mailbox.Unselect(context.Background(), nil)
	discard := &imapserver.FetchWriter{WriteFunc: func(context.Context, *imap.FetchMessageData) error { return nil }}
	all := imap.UIDSetNum(r.Snapshot.UIDs...)
	if err := r.Mailbox.Fetch(t.Context(), discard, all, &imapserver.FetchOptions{Items: []imap.FetchItem{imap.FetchItemUID, &imap.FetchItemBodySection{Part: []int{2}}}}); err == nil {
		t.Fatal("FETCH BODY[2] of a single-part message succeeded")
	}
	seen := 0
	flags := &imapserver.FetchWriter{WriteFunc: func(_ context.Context, data *imap.FetchMessageData) error {
		for _, item := range data.Items["FLAGS"] {
			if imap.ContainsFlag([]imap.Flag(item.(imap.FetchDataFlags)), imap.FlagSeen) {
				seen++
			}
		}
		return nil
	}}
	if err := r.Mailbox.Fetch(t.Context(), flags, all, &imapserver.FetchOptions{Items: []imap.FetchItem{imap.FetchItemUID, imap.FetchItemFlags}}); err != nil {
		t.Fatal(err)
	}
	if seen != pushes {
		t.Errorf("failed FETCH stored %d \\Seen changes but published %d update batches", seen, pushes)
	}
}

// TestMemoryRenameHierarchy (B6): RENAME moves inferiors, refuses a conflict
// without renaming anything, and refuses a move into its own hierarchy.
func TestMemoryRenameHierarchy(t *testing.T) {
	s := memorySession(t)
	for _, name := range []string{"parent", "parent/child", "parent/child/deep", "taken", "busy/child"} {
		if err := s.Create(t.Context(), name, nil); err != nil {
			t.Fatal(err)
		}
	}
	names := func() []string {
		var listed []string
		w := &imapserver.ListWriter{WriteFunc: func(_ context.Context, d *imap.ListData) error {
			listed = append(listed, d.Mailbox)
			return nil
		}}
		if err := s.List(t.Context(), w, "", []string{"*"}, nil); err != nil {
			t.Fatal(err)
		}
		slices.Sort(listed)
		return listed
	}
	if err := s.Rename(t.Context(), "parent", "renamed", nil); err != nil {
		t.Fatal(err)
	}
	if got, want := names(), []string{"INBOX", "busy/child", "renamed", "renamed/child", "renamed/child/deep", "taken"}; !slices.Equal(got, want) {
		t.Fatalf("after RENAME parent renamed: %v, want %v", got, want)
	}
	// "busy" is free, but renamed/child would land on the existing
	// busy/child: the whole rename is refused and nothing moves.
	before := names()
	if err := s.Rename(t.Context(), "renamed", "busy", nil); err == nil {
		t.Fatal("RENAME onto an occupied inferior succeeded")
	}
	if got := names(); !slices.Equal(got, before) {
		t.Fatalf("refused RENAME changed the hierarchy: %v, was %v", got, before)
	}
	if err := s.Rename(t.Context(), "renamed", "taken", nil); err == nil {
		t.Fatal("RENAME onto an existing mailbox succeeded")
	}
	if err := s.Rename(t.Context(), "renamed", "renamed/child/inside", nil); err == nil {
		t.Fatal("RENAME into its own hierarchy succeeded")
	}
}

type countingCatenateBackend struct {
	*memory.Backend
	calls *int
}

// countingCatenateSession counts URL resolutions; it keeps memory's atomic
// MULTIAPPEND so a two-message APPEND is accepted.
type countingCatenateSession struct {
	imapserver.Session
	imapserver.MultiAppendSession
	calls *int
}

func (b *countingCatenateBackend) Authenticate(ctx context.Context, info *imapserver.ConnInfo, credentials *imapserver.Credentials, options *imapserver.AuthenticateOptions) (imapserver.Session, error) {
	s, err := b.Backend.Authenticate(ctx, info, credentials, options)
	if err != nil {
		return nil, err
	}
	return &countingCatenateSession{Session: s, MultiAppendSession: s.(imapserver.MultiAppendSession), calls: b.calls}, nil
}

func (s *countingCatenateSession) ResolveCatenateURL(context.Context, string, *imapserver.CatenateOptions) (io.ReadCloser, error) {
	*s.calls++
	return io.NopCloser(strings.NewReader("x")), nil
}

// TestCatenateNotResolvedAfterRefusal (B5): once an APPEND is refused, later
// CATENATE parts are drained without resolving their URLs.
func TestCatenateNotResolvedAfterRefusal(t *testing.T) {
	calls := 0
	b := &countingCatenateBackend{Backend: memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}}), calls: &calls}
	client, reader := reviewRawSession(t, b, &imapserver.Options{AllowInsecureAuth: true, Limits: imapserver.Limits{MaxLiteralBytes: 8}})
	lines := reviewCommand(t, client, reader, "A", `APPEND INBOX CATENATE (TEXT {8+}`+"\r\n12345678"+` URL "one") CATENATE (URL "two")`, "NO")
	if !strings.Contains(strings.Join(lines, ""), "[TOOBIG]") {
		t.Errorf("APPEND = %q, want NO [TOOBIG]", lines)
	}
	if calls != 1 {
		t.Errorf("resolver called %d times, want only the first URL before the refusal", calls)
	}
	// The connection stays usable: every literal was consumed.
	reviewCommand(t, client, reader, "N", "NOOP", "OK")
}
