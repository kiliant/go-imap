package backendtest_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
	"github.com/kiliant/go-imap/imapserver/backendtest"
	"github.com/kiliant/go-imap/imapserver/memory"
)

func TestSearchConformance(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ mode, want string }{
		{mode: "correct"}, {mode: "no-witnesses"},
		{mode: "always-all", want: "want [1]"},
		{mode: "duplicates", want: "want [1 2 3]"},
		{mode: "ignore-modseq", want: "modseq-none"},
	} {
		t.Run(test.mode, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestSearchConformanceFixture$/(search|sort-returns-a-permutation|thread-reports-each-message-once)$", "-test.v")
			cmd.Env = append(os.Environ(), "GO_IMAP_SEARCH_FIXTURE="+test.mode)
			output, err := cmd.CombinedOutput()
			if test.want != "" {
				if err == nil || !strings.Contains(string(output), test.want) {
					t.Fatalf("faulty fixture: err=%v, want %q\n%s", err, test.want, output)
				}
			} else if err != nil {
				t.Fatalf("conforming fixture failed: %v\n%s", err, output)
			} else if test.mode == "no-witnesses" && !strings.Contains(string(output), "SKIP: TestSearchConformanceFixture/search/modseq-all") {
				t.Fatalf("unwitnessed extension was not skipped:\n%s", output)
			}
		})
	}
}

func TestSearchConformanceFixture(t *testing.T) {
	mode := os.Getenv("GO_IMAP_SEARCH_FIXTURE")
	if mode == "" {
		t.Skip("helper for TestSearchConformance")
	}
	backendtest.Run(t, &backendtest.Harness{New: func(context.Context, *backendtest.InstanceOptions) (*backendtest.Instance, error) {
		return &backendtest.Instance{
			Backend:     &searchFixtureBackend{Backend: memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}}), mode: mode},
			Credentials: imapserver.Credentials{Mechanism: "PLAIN", Username: "alice", Password: "secret"},
		}, nil
	}})
}

type searchFixtureBackend struct {
	*memory.Backend
	mode string
}

func (b *searchFixtureBackend) SupportsCapability(ctx context.Context, name string, options *imapserver.CapabilitySupportOptions) bool {
	return b.mode != "no-witnesses" && b.Backend.SupportsCapability(ctx, name, options)
}

func (b *searchFixtureBackend) Authenticate(ctx context.Context, info *imapserver.ConnInfo, credentials *imapserver.Credentials, options *imapserver.AuthenticateOptions) (imapserver.Session, error) {
	session, err := b.Backend.Authenticate(ctx, info, credentials, options)
	if err != nil {
		return nil, err
	}
	return &searchFixtureSession{Session: session, mode: b.mode}, nil
}

type searchFixtureSession struct {
	imapserver.Session
	mode string
}

func (s *searchFixtureSession) Select(ctx context.Context, name string, updater *imapserver.Updater, options *imapserver.SelectOptions) (*imapserver.SelectResult, error) {
	result, err := s.Session.Select(ctx, name, updater, options)
	if err != nil {
		return nil, err
	}
	result.Mailbox = &searchFixtureMailbox{SelectedMailbox: result.Mailbox, mode: s.mode}
	return result, nil
}

type searchFixtureMailbox struct {
	imapserver.SelectedMailbox
	mode string
}

func (m *searchFixtureMailbox) Search(ctx context.Context, query *imapserver.SearchQuery, options *imapserver.SearchOptions) (*imapserver.SearchResult, error) {
	if query == nil || query.Criteria() == nil {
		return nil, fmt.Errorf("fixture refuses nil or zero queries")
	}
	_, modseq := query.Criteria().(imap.SearchModSeq)
	if m.mode == "always-all" || m.mode == "ignore-modseq" && modseq {
		var err error
		query, err = imapserver.NewSearchQuery(imap.SearchAll, nil)
		if err != nil {
			return nil, err
		}
	}
	result, err := m.SelectedMailbox.Search(ctx, query, options)
	if err == nil && m.mode == "duplicates" && len(result.UIDs) > 0 {
		result.UIDs = append(result.UIDs, result.UIDs[0])
	}
	return result, err
}

func (m *searchFixtureMailbox) Sort(ctx context.Context, query *imapserver.SearchQuery, keys []imap.SortKeySpec, options *imapserver.SortOptions) ([]imap.UID, error) {
	if query == nil || query.Criteria() == nil {
		return nil, fmt.Errorf("fixture refuses nil or zero SORT queries")
	}
	return m.SelectedMailbox.(imapserver.SortMailbox).Sort(ctx, query, keys, options)
}

func (m *searchFixtureMailbox) Thread(ctx context.Context, query *imapserver.SearchQuery, algorithm imap.ThreadAlgorithm, options *imapserver.ThreadOptions) ([]imap.ThreadNode, error) {
	if query == nil || query.Criteria() == nil {
		return nil, fmt.Errorf("fixture refuses nil or zero THREAD queries")
	}
	return m.SelectedMailbox.(imapserver.ThreadMailbox).Thread(ctx, query, algorithm, options)
}
