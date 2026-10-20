package imapserver_test

// backendtest against external adapters whose results are unusual but valid
// under the exported contract (T33 review of 4a61a32). Each adapter wraps
// memory without forwarding CapabilitySupport, so only the baseline suite and
// structurally witnessed cases apply; a correct suite passes or skips every
// subtest.

import (
	"context"
	"io"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
	"github.com/kiliant/go-imap/imapserver/backendtest"
)

func r4Harness(wrap func(imapserver.Session) imapserver.Session) *backendtest.Harness {
	return &backendtest.Harness{New: func(context.Context, *backendtest.InstanceOptions) (*backendtest.Instance, error) {
		return &backendtest.Instance{
			Backend:     &r4WrapBackend{inner: r4Memory(), wrap: wrap},
			Credentials: imapserver.Credentials{Mechanism: "PLAIN", Username: "alice", Password: "secret"},
		}, nil
	}}
}

type r4WrapBackend struct {
	inner imapserver.Backend
	wrap  func(imapserver.Session) imapserver.Session
}

func (b *r4WrapBackend) Authenticate(ctx context.Context, info *imapserver.ConnInfo, cred *imapserver.Credentials, opts *imapserver.AuthenticateOptions) (imapserver.Session, error) {
	s, err := b.inner.Authenticate(ctx, info, cred, opts)
	if err != nil {
		return nil, err
	}
	return b.wrap(s), nil
}

// ---- unordered results, extra STATUS items, presence-false UID data ----

// None of these orders is fixed by the contract: Fetch "streams the requested
// items ... one message at a time", Search "returns the matching UIDs",
// Expunge reports "each removed UID", List writes results. Status: "Only the
// items options requests need be populated; the framework writes what it
// asked for and nothing else." AppendData.HasUID and CopyData.HasUIDs carry
// presence explicitly (API-STABILITY rule 8), and without UIDPLUS the data is
// optional.
type r4OddSession struct{ imapserver.Session }
type r4OddSelected struct{ imapserver.SelectedMailbox }

func (s *r4OddSession) List(ctx context.Context, w *imapserver.ListWriter, ref string, patterns []string, o *imapserver.ListOptions) error {
	var got []*imap.ListData
	err := s.Session.List(ctx, &imapserver.ListWriter{WriteFunc: func(_ context.Context, d *imap.ListData) error {
		got = append(got, d)
		return nil
	}}, ref, patterns, o)
	for i := len(got) - 1; i >= 0; i-- {
		if werr := w.WriteList(ctx, got[i]); werr != nil {
			return werr
		}
	}
	return err
}
func (s *r4OddSession) Status(ctx context.Context, name string, o *imapserver.StatusOptions) (*imap.StatusData, error) {
	extra := []imap.StatusItem{imap.StatusItemMessages, imap.StatusItemUIDNext, imap.StatusItemUIDValidity, imap.StatusItemUnseen}
	items := slices.Clone(o.Items)
	for _, item := range extra {
		if !slices.Contains(items, item) {
			items = append(items, item)
		}
	}
	return s.Session.Status(ctx, name, &imapserver.StatusOptions{Items: items})
}
func (s *r4OddSession) Append(ctx context.Context, name string, r io.Reader, o *imapserver.AppendOptions) (*imap.AppendData, error) {
	_, err := s.Session.Append(ctx, name, r, o)
	if err != nil {
		return nil, err
	}
	return &imap.AppendData{}, nil
}
func (s *r4OddSession) Select(ctx context.Context, name string, u *imapserver.Updater, o *imapserver.SelectOptions) (*imapserver.SelectResult, error) {
	r, err := s.Session.Select(ctx, name, u, o)
	if err == nil {
		r.Mailbox = &r4OddSelected{r.Mailbox}
	}
	return r, err
}
func (m *r4OddSelected) Search(ctx context.Context, q *imapserver.SearchQuery, o *imapserver.SearchOptions) (*imapserver.SearchResult, error) {
	r, err := m.SelectedMailbox.Search(ctx, q, o)
	if err != nil || r == nil {
		return r, err
	}
	uids := slices.Clone(r.UIDs)
	// Reversed only. Duplicates are not used: SearchResult does not say
	// whether UIDs must be unique, so they are not clearly contract-valid.
	slices.Reverse(uids)
	return &imapserver.SearchResult{UIDs: uids, ModSeq: r.ModSeq}, nil
}
func (m *r4OddSelected) Fetch(ctx context.Context, w *imapserver.FetchWriter, uids imap.UIDSet, o *imapserver.FetchOptions) error {
	if uids.Dynamic() {
		return m.SelectedMailbox.Fetch(ctx, w, uids, o)
	}
	all, err := m.SelectedMailbox.Search(ctx, r4All(), nil)
	if err != nil {
		return err
	}
	ordered := slices.Clone(all.UIDs)
	slices.Sort(ordered)
	for i := len(ordered) - 1; i >= 0; i-- {
		if !uids.Contains(ordered[i]) {
			continue
		}
		if err := m.SelectedMailbox.Fetch(ctx, w, imap.UIDSetNum(ordered[i]), o); err != nil {
			return err
		}
	}
	return nil
}
func (m *r4OddSelected) Copy(ctx context.Context, uids imap.UIDSet, dest string, o *imapserver.CopyOptions) (*imap.CopyData, error) {
	_, err := m.SelectedMailbox.Copy(ctx, uids, dest, o)
	if err != nil {
		return nil, err
	}
	return &imap.CopyData{}, nil
}
func (m *r4OddSelected) Expunge(ctx context.Context, w *imapserver.ExpungeWriter, uids *imap.UIDSet, o *imapserver.ExpungeOptions) error {
	var removed []imap.UID
	err := m.SelectedMailbox.Expunge(ctx, &imapserver.ExpungeWriter{WriteFunc: func(_ context.Context, uid imap.UID, _ *imapserver.WriteExpungeOptions) error {
		removed = append(removed, uid)
		return nil
	}}, uids, o)
	for i := len(removed) - 1; i >= 0; i-- {
		if werr := w.WriteExpunge(ctx, removed[i], nil); werr != nil {
			return werr
		}
	}
	return err
}

func r4All() *imapserver.SearchQuery {
	q, err := imapserver.NewSearchQuery(nil, nil)
	if err != nil {
		panic(err)
	}
	return q
}

func TestReview4SuiteUnorderedValidResults(t *testing.T) {
	backendtest.Run(t, r4Harness(func(s imapserver.Session) imapserver.Session { return &r4OddSession{s} }))
}

// ---- RENAME INBOX recreating INBOX with a greater UIDVALIDITY -----------

// RFC 3501 section 6.3.5: RENAME INBOX "moves all messages in INBOX to a new
// mailbox with the given name, leaving INBOX empty", inferiors unaffected;
// the highest UID "MUST be preserved so that a new mailbox created with the
// same name will not reuse the identifiers of the former incarnation, UNLESS
// the new incarnation has a different unique identifier validity value".
// RFC 5465 section 5.4: "When INBOX is renamed, a new INBOX is assumed to be
// created." Session.Rename: "its messages move to the new name, and INBOX
// itself remains, empty, with its inferiors." This adapter presents exactly
// that, with INBOX reincarnated under a greater UIDVALIDITY.
type r4ReincarnateSession struct {
	imapserver.Session
	bumped atomic.Bool
}

const r4Bump = 1000

func (s *r4ReincarnateSession) Rename(ctx context.Context, from, to string, o *imapserver.RenameOptions) error {
	err := s.Session.Rename(ctx, from, to, o)
	if err == nil && strings.EqualFold(from, "INBOX") {
		s.bumped.Store(true)
	}
	return err
}
func (s *r4ReincarnateSession) Status(ctx context.Context, name string, o *imapserver.StatusOptions) (*imap.StatusData, error) {
	d, err := s.Session.Status(ctx, name, o)
	if err == nil && d != nil && strings.EqualFold(name, "INBOX") && s.bumped.Load() {
		if v, ok := d.Values[imap.StatusItemUIDValidity].(uint64); ok {
			d.Values[imap.StatusItemUIDValidity] = v + r4Bump
			d.UIDValidity += r4Bump
		}
	}
	return d, err
}
func (s *r4ReincarnateSession) Append(ctx context.Context, name string, r io.Reader, o *imapserver.AppendOptions) (*imap.AppendData, error) {
	_, err := s.Session.Append(ctx, name, r, o)
	return nil, err
}
func (s *r4ReincarnateSession) Select(ctx context.Context, name string, u *imapserver.Updater, o *imapserver.SelectOptions) (*imapserver.SelectResult, error) {
	r, err := s.Session.Select(ctx, name, u, o)
	if err == nil && strings.EqualFold(name, "INBOX") && s.bumped.Load() {
		r.Snapshot.Status.UIDValidity += r4Bump
	}
	if err == nil {
		r.Mailbox = &r4NoUIDCopySelected{r.Mailbox}
	}
	return r, err
}

type r4NoUIDCopySelected struct{ imapserver.SelectedMailbox }

func (m *r4NoUIDCopySelected) Copy(ctx context.Context, uids imap.UIDSet, dest string, o *imapserver.CopyOptions) (*imap.CopyData, error) {
	_, err := m.SelectedMailbox.Copy(ctx, uids, dest, o)
	return nil, err
}

func TestReview4SuiteRenameInboxReincarnation(t *testing.T) {
	backendtest.Run(t, r4Harness(func(s imapserver.Session) imapserver.Session { return &r4ReincarnateSession{Session: s} }))
}
