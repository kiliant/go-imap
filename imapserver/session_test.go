package imapserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapsasl"
)

type stubSelectedMailbox struct{}

func (*stubSelectedMailbox) Status(context.Context, *StatusOptions) (*imap.MailboxStatus, error) {
	return &imap.MailboxStatus{}, nil
}
func (*stubSelectedMailbox) Fetch(context.Context, *FetchWriter, imap.UIDSet, *FetchOptions) error {
	return nil
}
func (*stubSelectedMailbox) Search(context.Context, *SearchQuery, *SearchOptions) (*SearchResult, error) {
	return &SearchResult{}, nil
}
func (*stubSelectedMailbox) Store(context.Context, *FetchWriter, imap.UIDSet, *StoreFlags, *StoreOptions) error {
	return nil
}
func (*stubSelectedMailbox) Copy(context.Context, imap.UIDSet, string, *CopyOptions) (*imap.CopyData, error) {
	return &imap.CopyData{}, nil
}
func (*stubSelectedMailbox) Expunge(context.Context, *ExpungeWriter, *imap.UIDSet, *ExpungeOptions) error {
	return nil
}
func (*stubSelectedMailbox) Unselect(context.Context) error { return nil }
func (*stubSelectedMailbox) Move(context.Context, imap.UIDSet, string, *MoveOptions) (*imap.CopyData, error) {
	return &imap.CopyData{}, nil
}

var _ SelectedMailbox = (*stubSelectedMailbox)(nil)
var _ MoveMailbox = (*stubSelectedMailbox)(nil)
var _ io.Reader = (*zeroReader)(nil)

type moveSupportBackend struct{ Backend }

func (moveSupportBackend) SupportsMove() bool { return true }

type moveSupportSession struct{ stubSession }

func (*moveSupportSession) SupportsMove() bool { return true }

// fullRev2Session witnesses everything RFC 9051 incorporates: atomic MOVE, the
// spoken tokens in rev2Incorporated, and NamespaceSession structurally. It is
// the minimum a backend must be before IMAP4REV2 may be advertised for it.
type fullRev2Session struct{ moveSupportSession }

func (*fullRev2Session) SupportsCapability(name string) bool {
	return slices.Contains(rev2Incorporated, name)
}

func (*fullRev2Session) Namespace(context.Context, *NamespaceOptions) (*imap.NamespaceData, error) {
	return &imap.NamespaceData{}, nil
}

type noMoveSupportSession struct{ stubSession }

func (*noMoveSupportSession) SupportsMove() bool { return false }

type zeroReader struct{}

func (*zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

func TestStateMachine(t *testing.T) {
	state := newSessionState(false)
	if !state.allows(stateMaskNotAuthenticated) || state.allows(stateMaskAuthenticated) {
		t.Fatal("initial state is not not-authenticated")
	}
	if state.authenticate(nil) {
		t.Fatal("nil backend session authenticated")
	}
	backendSession := &stubSession{}
	if !state.authenticate(backendSession) || !state.allows(stateMaskAuthenticated) {
		t.Fatal("authentication transition failed")
	}
	selected := &selectedState{mailbox: &stubSelectedMailbox{}}
	if !state.selectMailbox(selected) || !state.allows(stateMaskSelected) {
		t.Fatal("selection transition failed")
	}
	if got := state.unselect(); got != selected || !state.allows(stateMaskAuthenticated) {
		t.Fatal("unselect transition failed")
	}
}

func TestRev2SelectionRequiresAtomicMoveHandle(t *testing.T) {
	state := newSessionState(true)
	if !state.authenticate(&stubSession{}) || !state.enable("IMAP4REV2") {
		t.Fatal("failed to establish rev2 authenticated state")
	}
	withoutMove := &selectedState{mailbox: &selectedWithoutMove{SelectedMailbox: &stubSelectedMailbox{}}}
	if state.selectMailbox(withoutMove) || state.state != stateAuthenticated || state.selected != nil {
		t.Fatal("rev2 accepted a selected handle without atomic MOVE")
	}
	if !state.selectMailbox(&selectedState{mailbox: &stubSelectedMailbox{}}) {
		t.Fatal("rev2 rejected selected handle with atomic MOVE")
	}
}

func TestSASLServerExtractsCredentials(t *testing.T) {
	plain, err := newSASLServer("plain")
	if err != nil {
		t.Fatal(err)
	}
	credentials, challenge, err := plain.next([]byte("operator\x00alice\x00secret"))
	if err != nil || challenge != nil || credentials == nil || credentials.Mechanism != "PLAIN" ||
		credentials.AuthzID != "operator" || credentials.Username != "alice" || credentials.Password != "secret" {
		t.Fatalf("PLAIN = %#v, %q, %v", credentials, challenge, err)
	}

	login, err := newSASLServer("LOGIN")
	if err != nil {
		t.Fatal(err)
	}
	if got := string(login.initialChallenge()); got != "Username:" {
		t.Fatalf("LOGIN initial challenge = %q", got)
	}
	credentials, challenge, err = login.next([]byte("alice"))
	if err != nil || credentials != nil || string(challenge) != "Password:" {
		t.Fatalf("LOGIN username step = %#v, %q, %v", credentials, challenge, err)
	}
	credentials, challenge, err = login.next([]byte("secret"))
	if err != nil || challenge != nil || credentials == nil || credentials.Username != "alice" || credentials.Password != "secret" {
		t.Fatalf("LOGIN password step = %#v, %q, %v", credentials, challenge, err)
	}

	for _, test := range []struct {
		name      string
		mechanism *imapsasl.Mechanism
		username  string
		token     string
	}{
		{name: "XOAUTH2", mechanism: imapsasl.XOAUTH2("alice", "token-x"), username: "alice", token: "token-x"},
		{name: "OAUTHBEARER", mechanism: imapsasl.OAUTHBEARER("a,b=c", "token-o", "mail.example", "993"), username: "a,b=c", token: "token-o"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response, err := test.mechanism.Next(nil)
			if err != nil {
				t.Fatal(err)
			}
			server, err := newSASLServer(test.name)
			if err != nil {
				t.Fatal(err)
			}
			credentials, challenge, err := server.next(response)
			if err != nil || challenge != nil || credentials == nil || credentials.Username != test.username || credentials.Token != test.token {
				t.Fatalf("credentials = %#v, challenge %q, err %v", credentials, challenge, err)
			}
		})
	}
}

func TestSASLServerRejectsMalformedResponses(t *testing.T) {
	for _, test := range []struct {
		name     string
		response string
	}{
		{name: "PLAIN", response: "alice\x00secret"},
		{name: "PLAIN", response: "\x00\x00secret"},
		{name: "XOAUTH2", response: "user=alice\x01auth=Basic token\x01\x01"},
		{name: "OAUTHBEARER", response: "n,a=bad=XX,\x01auth=Bearer token\x01\x01"},
	} {
		t.Run(test.name+"/"+test.response, func(t *testing.T) {
			server, err := newSASLServer(test.name)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := server.next([]byte(test.response)); err == nil {
				t.Fatal("malformed response accepted")
			} else if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "token") {
				t.Fatalf("credential leaked through error: %v", err)
			}
		})
	}
	if _, _, err := decodeSASL("not-base64"); err == nil {
		t.Fatal("malformed base64 accepted")
	}
	if response, aborted, err := decodeSASL("*"); err != nil || !aborted || response != nil {
		t.Fatalf("SASL abort = %q, %v, %v", response, aborted, err)
	}
}

func TestFrameworkWriterLifetime(t *testing.T) {
	writes := 0
	writer := newListWriter(func(_ context.Context, data *imap.ListData) error {
		if data == nil {
			t.Fatal("nil LIST data")
		}
		writes++
		return nil
	})
	if err := writer.WriteList(context.Background(), &imap.ListData{}); err != nil {
		t.Fatal(err)
	}
	writer.core.close()
	if err := writer.WriteList(context.Background(), &imap.ListData{}); !errors.Is(err, ErrWriterClosed) {
		t.Fatalf("write after backend call = %v", err)
	}
	if writes != 1 {
		t.Fatalf("writes = %d, want 1", writes)
	}
	var zero FetchWriter
	if err := zero.WriteMessage(context.Background(), &imap.FetchMessageData{}); !errors.Is(err, ErrWriterClosed) {
		t.Fatalf("zero writer = %v", err)
	}
}

func TestWriterAndUpdaterAdapterCallbacks(t *testing.T) {
	ctx := context.Background()
	listCalled := false
	listWriter := &ListWriter{WriteFunc: func(got context.Context, data *imap.ListData) error {
		listCalled = got == ctx && data != nil
		return nil
	}}
	if err := listWriter.WriteList(ctx, &imap.ListData{}); err != nil || !listCalled {
		t.Fatalf("ListWriter adapter = %v, called %v", err, listCalled)
	}

	fetchCalled := false
	fetchWriter := &FetchWriter{WriteFunc: func(got context.Context, data *imap.FetchMessageData) error {
		fetchCalled = got == ctx && data != nil
		return nil
	}}
	if err := fetchWriter.WriteMessage(ctx, &imap.FetchMessageData{}); err != nil || !fetchCalled {
		t.Fatalf("FetchWriter adapter = %v, called %v", err, fetchCalled)
	}

	expungeCalled := false
	expungeWriter := &ExpungeWriter{WriteFunc: func(got context.Context, uid imap.UID) error {
		expungeCalled = got == ctx && uid == 7
		return nil
	}}
	if err := expungeWriter.WriteExpunge(ctx, 7); err != nil || !expungeCalled {
		t.Fatalf("ExpungeWriter adapter = %v, called %v", err, expungeCalled)
	}

	updateCalled := false
	updater := &Updater{PushFunc: func(batch *UpdateBatch) error {
		updateCalled = batch != nil && batch.After == "next"
		return nil
	}}
	if err := updater.Push(&UpdateBatch{After: "next"}); err != nil || !updateCalled {
		t.Fatalf("Updater adapter = %v, called %v", err, updateCalled)
	}
}

func TestCapabilitiesDerivedFromLiveState(t *testing.T) {
	server := New(nil, nil)
	state := newSessionState(false)
	capabilities := deriveCapabilities(&state, server)
	for _, want := range []string{"IMAP4REV1", "ENABLE", "ID", "LITERAL-", "LOGINDISABLED"} {
		if !slices.Contains(capabilities, want) {
			t.Errorf("pre-auth capabilities %v omit %s", capabilities, want)
		}
	}
	if slices.Contains(capabilities, "STARTTLS") {
		t.Fatal("STARTTLS advertised without TLS configuration")
	}

	server.backend = moveSupportBackend{}
	server.options.RequireTLS = true
	capabilities = deriveCapabilities(&state, server)
	if !slices.Contains(capabilities, "LOGINDISABLED") {
		t.Fatal("LOGINDISABLED not derived from RequireTLS")
	}
	state.tls = true
	capabilities = deriveCapabilities(&state, server)
	if slices.Contains(capabilities, "LOGINDISABLED") || slices.Contains(capabilities, "STARTTLS") {
		t.Fatalf("plaintext-only capabilities survived TLS: %v", capabilities)
	}
}

func TestRequireTLSOverridesInsecureAuthentication(t *testing.T) {
	server := New(moveSupportBackend{}, &Options{RequireTLS: true, AllowInsecureAuth: true})
	server.framework[frameworkAuth] = true
	state := newSessionState(false)
	capabilities := deriveCapabilities(&state, server)
	if slices.Contains(capabilities, "AUTH=PLAIN") || slices.Contains(capabilities, "AUTH=LOGIN") {
		t.Fatalf("cleartext password authentication advertised under RequireTLS: %v", capabilities)
	}
	state.tls = true
	capabilities = deriveCapabilities(&state, server)
	if !slices.Contains(capabilities, "AUTH=PLAIN") || !slices.Contains(capabilities, "AUTH=LOGIN") {
		t.Fatalf("password authentication omitted after TLS: %v", capabilities)
	}
}

func TestMoveAndRev2RequireAtomicMoveBackend(t *testing.T) {
	server := New(moveSupportBackend{}, nil)
	server.framework[frameworkMove] = true
	server.framework[frameworkRev2] = true
	state := newSessionState(true)
	if got := deriveCapabilities(&state, server); !slices.Contains(got, "IMAP4REV2") {
		t.Fatalf("pre-auth rev2 omitted with backend witness: %v", got)
	}
	state.state = stateAuthenticated
	state.session = &fullRev2Session{}
	if got := deriveCapabilities(&state, server); !slices.Contains(got, "MOVE") || !slices.Contains(got, "IMAP4REV2") {
		t.Fatalf("authenticated atomic capabilities omitted: %v", got)
	}
	state.state = stateSelected
	state.selected = &selectedState{mailbox: &selectedWithoutMove{SelectedMailbox: &stubSelectedMailbox{}}}
	if got := deriveCapabilities(&state, server); slices.Contains(got, "MOVE") || slices.Contains(got, "IMAP4REV2") {
		t.Fatalf("atomic capabilities advertised without MoveMailbox: %v", got)
	}
	state.selected.mailbox = &stubSelectedMailbox{}
	got := deriveCapabilities(&state, server)
	if !slices.Contains(got, "MOVE") || !slices.Contains(got, "IMAP4REV2") {
		t.Fatalf("atomic capabilities omitted with MoveMailbox: %v", got)
	}
}

// partialRev2Session witnesses everything except one token, which is how a
// backend written against an earlier revision of this framework looks: it
// implements what it was asked for and nothing that arrived later.
type partialRev2Session struct {
	moveSupportSession
	withhold string
}

func (s *partialRev2Session) SupportsCapability(name string) bool {
	return name != s.withhold && slices.Contains(rev2Incorporated, name)
}

func (s *partialRev2Session) Namespace(context.Context, *NamespaceOptions) (*imap.NamespaceData, error) {
	return &imap.NamespaceData{}, nil
}

// noNamespaceRev2Session speaks every token and does not implement
// NamespaceSession. NAMESPACE is witnessed structurally, so this — not a
// withheld token — is what a backend missing it actually looks like.
type noNamespaceRev2Session struct{ moveSupportSession }

func (*noNamespaceRev2Session) SupportsCapability(name string) bool {
	return slices.Contains(rev2Incorporated, name)
}

// noMoveRev2Session is complete but for atomic MOVE, which is witnessed by
// SupportsMove reporting false rather than by the method being absent.
type noMoveRev2Session struct{ fullRev2Session }

func (*noMoveRev2Session) SupportsMove() bool { return false }

// TestRev2RequiresEveryIncorporatedCapability is the gate on SERVER-DESIGN.md
// §1: IMAP4REV2 is a claim about the whole incorporated set, so withholding any
// single member of it must withdraw the umbrella.
//
// Before this test the umbrella was gated on atomic MOVE alone, so a backend
// witnessing MOVE and nothing else advertised rev2 and was then held to
// UID EXPUNGE, APPENDUID, COPYUID, NAMESPACE and an untagged LIST on SELECT it
// had never agreed to produce.
func TestRev2RequiresEveryIncorporatedCapability(t *testing.T) {
	server := New(moveSupportBackend{}, nil)
	server.framework[frameworkMove] = true
	server.framework[frameworkRev2] = true

	for _, withhold := range rev2Incorporated {
		t.Run(withhold, func(t *testing.T) {
			state := newSessionState(true)
			state.state = stateAuthenticated
			// Each capability has to be withheld the way its own witness reads
			// it. Dropping the NAMESPACE token from a session that still has the
			// method withholds nothing, because the method is the witness.
			switch withhold {
			case "MOVE":
				state.session = &noMoveRev2Session{}
			case "NAMESPACE":
				state.session = &noNamespaceRev2Session{}
			default:
				state.session = &partialRev2Session{withhold: withhold}
			}
			if got := deriveCapabilities(&state, server); slices.Contains(got, "IMAP4REV2") {
				t.Fatalf("IMAP4REV2 advertised without %s: %v", withhold, got)
			}
		})
	}

	// The T23-era backend the guardian probed with: atomic MOVE and nothing
	// else. It advertised IMAP4REV2 before this gate existed.
	state := newSessionState(true)
	state.state = stateAuthenticated
	state.session = &moveSupportSession{}
	if got := deriveCapabilities(&state, server); slices.Contains(got, "IMAP4REV2") {
		t.Fatalf("IMAP4REV2 advertised for a MOVE-only session: %v", got)
	}
}

// TestRev2IncorporatedNamesResolve stops a typo in rev2Incorporated widening the
// gate silently. capabilityWitness returns nil for an unknown name, which reads
// as "needs no backend support" — the failure direction that advertises more,
// not less, and the one no other test would notice.
func TestRev2IncorporatedNamesResolve(t *testing.T) {
	for _, name := range rev2Incorporated {
		if !slices.ContainsFunc(capabilityDescriptors, func(d capabilityDescriptor) bool { return d.Name == name }) {
			t.Errorf("rev2Incorporated names %q, which has no capability descriptor", name)
			continue
		}
		if capabilityWitness(name) == nil {
			t.Errorf("%s has no backend witness, so requiring it for IMAP4REV2 asserts nothing", name)
		}
	}
}

func TestSessionMoveWitnessOverridesServerWideSupport(t *testing.T) {
	server := New(moveSupportBackend{}, nil)
	server.framework[frameworkMove] = true
	server.framework[frameworkRev2] = true
	state := newSessionState(true)
	state.state = stateAuthenticated
	state.session = &noMoveSupportSession{}
	if got := deriveCapabilities(&state, server); slices.Contains(got, "MOVE") || slices.Contains(got, "IMAP4REV2") {
		t.Fatalf("session-specific refusal ignored: %v", got)
	}
}

type selectedWithoutMove struct{ SelectedMailbox }

func TestFeatureActivationIsRevisionAware(t *testing.T) {
	server := New(nil, nil)
	state := newSessionState(true)
	if activeFeatures(&state, server)[featureBinaryFetch] {
		t.Fatal("binary fetch active in plain rev1")
	}
	state.revision = revisionIMAP4rev2
	features := activeFeatures(&state, server)
	if !features[featureBinaryFetch] || !features[featureListMulti] {
		t.Fatalf("rev2 incorporated features inactive: %v", features)
	}
	if features[featureBinaryAppend] {
		t.Fatal("rev2 incorrectly activated binary APPEND")
	}
}

func TestSelectSnapshotValidation(t *testing.T) {
	valid := SelectSnapshot{
		UIDs:      []imap.UID{2, 4},
		Status:    imap.MailboxStatus{NumMessages: 2, UIDValidity: 1, UIDNext: 5},
		NumRecent: 1,
	}
	if err := validateSelectSnapshot(&valid, 2); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*SelectSnapshot){
		func(s *SelectSnapshot) { s.Status.NumMessages = 1 },
		func(s *SelectSnapshot) { s.Status.UIDValidity = 0 },
		func(s *SelectSnapshot) { s.UIDs = []imap.UID{2, 2} },
		func(s *SelectSnapshot) { s.Status.UIDNext = 0 },
		func(s *SelectSnapshot) { s.Status.UIDNext = 4 },
		func(s *SelectSnapshot) { s.NumRecent = 3 },
	} {
		broken := valid
		broken.UIDs = slices.Clone(valid.UIDs)
		mutate(&broken)
		if err := validateSelectSnapshot(&broken, 2); err == nil {
			t.Fatalf("invalid snapshot accepted: %#v", broken)
		}
	}
	if err := validateSelectSnapshot(&valid, 1); err == nil {
		t.Fatal("selected message limit was not enforced")
	}
}

type selectFailureSession struct {
	stubSession
	updater *Updater
	panic   bool
}

type trackedSelectedMailbox struct {
	stubSelectedMailbox
	unselected int
}

func (m *trackedSelectedMailbox) Unselect(context.Context) error {
	m.unselected++
	return nil
}

type invalidSelectSession struct {
	stubSession
	mailbox *trackedSelectedMailbox
}

func (s *invalidSelectSession) Select(_ context.Context, _ string, _ *Updater, _ *SelectOptions) (*SelectResult, error) {
	return &SelectResult{
		Mailbox: s.mailbox,
		Snapshot: SelectSnapshot{
			UIDs:   []imap.UID{2},
			Status: imap.MailboxStatus{NumMessages: 0, UIDNext: 3},
		},
	}, nil
}

func (s *selectFailureSession) Select(_ context.Context, _ string, updater *Updater, _ *SelectOptions) (*SelectResult, error) {
	s.updater = updater
	if s.panic {
		panic("select panic")
	}
	return nil, errors.New("select failed")
}

func TestFailedSelectInvalidatesUpdater(t *testing.T) {
	limits := Limits{}.withDefaults()
	for _, panicSelect := range []bool{false, true} {
		t.Run(fmt.Sprintf("panic=%v", panicSelect), func(t *testing.T) {
			session := &selectFailureSession{panic: panicSelect}
			func() {
				defer func() {
					recovered := recover()
					if panicSelect && recovered == nil {
						t.Fatal("SELECT panic was swallowed")
					}
				}()
				state := newSessionState(true)
				if !state.authenticate(session) {
					t.Fatal("failed to authenticate test state")
				}
				_, _ = openSelectedState(context.Background(), &state, "INBOX", nil, limits, nil)
			}()
			if session.updater == nil {
				t.Fatal("backend did not receive updater")
			}
			if err := session.updater.Push(&UpdateBatch{}); !errors.Is(err, ErrUpdaterClosed) {
				t.Fatalf("Push after failed SELECT = %v", err)
			}
		})
	}
}

func TestInvalidSelectSnapshotReleasesBackendHandle(t *testing.T) {
	mailbox := &trackedSelectedMailbox{}
	session := &invalidSelectSession{mailbox: mailbox}
	state := newSessionState(true)
	if !state.authenticate(session) {
		t.Fatal("failed to authenticate test state")
	}
	if _, err := openSelectedState(context.Background(), &state, "INBOX", nil, Limits{}.withDefaults(), nil); err == nil {
		t.Fatal("invalid snapshot was accepted")
	}
	if mailbox.unselected != 1 || state.state != stateAuthenticated || state.selected != nil {
		t.Fatalf("rejected selection cleanup = %d, state %#v", mailbox.unselected, state)
	}
}

func TestUpdateRevisionAndOriginAccounting(t *testing.T) {
	selected := &selectedState{uids: []imap.UID{1, 2}, revision: "r1"}
	updates, err := selected.applyBatch(&UpdateBatch{
		Before: "r1", After: "r2", Origin: 7,
		Changes: []Update{
			&UpdateAdd{UIDs: []imap.UID{3}},
			&UpdateFlags{UID: 2, Flags: []imap.Flag{imap.FlagSeen}},
		},
	}, updateAccounting{origin: 7, effect: effectStore})
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 1 || updates[0].kind != updateExists || updates[0].exists != 3 {
		t.Fatalf("same-origin STORE accounting delivered %#v", updates)
	}
	if !slices.Equal(selected.uids, []imap.UID{1, 2, 3}) || selected.revision != "r2" {
		t.Fatalf("selected state = %v/%q", selected.uids, selected.revision)
	}

	updates, err = selected.applyBatch(&UpdateBatch{
		Before: "r2", After: "r3", Origin: 8,
		Changes: []Update{&UpdateFlags{UID: 2, Flags: []imap.Flag{imap.FlagAnswered}}},
	}, updateAccounting{origin: 7, effect: effectStore})
	if err != nil || len(updates) != 1 || updates[0].kind != updateMessageFlags {
		t.Fatalf("unrelated origin was suppressed: %#v, %v", updates, err)
	}

	updates, err = selected.applyBatch(&UpdateBatch{
		Before: "r3", After: "r4", Origin: 9,
		Changes: []Update{&UpdateExpunge{UID: 1}, &UpdateExpunge{UID: 2}},
	}, updateAccounting{origin: 9, effect: effectExpunge})
	if err != nil || len(updates) != 0 || !slices.Equal(selected.uids, []imap.UID{3}) {
		t.Fatalf("same-origin EXPUNGE accounting = %#v, UIDs %v, err %v", updates, selected.uids, err)
	}

	_, err = selected.applyBatch(&UpdateBatch{Before: "wrong", After: "r5"}, updateAccounting{})
	if !errors.Is(err, ErrRevisionMismatch) {
		t.Fatalf("revision mismatch = %v", err)
	}

	selected = &selectedState{uids: []imap.UID{3}, revision: "r1"}
	_, err = selected.applyBatch(&UpdateBatch{
		Before: "r1", After: "r2",
		Changes: []Update{&UpdateVanished{UIDs: imap.UIDSetRange(3, 0)}},
	}, updateAccounting{})
	if !errors.Is(err, ErrRevisionMismatch) {
		t.Fatalf("dynamic VANISHED set = %v", err)
	}
}

func TestWireUpdateCoalescingPreservesRemovalSemantics(t *testing.T) {
	updates := coalesceWireUpdates([]deliveredUpdate{
		{kind: updateExists, exists: 3},
		{kind: updateExists, exists: 4},
		{kind: updateMessageFlags, uid: 1, flags: []imap.Flag{imap.FlagSeen}},
		{kind: updateMessageFlags, uid: 1, flags: []imap.Flag{imap.FlagAnswered}},
		{kind: updateMessageExpunge, uid: 2, seqNum: 2},
		{kind: updateMessageExpunge, uid: 3, seqNum: 2},
		{kind: updateMessageVanished, uids: imap.UIDSetNum(5)},
		{kind: updateMessageVanished, uids: imap.UIDSetNum(7)},
		{kind: updateMessageVanished, uids: imap.UIDSetNum(9), earlier: true},
	})
	if len(updates) != 6 {
		t.Fatalf("coalesced updates = %#v", updates)
	}
	if updates[0].kind != updateExists || updates[0].exists != 4 {
		t.Fatalf("EXISTS coalescing = %#v", updates[0])
	}
	if updates[1].kind != updateMessageFlags || !slices.Equal(updates[1].flags, []imap.Flag{imap.FlagAnswered}) {
		t.Fatalf("flag coalescing = %#v", updates[1])
	}
	if updates[2].kind != updateMessageExpunge || updates[3].kind != updateMessageExpunge {
		t.Fatalf("EXPUNGE values were merged: %#v", updates[2:4])
	}
	if updates[4].uids.String() != "5,7" || updates[4].earlier || !updates[5].earlier {
		t.Fatalf("VANISHED coalescing = %#v", updates[4:])
	}
}

func TestUpdateQueueBoundsAndLifetime(t *testing.T) {
	overflow := make(chan struct{}, 1)
	queue := newUpdateQueue(1, 1024, func() { overflow <- struct{}{} })
	updater := newUpdater(queue)
	if err := updater.Push(&UpdateBatch{Before: "a", After: "b"}); err != nil {
		t.Fatal(err)
	}
	if err := updater.Push(&UpdateBatch{Before: "b", After: "c"}); !errors.Is(err, ErrUpdateOverflow) {
		t.Fatalf("second Push = %v", err)
	}
	select {
	case <-overflow:
	case <-time.After(time.Second):
		t.Fatal("overflow callback did not run synchronously")
	}
	updater.core.close()
	if err := updater.Push(&UpdateBatch{}); !errors.Is(err, ErrUpdaterClosed) {
		t.Fatalf("Push after close = %v", err)
	}
}

func TestUpdateQueueBoundsBeforeCloneAndOwnsAcceptedBatch(t *testing.T) {
	overflowed := false
	tooSmall := newUpdateQueue(1, 80, func() { overflowed = true })
	updater := newUpdater(tooSmall)
	large := &UpdateBatch{
		Before: "a", After: "b",
		Changes: []Update{&UpdateFlags{UID: 1, Flags: []imap.Flag{imap.Flag(strings.Repeat("x", 128))}}},
	}
	if err := updater.Push(large); !errors.Is(err, ErrUpdateOverflow) || !overflowed {
		t.Fatalf("oversized batch = %v, overflow %v", err, overflowed)
	}
	if items := tooSmall.popAll(); len(items) != 0 {
		t.Fatalf("oversized batch was retained: %#v", items)
	}

	queue := newUpdateQueue(1, 1024, nil)
	updater = newUpdater(queue)
	accepted := &UpdateBatch{Before: "r1", After: "r2", Changes: []Update{&UpdateAdd{UIDs: []imap.UID{2}}}}
	if err := updater.Push(accepted); err != nil {
		t.Fatal(err)
	}
	accepted.After = "mutated"
	accepted.Changes[0].(*UpdateAdd).UIDs[0] = 99
	retained := queue.popAll()
	if len(retained) != 1 || retained[0].After != "r2" || retained[0].Changes[0].(*UpdateAdd).UIDs[0] != 2 {
		t.Fatalf("queue retained backend-owned aliases: %#v", retained)
	}
}

func TestMalformedUpdateIsNotSilentlyDropped(t *testing.T) {
	var nilExpunge *UpdateExpunge
	batch := cloneUpdateBatch(&UpdateBatch{
		Before: "r1", After: "r2", Changes: []Update{nilExpunge},
	})
	selected := &selectedState{uids: []imap.UID{1}, revision: "r1"}
	if _, err := selected.applyBatch(batch, updateAccounting{}); !errors.Is(err, ErrRevisionMismatch) {
		t.Fatalf("typed-nil update = %v", err)
	}
	if _, err := selected.applyBatch(&UpdateBatch{
		Before: "r1", After: "r2", Changes: []Update{&UpdateAdd{}},
	}, updateAccounting{}); !errors.Is(err, ErrRevisionMismatch) {
		t.Fatalf("empty ADD update = %v", err)
	}
}

func TestUpdateOverflowForceClosesBlockedWriter(t *testing.T) {
	serverSide, clientSide := net.Pipe()
	defer clientSide.Close() // deliberately never read: the server write blocks
	server := New(nil, &Options{Limits: Limits{
		MaxQueuedUpdates:     1,
		MaxQueuedUpdateBytes: 1024,
		ForceCloseTimeout:    20 * time.Millisecond,
	}})
	c, err := newConn(context.Background(), server, serverSide)
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()

	writerStarted := make(chan struct{})
	writerDone := make(chan error, 1)
	go func() {
		close(writerStarted)
		literal, err := c.encoder.ResponseLiteral(1<<20, false)
		if err == nil {
			_, err = io.CopyN(literal, &zeroReader{}, 1<<20)
		}
		writerDone <- err
	}()
	<-writerStarted

	queue := newUpdateQueue(1, 1024, c.updateOverflow)
	updater := newUpdater(queue)
	if err := updater.Push(&UpdateBatch{Before: "a", After: "b"}); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := updater.Push(&UpdateBatch{Before: "b", After: "c"}); !errors.Is(err, ErrUpdateOverflow) {
		t.Fatalf("overflow Push = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("overflow Push blocked for %v", elapsed)
	}
	select {
	case <-c.ctx.Done():
		if !errors.Is(context.Cause(c.ctx), ErrUpdateOverflow) {
			t.Fatalf("connection cause = %v", context.Cause(c.ctx))
		}
	case <-time.After(time.Second):
		t.Fatal("overflow did not cancel connection context")
	}
	select {
	case err := <-writerDone:
		if err == nil {
			t.Fatal("blocked writer unexpectedly completed successfully")
		}
	case <-time.After(time.Second):
		t.Fatal("forced close did not unblock writer")
	}
}

type stubSession struct{}

func (*stubSession) List(context.Context, *ListWriter, string, []string, *ListOptions) error {
	return nil
}
func (*stubSession) Status(context.Context, string, *StatusOptions) (*imap.StatusData, error) {
	return &imap.StatusData{}, nil
}
func (*stubSession) Create(context.Context, string, *CreateOptions) error { return nil }
func (*stubSession) Delete(context.Context, string, *DeleteOptions) error { return nil }
func (*stubSession) Rename(context.Context, string, string, *RenameOptions) error {
	return nil
}
func (*stubSession) Subscribe(context.Context, string, *SubscribeOptions) error { return nil }
func (*stubSession) Unsubscribe(context.Context, string, *UnsubscribeOptions) error {
	return nil
}
func (*stubSession) Append(context.Context, string, io.Reader, *AppendOptions) (*imap.AppendData, error) {
	return &imap.AppendData{}, nil
}
func (*stubSession) Select(context.Context, string, *Updater, *SelectOptions) (*SelectResult, error) {
	return nil, nil
}
func (*stubSession) Close(context.Context) error { return nil }

var _ Session = (*stubSession)(nil)
