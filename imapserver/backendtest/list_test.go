package backendtest_test

import (
	"context"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
	"github.com/kiliant/go-imap/imapserver/backendtest"
	"github.com/kiliant/go-imap/imapserver/memory"
)

// Run the actual public suite against faulty backends in child test processes:
// their expected t.Fatal/t.Error must not mark this parent test as failed.
func TestListConformance(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		mode string
		want string
	}{
		{mode: "witness"},
		{mode: "unwitnessed"},
		{mode: "drop-remote-descendants", want: `want only parent "remote-parent"`},
		{mode: "filter-special-use-descendants", want: `want only parent "recursive"`},
		{mode: "missing-childinfo", want: "ChildInfo = [], want SUBSCRIBED"},
		{mode: "hidden-from-star", want: `but LIST "" "*" omits it`},
	} {
		t.Run(test.mode, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestListConformanceFixture$/^list-", "-test.v")
			cmd.Env = append(os.Environ(), "GO_IMAP_LIST_CONFORMANCE_FIXTURE="+test.mode)
			output, err := cmd.CombinedOutput()
			if test.want == "" {
				if err != nil {
					t.Fatalf("conforming fixture failed: %v\n%s", err, output)
				}
				// The witness fixture includes a real remote-descendant scenario.
				if test.mode == "witness" && !strings.Contains(string(output), "PASS: TestListConformanceFixture/list-recursivematch-remote-descendant") {
					t.Fatalf("remote-descendant fixture did not run:\n%s", output)
				}
			} else if err == nil || !strings.Contains(string(output), test.want) {
				t.Fatalf("faulty fixture: err=%v, want failure containing %q\n%s", err, test.want, output)
			}
		})
	}
}

func TestListConformanceFixture(t *testing.T) {
	mode := os.Getenv("GO_IMAP_LIST_CONFORMANCE_FIXTURE")
	if mode == "" {
		t.Skip("helper for TestListConformance")
	}
	backendtest.Run(t, &backendtest.Harness{New: func(ctx context.Context, _ *backendtest.InstanceOptions) (*backendtest.Instance, error) {
		inner := memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}})
		backend := &listFixtureBackend{Backend: inner, mode: mode}
		credentials := imapserver.Credentials{Mechanism: "PLAIN", Username: "alice", Password: "secret"}
		seed, err := inner.Authenticate(ctx, &imapserver.ConnInfo{}, &credentials, nil)
		if err != nil {
			return nil, err
		}
		defer seed.Close(ctx, nil)
		for _, name := range []string{"remote-parent", "remote-parent/child"} {
			if err := seed.Create(ctx, name, nil); err != nil {
				return nil, err
			}
		}
		return &backendtest.Instance{Backend: backend, Credentials: credentials}, nil
	}})
}

type listFixtureBackend struct {
	*memory.Backend
	mode string
}

func (b *listFixtureBackend) SupportsCapability(ctx context.Context, name string, options *imapserver.CapabilitySupportOptions) bool {
	if name == "LIST-EXTENDED" && (b.mode == "unwitnessed" || b.mode == "hidden-from-star") {
		return false
	}
	return b.Backend.SupportsCapability(ctx, name, options)
}

func (b *listFixtureBackend) Authenticate(ctx context.Context, info *imapserver.ConnInfo, credentials *imapserver.Credentials, options *imapserver.AuthenticateOptions) (imapserver.Session, error) {
	session, err := b.Backend.Authenticate(ctx, info, credentials, options)
	if err != nil {
		return nil, err
	}
	return &listFixtureSession{Session: session, mode: b.mode}, nil
}

type listFixtureSession struct {
	imapserver.Session
	mode string
}

func (s *listFixtureSession) List(ctx context.Context, writer *imapserver.ListWriter, reference string, patterns []string, options *imapserver.ListOptions) error {
	return s.Session.List(ctx, &imapserver.ListWriter{WriteFunc: func(ctx context.Context, data *imap.ListData) error {
		copy := *data
		if copy.Mailbox == "remote-parent" && options != nil && options.SelectRecursiveMatch && !options.SelectRemote {
			return nil
		}
		if copy.Mailbox == "remote-parent/child" {
			if options == nil || !options.SelectRemote {
				return nil
			}
			copy.Attrs = append(slices.Clone(copy.Attrs), imap.MailboxAttrRemote)
		}
		if s.mode == "hidden-from-star" && reference == "" && slices.Equal(patterns, []string{"*"}) && copy.Mailbox == "recursive/child" {
			return nil
		}
		if options != nil && options.SelectRecursiveMatch {
			switch s.mode {
			case "drop-remote-descendants":
				if options.SelectRemote && copy.Mailbox == "remote-parent" {
					return nil
				}
			case "filter-special-use-descendants":
				if options.SelectSpecialUse && copy.Mailbox == "recursive" {
					return nil
				}
			case "missing-childinfo":
				copy.ChildInfo = nil
			}
		}
		return writer.WriteList(ctx, &copy)
	}}, reference, patterns, options)
}
