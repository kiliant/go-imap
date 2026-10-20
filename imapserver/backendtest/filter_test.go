package backendtest_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
	"github.com/kiliant/go-imap/imapserver/backendtest"
	"github.com/kiliant/go-imap/imapserver/memory"
)

// A forwarding wrapper has structural methods its inner backend cannot serve.
// The suite must skip those, while still exercising retained server metadata
// and LIST-EXTENDED's independent routing witness.
func TestFilteredBackendConformance(t *testing.T) {
	serverReads := 0
	backendtest.Run(t, &backendtest.Harness{New: func(context.Context, *backendtest.InstanceOptions) (*backendtest.Instance, error) {
		return &backendtest.Instance{
			Backend:     &filteredFixtureBackend{Backend: memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}}), reads: &serverReads},
			Credentials: imapserver.Credentials{Mechanism: "PLAIN", Username: "alice", Password: "secret"},
		}, nil
	}})
	if serverReads == 0 {
		t.Fatal("retained server metadata was not tested")
	}
}

type filteredFixtureBackend struct {
	*memory.Backend
	reads *int
}

func (*filteredFixtureBackend) AllowCapability(_ context.Context, name string, _ *imapserver.CapabilityFilterOptions) bool {
	return name == "METADATA-SERVER"
}

func (b *filteredFixtureBackend) Authenticate(ctx context.Context, info *imapserver.ConnInfo, credentials *imapserver.Credentials, options *imapserver.AuthenticateOptions) (imapserver.Session, error) {
	session, err := b.Backend.Authenticate(ctx, info, credentials, options)
	if err != nil {
		return nil, err
	}
	return &filteredFixtureSession{Session: session, MetadataSession: session.(imapserver.MetadataSession), reads: b.reads}, nil
}

type filteredFixtureSession struct {
	imapserver.Session
	imapserver.MetadataSession
	// These deliberately have no implementation. Their filtered conformance
	// cases must not invoke the forwarding methods.
	imapserver.NamespaceSession
	reads *int
}

func (s *filteredFixtureSession) GetMetadata(ctx context.Context, mailbox string, entries []imap.MetadataEntryName, options *imapserver.GetMetadataOptions) (*imap.MailboxMetadata, error) {
	if mailbox != "" {
		return nil, fmt.Errorf("mailbox metadata is filtered")
	}
	*s.reads++
	return s.MetadataSession.GetMetadata(ctx, mailbox, entries, options)
}

func (s *filteredFixtureSession) SetMetadata(ctx context.Context, mailbox string, entries []imap.MetadataEntry, options *imapserver.SetMetadataOptions) error {
	if mailbox != "" {
		return fmt.Errorf("mailbox metadata is filtered")
	}
	return s.MetadataSession.SetMetadata(ctx, mailbox, entries, options)
}
