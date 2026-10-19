package maildir

import (
	"context"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
)

type session struct {
	account    *account
	selections map[*selected]bool
	closed     bool
}
type selected struct {
	session          *session
	recent           map[imap.UID]bool
	id               uint32
	readOnly, closed bool
}

func (s *session) enter(ctx context.Context) error {
	if err := s.account.enter(ctx); err != nil {
		return err
	}
	if s.closed {
		s.account.leave()
		return noError(imap.CodeClosed, "session is closed")
	}
	return nil
}
func (s *selected) enter(ctx context.Context) (string, *mailboxState, error) {
	if err := s.session.enter(ctx); err != nil {
		return "", nil, err
	}
	if !s.closed {
		for name, m := range s.session.account.state.Mailboxes {
			if m.UIDValidity == s.id {
				return name, m, nil
			}
		}
	}
	s.session.account.leave()
	return "", nil, noError(imap.CodeClosed, "mailbox is no longer selected")
}
func missingMailbox() error { return noError(imap.CodeNonExistent, "mailbox does not exist") }
func byID(state *accountState, id uint32) *mailboxState {
	for _, m := range state.Mailboxes {
		if m.UIDValidity == id {
			return m
		}
	}
	return nil
}

func (a *account) publishCommit(next *accountState, actions []fileAction, changes map[uint32][]imapserver.Update, origin imapserver.ChangeToken) error {
	batches := make(map[uint32]*imapserver.UpdateBatch)
	for id, updates := range changes {
		if len(updates) == 0 {
			continue
		}
		m := byID(next, id)
		if m == nil {
			return fmt.Errorf("maildir: missing updated mailbox")
		}
		if m.Revision == math.MaxUint64 {
			return noError(imap.CodeLimit, "revision space exhausted")
		}
		before := revision(m.Revision)
		m.Revision++
		batches[id] = &imapserver.UpdateBatch{Before: before, After: revision(m.Revision), Origin: origin, Changes: updates}
	}
	claims := a.prepareRecent(next, changes)
	if err := a.commit(next, actions); err != nil {
		return err
	}
	for id, batch := range batches {
		a.publishRecent(id, batch, claims[id])
	}
	return nil
}
func revision(n uint64) imapserver.MailboxRevision {
	return imapserver.MailboxRevision(strconv.FormatUint(n, 10))
}
func bump(m *mailboxState) (uint64, error) {
	if m.HighestModSeq >= math.MaxInt64 {
		return 0, noError(imap.CodeLimit, "modification sequence exhausted")
	}
	m.HighestModSeq++
	return m.HighestModSeq, nil
}
func allocateUID(m *mailboxState) (imap.UID, error) {
	if m.UIDNext == 0 || m.UIDNext == math.MaxUint32 {
		return 0, noError(imap.CodeLimit, "UID space exhausted")
	}
	uid := m.UIDNext
	m.UIDNext++
	return uid, nil
}
func ensureFlags(m *mailboxState, flags []imap.Flag) bool {
	changed := false
	for _, flag := range flags {
		if !imap.ContainsFlag(m.Flags, flag) {
			m.Flags = append(m.Flags, flag)
			changed = true
		}
	}
	return changed
}

// withMailboxFlags puts the mailbox's complete FLAGS list first in a batch,
// ahead of every update that uses a keyword it introduces (RFC 3501 section
// 7.2.6). Each list is the whole set, so a later one replaces an earlier one
// from the same batch; prepending both would apply the older, smaller set last.
func withMailboxFlags(changes []imapserver.Update, flags []imap.Flag) []imapserver.Update {
	update := &imapserver.UpdateMailboxFlags{Flags: slices.Clone(flags)}
	if len(changes) > 0 {
		if _, ok := changes[0].(*imapserver.UpdateMailboxFlags); ok {
			changes[0] = update
			return changes
		}
	}
	return append([]imapserver.Update{update}, changes...)
}

func uniqueFlags(flags []imap.Flag) []imap.Flag {
	var result []imap.Flag
	for _, flag := range flags {
		if !imap.ContainsFlag(result, flag) && !flag.Equal(imap.FlagRecent) {
			result = append(result, flag)
		}
	}
	return result
}
func mkdirMailbox(name string) []fileAction {
	var actions []fileAction
	for _, sub := range []string{"cur", "new", "tmp"} {
		actions = append(actions, fileAction{Kind: "mkdir", To: filepath.Join(mailboxPath(name), sub)})
	}
	return actions
}

func (s *session) Create(ctx context.Context, name string, options *imapserver.CreateOptions) error {
	if err := validMailbox(name); err != nil {
		return err
	}
	name = mailboxKey(name)
	if err := s.enter(ctx); err != nil {
		return err
	}
	defer s.account.leave()
	if s.account.state.Mailboxes[name] != nil {
		return noError(imap.CodeAlreadyExists, "mailbox already exists")
	}
	var attrs []imap.MailboxAttr
	if options != nil {
		for _, attr := range options.SpecialUse {
			switch attr {
			case imap.MailboxAttrAll, imap.MailboxAttrArchive, imap.MailboxAttrDrafts, imap.MailboxAttrFlagged, imap.MailboxAttrJunk, imap.MailboxAttrSent, imap.MailboxAttrTrash:
				attrs = append(attrs, attr)
			default:
				return noError(imap.CodeUseAttr, "unsupported special use")
			}
		}
	}
	next, err := cloneState(s.account.state)
	if err != nil {
		return err
	}
	m, err := s.account.newMailbox()
	if err != nil {
		return err
	}
	m.SpecialUse = attrs
	next.Mailboxes[name] = m
	return s.account.commit(next, mkdirMailbox(name))
}
func (s *session) Delete(ctx context.Context, name string, _ *imapserver.DeleteOptions) error {
	name = mailboxKey(name)
	if name == "INBOX" {
		return noError(imap.CodeCannot, "INBOX cannot be deleted")
	}
	if err := s.enter(ctx); err != nil {
		return err
	}
	defer s.account.leave()
	if s.account.state.Mailboxes[name] == nil {
		return missingMailbox()
	}
	next, err := cloneState(s.account.state)
	if err != nil {
		return err
	}
	delete(next.Mailboxes, name)
	var actions []fileAction
	for _, sub := range []string{"cur", "new", "tmp"} {
		actions = append(actions, fileAction{Kind: "remove", From: filepath.Join(mailboxPath(name), sub)})
	}
	return s.account.commit(next, actions)
}
func (s *session) Rename(ctx context.Context, oldName, newName string, _ *imapserver.RenameOptions) error {
	if err := validMailbox(newName); err != nil {
		return err
	}
	oldName, newName = mailboxKey(oldName), mailboxKey(newName)
	if err := s.enter(ctx); err != nil {
		return err
	}
	defer s.account.leave()
	if s.account.state.Mailboxes[oldName] == nil {
		return missingMailbox()
	}
	if s.account.state.Mailboxes[newName] != nil {
		return noError(imap.CodeAlreadyExists, "destination mailbox already exists")
	}
	if strings.HasPrefix(newName, oldName+"/") {
		return noError(imap.CodeCannot, "cannot rename beneath itself")
	}
	next, err := cloneState(s.account.state)
	if err != nil {
		return err
	}
	var actions []fileAction
	if oldName == "INBOX" {
		// Move messages into a new identity, leaving the selected INBOX and
		// its children in place (RFC 3501 section 6.3.5).
		destination, err := s.account.newMailbox()
		if err != nil {
			return err
		}
		source := next.Mailboxes[oldName]
		next.Mailboxes[newName] = destination
		actions = append(actions, mkdirMailbox(newName)...)
		var removals []imapserver.Update
		for _, original := range source.Messages {
			copy := *original
			copy.UID, err = allocateUID(destination)
			if err != nil {
				return err
			}
			copy.ModSeq, err = bump(destination)
			if err != nil {
				return err
			}
			destination.Messages = append(destination.Messages, &copy)
			ensureFlags(destination, copy.Flags)
			modseq, err := bump(source)
			if err != nil {
				return err
			}
			source.Tombstones = append(source.Tombstones, tombstone{UID: original.UID, ModSeq: modseq})
			removals = append(removals, &imapserver.UpdateExpunge{UID: original.UID})
			actions = append(actions, fileAction{Kind: "rename", From: messagePath(oldName, original), To: messagePath(newName, &copy)})
		}
		source.Messages = nil
		if err := ctx.Err(); err != nil {
			return err
		}
		return s.account.publishCommit(next, actions, map[uint32][]imapserver.Update{source.UIDValidity: removals}, 0)
	} else {
		moved := make(map[string]bool)
		for name := range s.account.state.Mailboxes {
			if name == oldName || strings.HasPrefix(name, oldName+"/") {
				moved[name] = true
			}
		}
		renamed := make(map[string]*mailboxState)
		for name := range moved {
			if name != oldName && !strings.HasPrefix(name, oldName+"/") {
				continue
			}
			target := newName + strings.TrimPrefix(name, oldName)
			if held := next.Mailboxes[target]; held != nil && !moved[target] {
				return noError(imap.CodeAlreadyExists, "destination hierarchy already exists")
			}
			renamed[target] = next.Mailboxes[name]
		}
		for name := range moved {
			delete(next.Mailboxes, name)
		}
		for name, m := range renamed {
			next.Mailboxes[name] = m
		}
		id, err := uniqueName()
		if err != nil {
			return err
		}
		stage := filepath.Join(".staging", "rename-"+id)
		actions = []fileAction{{Kind: "relocate", From: mailboxPath(oldName), To: mailboxPath(newName), Stage: stage}}
	}
	return s.account.commit(next, actions)
}
func (s *session) Subscribe(ctx context.Context, name string, _ *imapserver.SubscribeOptions) error {
	return s.subscribe(ctx, name, true)
}
func (s *session) Unsubscribe(ctx context.Context, name string, _ *imapserver.UnsubscribeOptions) error {
	return s.subscribe(ctx, name, false)
}
func (s *session) subscribe(ctx context.Context, name string, value bool) error {
	if err := validMailbox(name); err != nil {
		return err
	}
	if err := s.enter(ctx); err != nil {
		return err
	}
	defer s.account.leave()
	next, err := cloneState(s.account.state)
	if err != nil {
		return err
	}
	if value {
		next.Subscriptions[mailboxKey(name)] = true
	} else {
		delete(next.Subscriptions, mailboxKey(name))
	}
	return s.account.commit(next, nil)
}

func (s *session) List(ctx context.Context, writer *imapserver.ListWriter, reference string, patterns []string, options *imapserver.ListOptions) error {
	if err := s.enter(ctx); err != nil {
		return err
	}
	defer s.account.leave()
	state := s.account.state
	names := make(map[string]bool)
	for name := range state.Mailboxes {
		names[name] = true
		for parent := name; strings.Contains(parent, "/"); {
			parent = parent[:strings.LastIndex(parent, "/")]
			names[parent] = true
		}
	}
	if options != nil && options.Subscribed {
		for name := range state.Subscriptions {
			names[name] = true
		}
	}
	var results []*imap.ListData
	if slices.Contains(patterns, "") {
		results = append(results, &imap.ListData{Attrs: []imap.MailboxAttr{imap.MailboxAttrNoSelect}, Delimiter: '/', Mailbox: ""})
	}
	for name := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		match := false
		for _, pattern := range patterns {
			if pattern == "" {
				continue
			}
			if !strings.HasPrefix(pattern, "/") {
				pattern = reference + pattern
			}
			if matchesPattern(name, pattern) {
				match = true
				break
			}
		}
		if !match {
			continue
		}
		m := state.Mailboxes[name]
		if options != nil && options.Subscribed && !state.Subscriptions[name] {
			continue
		}
		if options != nil && options.SelectSpecialUse && (m == nil || len(m.SpecialUse) == 0) {
			continue
		}
		data := &imap.ListData{Mailbox: name, Delimiter: '/'}
		if m == nil {
			data.Attrs = append(data.Attrs, imap.MailboxAttrNonExistent)
		} else {
			data.Attrs = append(data.Attrs, m.SpecialUse...)
		}
		if state.Subscriptions[name] {
			data.Attrs = append(data.Attrs, imap.MailboxAttrSubscribed)
		}
		hasChildren := false
		for child := range state.Mailboxes {
			if strings.HasPrefix(child, name+"/") {
				hasChildren = true
				break
			}
		}
		if hasChildren {
			data.Attrs = append(data.Attrs, imap.MailboxAttrHasChildren)
		} else {
			data.Attrs = append(data.Attrs, imap.MailboxAttrHasNoChildren)
		}
		results = append(results, data)
	}
	for _, pattern := range patterns {
		if pattern == "" {
			results = append(results, &imap.ListData{Mailbox: "", Delimiter: '/', Attrs: []imap.MailboxAttr{imap.MailboxAttrNoSelect}})
			break
		}
	}
	slices.SortFunc(results, func(a, b *imap.ListData) int { return strings.Compare(a.Mailbox, b.Mailbox) })
	for _, data := range results {
		if err := writer.WriteList(ctx, data); err != nil {
			return err
		}
	}
	return nil
}
func matchesPattern(name, pattern string) bool {
	if strings.EqualFold(name, "INBOX") && strings.EqualFold(pattern, "INBOX") {
		return true
	}
	n := []rune(name)
	row := make([]bool, len(n)+1)
	row[0] = true
	for _, r := range pattern {
		next := make([]bool, len(n)+1)
		if r == '*' || r == '%' {
			next[0] = row[0]
		}
		for i, c := range n {
			if r == '*' || r == '%' {
				next[i+1] = row[i+1] || (r == '*' || c != '/') && next[i]
			} else {
				next[i+1] = row[i] && r == c
			}
		}
		row = next
	}
	return row[len(n)]
}

func statusData(name string, m *mailboxState) *imap.StatusData {
	data := &imap.StatusData{Mailbox: name, NumMessages: uint32(len(m.Messages)), UIDNext: m.UIDNext, UIDValidity: m.UIDValidity, HighestModSeq: m.HighestModSeq, Values: make(map[imap.StatusItemKeyword]any)}
	var size, deleted uint64
	for _, msg := range m.Messages {
		size += uint64(msg.Size)
		if !imap.ContainsFlag(msg.Flags, imap.FlagSeen) {
			data.NumUnseen++
		}
		if imap.ContainsFlag(msg.Flags, imap.FlagDeleted) {
			deleted++
		}
	}
	data.Values[imap.StatusItemMessages] = uint64(data.NumMessages)
	data.Values[imap.StatusItemUIDNext] = uint64(m.UIDNext)
	data.Values[imap.StatusItemUIDValidity] = uint64(m.UIDValidity)
	data.Values[imap.StatusItemUnseen] = uint64(data.NumUnseen)
	for _, msg := range m.Messages {
		if msg.Recent {
			data.NumRecent++
		}
	}
	data.Values[imap.StatusItemRecent] = uint64(data.NumRecent)
	data.Values[imap.StatusItemHighestModSeq] = m.HighestModSeq
	data.Values[imap.StatusItemSize] = size
	data.Values[imap.StatusItemDeleted] = deleted
	return data
}
func (s *session) Status(ctx context.Context, name string, _ *imapserver.StatusOptions) (*imap.StatusData, error) {
	if err := s.enter(ctx); err != nil {
		return nil, err
	}
	defer s.account.leave()
	name = mailboxKey(name)
	m := s.account.state.Mailboxes[name]
	if m == nil {
		return nil, missingMailbox()
	}
	return statusData(name, m), nil
}
func snapshot(name string, m *mailboxState, readOnly bool) imapserver.SelectSnapshot {
	uids := make([]imap.UID, len(m.Messages))
	var unseen uint32
	for i, msg := range m.Messages {
		uids[i] = msg.UID
		if unseen == 0 && !imap.ContainsFlag(msg.Flags, imap.FlagSeen) {
			unseen = uint32(i + 1)
		}
	}
	flags := slices.Clone(m.Flags)
	permanent := append(slices.Clone(flags), imap.FlagWildcard)
	return imapserver.SelectSnapshot{UIDs: uids, Status: imap.MailboxStatus{Mailbox: name, Flags: flags, PermanentFlags: permanent, NumMessages: uint32(len(uids)), UIDNext: m.UIDNext, UIDValidity: m.UIDValidity, Unseen: unseen, HighestModSeq: m.HighestModSeq, ReadOnly: readOnly}, Revision: revision(m.Revision)}
}
func (s *session) Select(ctx context.Context, name string, updater *imapserver.Updater, options *imapserver.SelectOptions) (*imapserver.SelectResult, error) {
	if updater == nil {
		return nil, noError(imap.CodeClientBug, "nil updater")
	}
	if err := s.enter(ctx); err != nil {
		return nil, err
	}
	defer s.account.leave()
	name = mailboxKey(name)
	m := s.account.state.Mailboxes[name]
	if m == nil {
		return nil, missingMailbox()
	}
	selection := &selected{session: s, id: m.UIDValidity, readOnly: options != nil && options.ReadOnly}
	selection.recent = make(map[imap.UID]bool)
	for _, msg := range m.Messages {
		if msg.Recent {
			selection.recent[msg.UID] = true
		}
	}
	if !selection.readOnly && len(selection.recent) > 0 {
		next, err := cloneState(s.account.state)
		if err != nil {
			return nil, err
		}
		for _, msg := range next.Mailboxes[name].Messages {
			msg.Recent = false
		}
		if err := s.account.commit(next, nil); err != nil {
			return nil, err
		}
		m = s.account.state.Mailboxes[name]
	}
	if s.account.watchers[selection.id] == nil {
		s.account.watchers[selection.id] = make(map[*selected]*imapserver.Updater)
	}
	s.account.watchers[selection.id][selection] = updater
	s.selections[selection] = true
	snap := snapshot(name, m, selection.readOnly)
	snap.Status.NumRecent = uint32(len(selection.recent))
	snap.Status.Flags = append(snap.Status.Flags, imap.FlagRecent)
	return &imapserver.SelectResult{Mailbox: selection, Snapshot: snap}, nil
}
func (s *session) Close(ctx context.Context, _ *imapserver.SessionCloseOptions) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	if err := s.account.acquire(ctx); err != nil {
		return err
	}
	defer s.account.release()
	for sel := range s.selections {
		delete(s.account.watchers[sel.id], sel)
		sel.closed = true
		delete(s.selections, sel)
	}
	s.closed = true
	return nil
}
func (s *selected) Unselect(ctx context.Context, _ *imapserver.UnselectOptions) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	if err := s.session.account.acquire(ctx); err != nil {
		return err
	}
	defer s.session.account.release()
	delete(s.session.account.watchers[s.id], s)
	delete(s.session.selections, s)
	s.closed = true
	return nil
}

func (s *session) Append(ctx context.Context, name string, literal io.Reader, options *imapserver.AppendOptions) (*imap.AppendData, error) {
	if literal == nil {
		return nil, noError(imap.CodeCannot, "nil message literal")
	}
	name = mailboxKey(name)
	// Refused before reading a literal that would only be discarded. The
	// commit below checks again, since the lock is released in between.
	if err := s.enter(ctx); err != nil {
		return nil, err
	}
	exists := s.account.state.Mailboxes[name] != nil
	s.account.leave()
	if !exists {
		return nil, noError(imap.CodeTryCreate, "destination mailbox does not exist")
	}
	date := time.Now()
	var flags []imap.Flag
	var origin imapserver.ChangeToken
	if options != nil {
		flags = uniqueFlags(options.Flags)
		origin = options.Origin
		if !options.InternalDate.IsZero() {
			date = options.InternalDate
		}
	}
	// The literal is staged before the account lock is taken. Reading it waits
	// on the client, and holding the lock meanwhile stalled every operation on
	// the account, delivery polling included, for as long as the client took.
	// A staged file has a unique name, no journal refers to it until the commit
	// below, and Open sweeps .staging if the process dies first.
	if err := s.account.backend.begin(ctx); err != nil {
		return nil, err
	}
	defer s.account.backend.ops.Done()
	stage, size, actualDate, err := s.account.stage(ctx, literal, date, false)
	if err != nil {
		return nil, err
	}
	// Already counted as an operation, so Close waits for this commit rather
	// than refusing it; only the lock remains to be taken.
	if err := s.account.acquire(ctx); err != nil {
		_ = s.account.root.Remove(stage)
		return nil, err
	}
	defer s.account.release()
	defer s.account.discardStage(stage)
	if s.account.failure != nil {
		return nil, noError(imap.CodeUnavailable, "account requires recovery; reopen the backend")
	}
	if s.closed {
		return nil, noError(imap.CodeClosed, "session is closed")
	}
	if s.account.state.Mailboxes[name] == nil {
		return nil, noError(imap.CodeTryCreate, "destination mailbox does not exist")
	}
	next, err := cloneState(s.account.state)
	if err != nil {
		return nil, err
	}
	m := next.Mailboxes[name]
	uid, err := allocateUID(m)
	if err != nil {
		return nil, err
	}
	modseq, err := bump(m)
	if err != nil {
		return nil, err
	}
	file := filepath.Base(stage) + flagSuffix(flags)
	msg := &messageRecord{Recent: true, UID: uid, File: file, Size: size, Date: actualDate, Flags: flags, ModSeq: modseq}
	m.Messages = append(m.Messages, msg)
	updates := []imapserver.Update{&imapserver.UpdateAdd{UIDs: []imap.UID{uid}}}
	if ensureFlags(m, flags) {
		updates = withMailboxFlags(updates, m.Flags)
	}
	actions := []fileAction{{Kind: "rename", From: stage, To: messagePath(name, msg)}}
	if err := s.account.publishCommit(next, actions, map[uint32][]imapserver.Update{m.UIDValidity: updates}, origin); err != nil {
		return nil, err
	}
	return &imap.AppendData{HasUID: true, UID: uid, UIDValidity: m.UIDValidity}, nil
}
func (s *session) Namespace(ctx context.Context, _ *imapserver.NamespaceOptions) (*imap.NamespaceData, error) {
	if err := s.enter(ctx); err != nil {
		return nil, err
	}
	defer s.account.leave()
	return &imap.NamespaceData{Personal: []imap.NamespaceDescriptor{{Prefix: "", Delimiter: '/'}}}, nil
}

var _ imapserver.Session = (*session)(nil)
var _ imapserver.NamespaceSession = (*session)(nil)
