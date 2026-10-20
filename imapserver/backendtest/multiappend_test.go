package backendtest_test

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
	"github.com/kiliant/go-imap/imapserver/backendtest"
	"github.com/kiliant/go-imap/imapserver/memory"
)

func TestMultiAppendConformanceRejectsPartialCommit(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestPartialMultiAppendFixture$/^multiappend-is-atomic$", "-test.v")
	cmd.Env = append(os.Environ(), "GO_IMAP_PARTIAL_APPEND_FIXTURE=1")
	output, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "failed batch changed snapshot") {
		t.Fatalf("partial commit escaped conformance: %v\n%s", err, output)
	}
}

func TestPartialMultiAppendFixture(t *testing.T) {
	if os.Getenv("GO_IMAP_PARTIAL_APPEND_FIXTURE") == "" {
		t.Skip("child fixture for TestMultiAppendConformanceRejectsPartialCommit")
	}
	backendtest.Run(t, &backendtest.Harness{New: func(context.Context, *backendtest.InstanceOptions) (*backendtest.Instance, error) {
		return &backendtest.Instance{Backend: partialAppendBackend{memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}})}, Credentials: imapserver.Credentials{Mechanism: "PLAIN", Username: "alice", Password: "secret"}}, nil
	}})
}

type partialAppendBackend struct{ *memory.Backend }

func (b partialAppendBackend) Authenticate(ctx context.Context, info *imapserver.ConnInfo, credentials *imapserver.Credentials, options *imapserver.AuthenticateOptions) (imapserver.Session, error) {
	session, err := b.Backend.Authenticate(ctx, info, credentials, options)
	if err != nil {
		return nil, err
	}
	return partialAppendSession{session}, nil
}

type partialAppendSession struct{ imapserver.Session }

func (s partialAppendSession) MultiAppend(ctx context.Context, mailbox string, messages []imapserver.AppendMessage, _ *imapserver.MultiAppendOptions) (*imap.MultiAppendData, error) {
	for _, message := range messages {
		if _, err := s.Append(ctx, mailbox, message.Literal, nil); err != nil {
			return nil, err
		}
	}
	return nil, nil
}
