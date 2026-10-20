package backendtest_test

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
	"github.com/kiliant/go-imap/imapserver/backendtest"
	"github.com/kiliant/go-imap/imapserver/memory"
)

// TestOptionalResultConformance runs the whole suite against adapters whose
// only difference from the memory backend is how they use results the backend
// contract makes optional. Conforming ones must pass; the faulty ones prove the
// witnessed and reported-value checks are still enforced.
func TestOptionalResultConformance(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ mode, want string }{
		// APPEND, COPY and MOVE data are optional per call without UIDPLUS.
		{mode: "partial-append"},
		{mode: "partial-copy"},
		// A nil reader is CATENATE's documented unresolved result.
		{mode: "catenate-nil"},
		// Implementing an interface is not consent when its token is spoken.
		{mode: "unwitnessed-structure"},
		// HighestModSeq is optional in a CONDSTORE result.
		{mode: "zero-highestmodseq"},
		// UIDPLUS makes the results mandatory.
		{mode: "uidplus-partial-append", want: "Append data ="},
		// Reported values must name their own messages, witness or not.
		{mode: "wrong-append", want: "reported UID"},
		{mode: "wrong-copy", want: "reported destination UID"},
	} {
		t.Run(test.mode, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestOptionalResultFixture$", "-test.v")
			cmd.Env = append(os.Environ(), "GO_IMAP_OPTIONAL_FIXTURE="+test.mode)
			output, err := cmd.CombinedOutput()
			if test.want != "" {
				if err == nil || !strings.Contains(string(output), test.want) {
					t.Fatalf("faulty fixture: err=%v, want %q\n%s", err, test.want, output)
				}
				return
			}
			if err != nil {
				t.Fatalf("conforming fixture failed: %v\n%s", err, output)
			}
			if test.mode == "unwitnessed-structure" {
				for _, name := range []string{"condstore-conditional-store", "qresync-vanished-excludes-present-messages", "sort-returns-a-permutation", "thread-reports-each-message-once"} {
					if !strings.Contains(string(output), "SKIP: TestOptionalResultFixture/"+name) {
						t.Fatalf("unwitnessed %s was not skipped:\n%s", name, output)
					}
				}
			}
		})
	}
}

func TestOptionalResultFixture(t *testing.T) {
	mode := os.Getenv("GO_IMAP_OPTIONAL_FIXTURE")
	if mode == "" {
		t.Skip("helper for TestOptionalResultConformance")
	}
	backendtest.Run(t, &backendtest.Harness{New: func(context.Context, *backendtest.InstanceOptions) (*backendtest.Instance, error) {
		return &backendtest.Instance{
			Backend:     &optionalFixtureBackend{Backend: memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}}), mode: mode},
			Credentials: imapserver.Credentials{Mechanism: "PLAIN", Username: "alice", Password: "secret"},
		}, nil
	}})
}

type optionalFixtureBackend struct {
	*memory.Backend
	mode string
}

func (b *optionalFixtureBackend) SupportsCapability(ctx context.Context, name string, options *imapserver.CapabilitySupportOptions) bool {
	switch b.mode {
	case "partial-append", "partial-copy", "wrong-append", "wrong-copy":
		if name == "UIDPLUS" {
			return false
		}
	case "unwitnessed-structure":
		switch name {
		case "CONDSTORE", "QRESYNC", "SORT", "SORT=DISPLAY", "THREAD=ORDEREDSUBJECT", "THREAD=REFERENCES":
			return false
		}
	}
	return b.Backend.SupportsCapability(ctx, name, options)
}

func (b *optionalFixtureBackend) Authenticate(ctx context.Context, info *imapserver.ConnInfo, credentials *imapserver.Credentials, options *imapserver.AuthenticateOptions) (imapserver.Session, error) {
	session, err := b.Backend.Authenticate(ctx, info, credentials, options)
	if err != nil {
		return nil, err
	}
	return &optionalFixtureSession{Session: session, catenate: session.(imapserver.CatenateSession), mode: b.mode}, nil
}

type optionalFixtureSession struct {
	imapserver.Session
	catenate imapserver.CatenateSession
	mode     string
	appends  int
}

func (s *optionalFixtureSession) Append(ctx context.Context, name string, literal io.Reader, options *imapserver.AppendOptions) (*imap.AppendData, error) {
	data, err := s.Session.Append(ctx, name, literal, options)
	if err != nil || data == nil {
		return data, err
	}
	s.appends++
	switch s.mode {
	case "partial-append":
		if s.appends%2 == 0 {
			return nil, nil
		}
	case "uidplus-partial-append":
		// The opposite phase, so the strictly checked third APPEND of a
		// session is among the unreported ones.
		if s.appends%2 == 1 {
			return nil, nil
		}
	case "wrong-append":
		data.UID += 100
	}
	return data, nil
}

// ResolveCatenateURL declines the suite's made-up localhost authority with the
// nil-reader result and resolves everything else normally.
func (s *optionalFixtureSession) ResolveCatenateURL(ctx context.Context, url string, options *imapserver.CatenateOptions) (io.ReadCloser, error) {
	if s.mode == "catenate-nil" && strings.HasPrefix(url, "imap://localhost/") {
		return nil, nil
	}
	return s.catenate.ResolveCatenateURL(ctx, url, options)
}

func (s *optionalFixtureSession) Select(ctx context.Context, name string, updater *imapserver.Updater, options *imapserver.SelectOptions) (*imapserver.SelectResult, error) {
	result, err := s.Session.Select(ctx, name, updater, options)
	if err != nil {
		return nil, err
	}
	result.Mailbox = &optionalFixtureMailbox{SelectedMailbox: result.Mailbox, mode: s.mode}
	return result, nil
}

type optionalFixtureMailbox struct {
	imapserver.SelectedMailbox
	mode   string
	copies int
}

func (m *optionalFixtureMailbox) adjustCopy(data *imap.CopyData, err error) (*imap.CopyData, error) {
	if err != nil || data == nil {
		return data, err
	}
	m.copies++
	switch m.mode {
	case "partial-copy":
		if m.copies%2 == 0 {
			return nil, nil
		}
	case "wrong-copy":
		uids, _ := data.DestinationUIDs.Nums()
		for i := range uids {
			uids[i] += 100
		}
		data.DestinationUIDs = imap.UIDSetNum(uids...)
	}
	return data, nil
}

func (m *optionalFixtureMailbox) Copy(ctx context.Context, uids imap.UIDSet, destination string, options *imapserver.CopyOptions) (*imap.CopyData, error) {
	return m.adjustCopy(m.SelectedMailbox.Copy(ctx, uids, destination, options))
}

func (m *optionalFixtureMailbox) Move(ctx context.Context, uids imap.UIDSet, destination string, options *imapserver.MoveOptions) (*imap.CopyData, error) {
	return m.adjustCopy(m.SelectedMailbox.(imapserver.MoveMailbox).Move(ctx, uids, destination, options))
}

var errUnwitnessed = errors.New("fixture: this optional operation is broken but unwitnessed")

// The CONDSTORE, QRESYNC, SORT and THREAD methods are present on every
// selection. Under unwitnessed-structure they are broken, and their tokens
// are withdrawn, so the framework never calls them and neither may the suite.
func (m *optionalFixtureMailbox) StoreCondStore(ctx context.Context, writer *imapserver.FetchWriter, uids imap.UIDSet, flags *imapserver.StoreFlags, options *imapserver.StoreOptions) (*imapserver.CondStoreResult, error) {
	if m.mode == "unwitnessed-structure" {
		return nil, errUnwitnessed
	}
	result, err := m.SelectedMailbox.(imapserver.CondStoreMailbox).StoreCondStore(ctx, writer, uids, flags, options)
	if result != nil && m.mode == "zero-highestmodseq" {
		result.HighestModSeq = 0
	}
	return result, err
}

func (m *optionalFixtureMailbox) Resync(ctx context.Context, params *imapserver.QResyncSelect, options *imapserver.QResyncOptions) (*imapserver.QResyncResult, error) {
	if m.mode == "unwitnessed-structure" {
		return nil, errUnwitnessed
	}
	return m.SelectedMailbox.(imapserver.QResyncMailbox).Resync(ctx, params, options)
}

func (m *optionalFixtureMailbox) Sort(ctx context.Context, query *imapserver.SearchQuery, keys []imap.SortKeySpec, options *imapserver.SortOptions) ([]imap.UID, error) {
	if m.mode == "unwitnessed-structure" {
		return nil, errUnwitnessed
	}
	return m.SelectedMailbox.(imapserver.SortMailbox).Sort(ctx, query, keys, options)
}

func (m *optionalFixtureMailbox) Thread(ctx context.Context, query *imapserver.SearchQuery, algorithm imap.ThreadAlgorithm, options *imapserver.ThreadOptions) ([]imap.ThreadNode, error) {
	if m.mode == "unwitnessed-structure" {
		return []imap.ThreadNode{{Num: 1}, {Num: 1}}, nil
	}
	return m.SelectedMailbox.(imapserver.ThreadMailbox).Thread(ctx, query, algorithm, options)
}
