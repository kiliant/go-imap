package imapserver

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

type filterBackend struct {
	Backend
	allow func(context.Context, string, *CapabilityFilterOptions) bool
}

func (b filterBackend) AllowCapability(ctx context.Context, name string, options *CapabilityFilterOptions) bool {
	return b.allow(ctx, name, options)
}

type filterSession struct {
	Session
	allow func(context.Context, string, *CapabilityFilterOptions) bool
}

func (s filterSession) AllowCapability(ctx context.Context, name string, options *CapabilityFilterOptions) bool {
	return s.allow(ctx, name, options)
}

type metadataFilterSession struct {
	fullRev2Session
	get, set int
}

func (s *metadataFilterSession) GetMetadata(_ context.Context, mailbox string, _ []imap.MetadataEntryName, _ *MetadataOptions) (*imap.MailboxMetadata, error) {
	s.get++
	return &imap.MailboxMetadata{Mailbox: mailbox}, nil
}

func (s *metadataFilterSession) SetMetadata(context.Context, string, []imap.MetadataEntry, *MetadataOptions) error {
	s.set++
	return nil
}

func TestCapabilityFilterLegacyAndPrecedence(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	state := newSessionState(true)
	state.authenticate(&metadataFilterSession{})
	legacy := New(struct{ Backend }{}, nil)
	want := deriveCapabilitiesContext(ctx, &state, legacy)
	for _, name := range []string{"METADATA", "METADATA-SERVER", "LIST-METADATA", "IMAP4REV2"} {
		if !slices.Contains(want, name) {
			t.Fatalf("legacy structural support lost %s: %v", name, want)
		}
	}
	calls := 0
	backend := filterBackend{allow: func(got context.Context, name string, options *CapabilityFilterOptions) bool {
		calls++
		if got != ctx || options != nil {
			t.Error("filter lost context or nil options")
		}
		return name != "METADATA" && name != "NAMESPACE"
	}}
	server := New(backend, nil)
	got := deriveCapabilitiesContext(ctx, &state, server)
	for _, name := range []string{"METADATA", "LIST-METADATA", "NAMESPACE", "IMAP4REV2"} {
		if slices.Contains(got, name) {
			t.Errorf("filtered capability still advertised %s: %v", name, got)
		}
	}
	if !slices.Contains(got, "METADATA-SERVER") || calls == 0 {
		t.Fatalf("backend filter did not retain server scope: %v", got)
	}
	// A session policy answers alone, including for incorporated rev2 names.
	state.session = struct {
		*metadataFilterSession
		CapabilityFilter
	}{metadataFilterSession: &metadataFilterSession{}, CapabilityFilter: filterSession{allow: func(got context.Context, _ string, options *CapabilityFilterOptions) bool {
		if got != ctx || options != nil {
			t.Error("session filter lost context or nil options")
		}
		return true
	}}}
	before := calls
	if got := deriveCapabilitiesContext(ctx, &state, server); !slices.Equal(got, want) || calls != before {
		t.Fatalf("session did not supersede backend: %v, backend calls %d→%d", got, before, calls)
	}
}

func TestCapabilityFilterCannotGrantSupportOrFilterFramework(t *testing.T) {
	for _, allow := range []bool{false, true} {
		t.Run(fmt.Sprint(allow), func(t *testing.T) {
			backend := filterBackend{allow: func(context.Context, string, *CapabilityFilterOptions) bool { return allow }}
			server := New(backend, &Options{AllowInsecureAuth: true})
			state := newSessionState(false)
			state.authenticate(&stubSession{})
			got := deriveCapabilitiesContext(t.Context(), &state, server)
			for _, name := range []string{"METADATA", "QUOTA", "SORT", "IMAP4REV2", "AUTH=XOAUTH2"} {
				if slices.Contains(got, name) {
					t.Errorf("filter granted %s: %v", name, got)
				}
			}
			for _, name := range []string{"IMAP4REV1", "ENABLE", "LIST-EXTENDED"} {
				if !slices.Contains(got, name) {
					t.Errorf("filter withdrew framework token %s: %v", name, got)
				}
			}
		})
	}
	// An allow-all filter still cannot bypass a spoken witness's TLS gate.
	backend := filterBackend{Backend: bearerCapabilityBackend{witness: func(context.Context, string, *CapabilitySupportOptions) bool { return true }},
		allow: func(context.Context, string, *CapabilityFilterOptions) bool { return true }}
	// Embedding Backend deliberately hides the spoken interface, so expose it.
	server := New(struct {
		filterBackend
		CapabilitySupport
	}{backend, backend.Backend.(CapabilitySupport)}, nil)
	state := newSessionState(false)
	if slices.Contains(deriveCapabilities(&state, server), "AUTH=XOAUTH2") {
		t.Fatal("filter bypassed TLS requirement")
	}
	state.tls = true
	if !slices.Contains(deriveCapabilities(&state, server), "AUTH=XOAUTH2") {
		t.Fatal("allow-all filter lost witnessed capability over TLS")
	}
}

func TestCapabilityFilterPrunesDependencyChains(t *testing.T) {
	original := capabilityDescriptors
	t.Cleanup(func() { capabilityDescriptors = original })
	capabilityDescriptors = slices.Clone(original)
	registerCapabilities(
		capabilityDescriptor{Name: "TEST-LEAF", States: stateMaskAny, Depends: []string{"TEST-MIDDLE"}},
		capabilityDescriptor{Name: "TEST-MIDDLE", States: stateMaskAny, Depends: []string{"TEST-BASE"}},
		capabilityDescriptor{Name: "TEST-BASE", States: stateMaskAny, RequiresBackend: hasAuthenticationBackend},
	)
	state := newSessionState(false)
	server := New(filterBackend{allow: func(_ context.Context, name string, _ *CapabilityFilterOptions) bool { return name != "TEST-BASE" }}, nil)
	for _, name := range deriveCapabilities(&state, server) {
		if strings.HasPrefix(name, "TEST-") {
			t.Fatalf("transitive dependency survived filtered base: %s", name)
		}
	}
}

func TestCapabilityFilterMetadataScopes(t *testing.T) {
	for _, serverOnly := range []bool{false, true} {
		t.Run(fmt.Sprint(serverOnly), func(t *testing.T) {
			session := &metadataFilterSession{}
			backend := filterBackend{allow: func(_ context.Context, name string, _ *CapabilityFilterOptions) bool {
				if serverOnly {
					return name != "METADATA"
				}
				return name != "METADATA-SERVER"
			}}
			state := newSessionState(true)
			state.authenticate(session)
			var wire bytes.Buffer
			c := &conn{ctx: t.Context(), state: state, server: New(backend, nil),
				encoder: imapwire.NewEncoder(&wire, &imapwire.EncoderOptions{ServerResponse: true})}
			for _, mailbox := range []string{"", "INBOX"} {
				wire.Reset()
				get := &queuedCommand{tag: "a", name: "GETMETADATA", args: &getMetadataArgs{mailbox: mailbox, entries: []imap.MetadataEntryName{"/private/test"}}}
				set := &queuedCommand{tag: "b", name: "SETMETADATA", args: &setMetadataArgs{mailbox: mailbox, entries: []imap.MetadataEntry{{Name: "/private/test"}}}}
				if err := handleGetMetadata(t.Context(), c, get); err != nil {
					t.Fatal(err)
				}
				if err := handleSetMetadata(t.Context(), c, set); err != nil {
					t.Fatal(err)
				}
				if rejected := strings.Contains(wire.String(), "a BAD") && strings.Contains(wire.String(), "b BAD"); rejected != (serverOnly && mailbox != "") {
					t.Fatalf("scope %q: %s", mailbox, wire.String())
				}
			}
			want := 2
			if serverOnly {
				want = 1
			}
			if session.get != want || session.set != want {
				t.Fatalf("backend calls get=%d set=%d, want %d", session.get, session.set, want)
			}
		})
	}
}

type filteredLimitsSession struct {
	filterSession
	calls int
}

func (s *filteredLimitsSession) MessageLimits(context.Context, *MessageLimitOptions) (*MessageLimits, error) {
	s.calls++
	return &MessageLimits{HasMessageLimit: true, MessageLimit: 9, HasSaveLimit: true, SaveLimit: 7}, nil
}

func TestCapabilityFilterSkipsWithdrawnLimitQuery(t *testing.T) {
	for _, allowSave := range []bool{false, true} {
		session := &filteredLimitsSession{filterSession: filterSession{Session: &stubSession{}, allow: func(_ context.Context, name string, _ *CapabilityFilterOptions) bool {
			return name == "SAVELIMIT" && allowSave
		}}}
		state := newSessionState(true)
		state.authenticate(session)
		resolveMessageLimits(t.Context(), &state, nil)
		want := 0
		if allowSave {
			want = 1
		}
		if session.calls != want {
			t.Fatalf("limit calls=%d, want %d", session.calls, want)
		}
		got := deriveCapabilities(&state, New(nil, nil))
		if slices.Contains(got, "SAVELIMIT=7") != allowSave || slices.Contains(got, "MESSAGELIMIT=9") {
			t.Fatalf("filtered value tokens: %v", got)
		}
	}
}
