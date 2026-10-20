package memory

import (
	"bytes"
	"context"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
	servermessage "github.com/kiliant/go-imap/imapserver/message"
)

type session struct {
	backend *Backend
	account *account
	// username is the authenticated identity, which ACL entries are keyed by.
	// See ext_d.go.
	username   string
	selections map[*selected]struct{}
	closed     bool
	// notify and notifyConfig are the session's NOTIFY registration.
	// See ext_d.go.
	notify       *imapserver.SessionUpdater
	notifyConfig *imapserver.NotifyConfig
	// comparator is the active RFC 5255 collation. See ext_e.go.
	comparator string
}

func (s *session) List(ctx context.Context, writer *imapserver.ListWriter, reference string, patterns []string, options *imapserver.ListOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.account.mu.Lock()
	if s.closed {
		s.account.mu.Unlock()
		return noError(imap.CodeClosed, "session is closed")
	}
	var results []*imap.ListData
	if options != nil && options.Subscribed && options.SelectRecursiveMatch {
		results = s.listRecursiveSubscribedLocked(reference, patterns)
	} else {
		for _, m := range s.account.mailboxes {
			if options != nil && options.Subscribed && !m.subscribed {
				continue
			}
			if options != nil && options.SelectSpecialUse && len(m.specialUse) == 0 {
				continue
			}
			if !matchesAnyMailbox(m.name, reference, patterns) {
				continue
			}
			results = append(results, &imap.ListData{Attrs: s.mailboxAttrsLocked(m), Delimiter: '/', Mailbox: m.name})
		}
	}
	if slices.Contains(patterns, "") {
		results = append(results, &imap.ListData{Attrs: []imap.MailboxAttr{imap.MailboxAttrNoSelect}, Delimiter: '/', Mailbox: ""})
	}
	s.account.mu.Unlock()
	slices.SortFunc(results, func(a, b *imap.ListData) int { return strings.Compare(a.Mailbox, b.Mailbox) })
	for _, result := range results {
		if err := writer.WriteList(ctx, result); err != nil {
			return err
		}
	}
	return nil
}

func matchesAnyMailbox(name, reference string, patterns []string) bool {
	if len(patterns) == 0 {
		return false
	}
	for _, pattern := range patterns {
		if pattern == "" {
			continue
		}
		resolved := pattern
		if reference != "" && !strings.HasPrefix(pattern, "/") {
			resolved = reference + pattern
		}
		if matchesMailboxPattern(name, resolved) {
			return true
		}
	}
	return false
}

func matchesMailboxPattern(name, pattern string) bool {
	nameRunes, patternRunes := []rune(name), []rune(pattern)
	type state struct{ name, pattern int }
	memo := make(map[state]bool)
	seen := make(map[state]bool)
	var match func(int, int) bool
	match = func(nameAt, patternAt int) bool {
		key := state{name: nameAt, pattern: patternAt}
		if seen[key] {
			return memo[key]
		}
		seen[key] = true
		var ok bool
		switch {
		case patternAt == len(patternRunes):
			ok = nameAt == len(nameRunes)
		case patternRunes[patternAt] == '*':
			ok = match(nameAt, patternAt+1) || nameAt < len(nameRunes) && match(nameAt+1, patternAt)
		case patternRunes[patternAt] == '%':
			ok = match(nameAt, patternAt+1) || nameAt < len(nameRunes) && nameRunes[nameAt] != '/' && match(nameAt+1, patternAt)
		default:
			ok = nameAt < len(nameRunes) && nameRunes[nameAt] == patternRunes[patternAt] && match(nameAt+1, patternAt+1)
		}
		memo[key] = ok
		return ok
	}
	return match(0, 0)
}

func (s *session) Status(ctx context.Context, name string, options *imapserver.StatusOptions) (*imap.StatusData, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.account.mu.Lock()
	if s.closed {
		s.account.mu.Unlock()
		return nil, noError(imap.CodeClosed, "session is closed")
	}
	m := s.account.mailboxes[mailboxKey(name)]
	if m == nil {
		s.account.mu.Unlock()
		return nil, nonexistentMailbox(name)
	}
	data := statusDataLocked(m, options)
	s.account.mu.Unlock()
	return data, nil
}

func (s *session) Create(ctx context.Context, name string, options *imapserver.CreateOptions) error {
	name = strings.TrimSuffix(name, "/")
	if err := ctx.Err(); err != nil {
		return err
	}
	if name == "" {
		return noError(imap.CodeCannot, "empty mailbox name")
	}
	s.account.mu.Lock()
	defer s.account.mu.Unlock()
	if s.closed {
		return noError(imap.CodeClosed, "session is closed")
	}
	key := mailboxKey(name)
	if s.account.mailboxes[key] != nil {
		return noError(imap.CodeAlreadyExists, "mailbox already exists")
	}
	s.account.createMailboxLocked(name)
	// The USE parameter of CREATE-SPECIAL-USE. A rejected attribute must not
	// leave a mailbox behind, since the client asked for a mailbox with that
	// use and would otherwise be told no while one exists. See ext_a.go.
	if err := s.applyCreateSpecialUseLocked(s.account.mailboxes[key], options); err != nil {
		delete(s.account.mailboxes, key)
		return err
	}
	return nil
}

func (s *session) Delete(ctx context.Context, name string, _ *imapserver.DeleteOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	key := mailboxKey(name)
	if key == "INBOX" {
		return noError(imap.CodeCannot, "INBOX cannot be deleted")
	}
	s.account.mu.Lock()
	defer s.account.mu.Unlock()
	if s.closed {
		return noError(imap.CodeClosed, "session is closed")
	}
	m := s.account.mailboxes[key]
	if m == nil {
		return nonexistentMailbox(name)
	}
	if len(m.watchers) != 0 {
		return noError(imap.CodeInUse, "mailbox is selected")
	}
	delete(s.account.mailboxes, key)
	delete(s.account.urlAuthKeys, key)
	return nil
}

func (s *session) Rename(ctx context.Context, oldName, newName string, _ *imapserver.RenameOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if newName == "" {
		return noError(imap.CodeCannot, "mailbox cannot be renamed")
	}
	s.account.mu.Lock()
	defer s.account.mu.Unlock()
	if s.closed {
		return noError(imap.CodeClosed, "session is closed")
	}
	if mailboxKey(oldName) == "INBOX" {
		return s.renameInboxLocked(newName)
	}
	oldKey := mailboxKey(oldName)
	m := s.account.mailboxes[oldKey]
	if m == nil {
		return nonexistentMailbox(oldName)
	}
	// RFC 3501 section 6.3.5: inferiors move with the mailbox. Plan the whole
	// hierarchy and check every destination before renaming anything, so a
	// conflict leaves no partial rename; identities move with their mailboxes.
	if strings.HasPrefix(newName, m.name+"/") {
		return noError(imap.CodeCannot, "a mailbox cannot be renamed into its own hierarchy")
	}
	moving := map[string]string{oldKey: newName}
	for key, child := range s.account.mailboxes {
		if suffix, ok := strings.CutPrefix(child.name, m.name+"/"); ok {
			moving[key] = newName + "/" + suffix
		}
	}
	for _, target := range moving {
		if _, ok := moving[mailboxKey(target)]; !ok && s.account.mailboxes[mailboxKey(target)] != nil {
			return noError(imap.CodeAlreadyExists, "destination mailbox already exists")
		}
	}
	moved := make(map[string]*mailbox, len(moving))
	for key := range moving {
		moved[key] = s.account.mailboxes[key]
		delete(s.account.mailboxes, key)
		delete(s.account.urlAuthKeys, key)
	}
	for key, target := range moving {
		moved[key].name = target
		s.account.mailboxes[mailboxKey(target)] = moved[key]
	}
	return nil
}

// renameInboxLocked is RFC 3501 section 6.3.5's special case: INBOX's
// messages move to a new mailbox, and INBOX itself, with its inferiors,
// identity, subscription and metadata, stays where it is, empty.
//
// The new mailbox is created as CREATE would create it, so it has its own
// UIDVALIDITY, MAILBOXID and UIDs. Each message keeps its flags, INTERNALDATE and
// content, and with the content its EMAILID; in its new mailbox it is recent,
// as a copied message is. INBOX records each removal at a new modification
// sequence for QRESYNC, and its selections see the removals like an EXPUNGE.
// Every check precedes the first change, all under the account lock, so a
// refusal changes nothing and no session observes half a rename.
//
// The caller must hold the account lock.
func (s *session) renameInboxLocked(newName string) error {
	inbox := s.account.mailboxes["INBOX"]
	if inbox == nil {
		return nonexistentMailbox("INBOX")
	}
	if s.account.mailboxes[mailboxKey(newName)] != nil {
		return noError(imap.CodeAlreadyExists, "destination mailbox already exists")
	}
	// INBOX remains, so a name beneath it would be one of its own inferiors.
	// Refused, as the Maildir backend does, rather than created below the
	// mailbox it is being emptied out of.
	if strings.HasPrefix(newName, inbox.name+"/") {
		return noError(imap.CodeCannot, "INBOX cannot be renamed into its own hierarchy")
	}
	destination := s.account.createMailboxLocked(newName)
	var added []imap.UID
	var removals []imapserver.Update
	for _, msg := range inbox.messages {
		clone := *msg
		clone.uid = destination.uidNext
		clone.saveDate = time.Now()
		destination.uidNext++
		clone.recent = true
		clone.flags = cloneFlags(msg.flags)
		clone.modSeq = bumpModSeqLocked(destination)
		destination.messages = append(destination.messages, &clone)
		ensureMailboxFlagsLocked(destination, clone.flags)
		added = append(added, clone.uid)
		recordVanishedLocked(inbox, msg.uid)
		removals = append(removals, &imapserver.UpdateExpunge{UID: msg.uid})
	}
	inbox.messages = nil
	if len(removals) != 0 {
		publishLocked(inbox, advanceLocked(inbox, 0, removals))
		publishLocked(destination, advanceLocked(destination, 0, []imapserver.Update{&imapserver.UpdateAdd{UIDs: added}}))
	}
	return nil
}

func (s *session) Subscribe(ctx context.Context, name string, _ *imapserver.SubscribeOptions) error {
	return s.setSubscribed(ctx, name, true)
}

func (s *session) Unsubscribe(ctx context.Context, name string, _ *imapserver.UnsubscribeOptions) error {
	return s.setSubscribed(ctx, name, false)
}

func (s *session) setSubscribed(ctx context.Context, name string, subscribed bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.account.mu.Lock()
	defer s.account.mu.Unlock()
	if s.closed {
		return noError(imap.CodeClosed, "session is closed")
	}
	m := s.account.mailboxes[mailboxKey(name)]
	if m == nil {
		return nonexistentMailbox(name)
	}
	m.subscribed = subscribed
	return nil
}

func (s *session) Append(ctx context.Context, name string, literal io.Reader, options *imapserver.AppendOptions) (*imap.AppendData, error) {
	if literal == nil {
		return nil, noError(imap.CodeCannot, "nil message literal")
	}
	raw, err := io.ReadAll(&contextReader{ctx: ctx, reader: literal})
	if err != nil {
		return nil, err
	}
	analysis, err := servermessage.Analyze(ctx, bytes.NewReader(raw), int64(len(raw)), nil)
	if err != nil {
		return nil, err
	}
	internalDate := time.Now()
	var flags []imap.Flag
	var origin imapserver.ChangeToken
	if options != nil {
		flags = uniqueFlags(options.Flags)
		origin = options.Origin
		if !options.InternalDate.IsZero() {
			internalDate = options.InternalDate
		}
	}

	s.account.mu.Lock()
	if s.closed {
		s.account.mu.Unlock()
		return nil, noError(imap.CodeClosed, "session is closed")
	}
	m := s.account.mailboxes[mailboxKey(name)]
	if m == nil {
		s.account.mu.Unlock()
		return nil, noError(imap.CodeTryCreate, "destination mailbox does not exist")
	}
	uid := m.uidNext
	m.uidNext++
	m.messages = append(m.messages, &message{
		recent: true, uid: uid, flags: flags, internalDate: internalDate, raw: raw, analysis: analysis,
		modSeq: bumpModSeqLocked(m), saveDate: time.Now(),
	})
	changes := []imapserver.Update{&imapserver.UpdateAdd{UIDs: []imap.UID{uid}}}
	if ensureMailboxFlagsLocked(m, flags) {
		changes = append([]imapserver.Update{&imapserver.UpdateMailboxFlags{Flags: mailboxFlagsLocked(m)}}, changes...)
	}
	batch := advanceLocked(m, origin, changes)
	publishLocked(m, batch)
	// NOTIFY watchers hear about a mailbox they have not selected.
	uidValidity := m.uidValidity
	s.account.mu.Unlock()
	return &imap.AppendData{HasUID: true, UIDValidity: uidValidity, UID: uid}, nil
}

func (s *session) Select(ctx context.Context, name string, updater *imapserver.Updater, options *imapserver.SelectOptions) (*imapserver.SelectResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if updater == nil {
		return nil, noError(imap.CodeClientBug, "nil updater")
	}
	s.account.mu.Lock()
	if s.closed {
		s.account.mu.Unlock()
		return nil, noError(imap.CodeClosed, "session is closed")
	}
	m := s.account.mailboxes[mailboxKey(name)]
	if m == nil {
		s.account.mu.Unlock()
		return nil, nonexistentMailbox(name)
	}
	selected := &selected{session: s, mailbox: m, readOnly: options != nil && options.ReadOnly}
	m.watchers[selected] = updater
	if s.account.selectFailure[mailboxKey(name)] {
		delete(m.watchers, selected) // detach on every failed path
		s.account.mu.Unlock()
		return nil, noError(imap.CodeServerBug, "forced selection failure")
	}
	s.selections[selected] = struct{}{}
	selected.recent = make(map[imap.UID]bool)
	for _, msg := range m.messages {
		if msg.recent {
			selected.recent[msg.uid] = true
			if !selected.readOnly {
				msg.recent = false
			}
		}
	}
	snapshot := snapshotLocked(m, selected.readOnly)
	snapshot.Status.NumRecent = uint32(len(selected.recent))
	snapshot.Status.Flags = append(snapshot.Status.Flags, imap.FlagRecent)
	s.account.mu.Unlock()
	return &imapserver.SelectResult{Mailbox: selected, Snapshot: snapshot}, nil
}

func (s *session) Close(ctx context.Context, _ *imapserver.SessionCloseOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.account.mu.Lock()
	defer s.account.mu.Unlock()
	if s.closed {
		return nil
	}
	for selected := range s.selections {
		selected.closed = true
		delete(selected.mailbox.watchers, selected)
		delete(s.selections, selected)
	}
	s.closed = true
	s.notify, s.notifyConfig = nil, nil
	delete(s.account.sessions, s)
	return nil
}

func statusDataLocked(m *mailbox, options *imapserver.StatusOptions) *imap.StatusData {
	var unseen uint32
	for _, msg := range m.messages {
		if !imap.ContainsFlag(msg.flags, imap.FlagSeen) {
			unseen++
		}
	}
	data := &imap.StatusData{
		Mailbox:     m.name,
		NumMessages: uint32(len(m.messages)),
		UIDNext:     m.uidNext,
		UIDValidity: m.uidValidity,
		NumUnseen:   unseen,
		Values:      make(map[imap.StatusItemKeyword]any),
	}
	for _, msg := range m.messages {
		if msg.recent {
			data.NumRecent++
		}
	}
	items := []imap.StatusItem{
		imap.StatusItemMessages,
		imap.StatusItemUIDNext,
		imap.StatusItemUIDValidity,
		imap.StatusItemUnseen,
		imap.StatusItemRecent,
	}
	if options != nil && len(options.Items) != 0 {
		items = options.Items
	}
	for _, item := range items {
		keyword, ok := item.(imap.StatusItemKeyword)
		if !ok {
			continue
		}
		switch keyword {
		case imap.StatusItemMessages:
			data.Values[keyword] = uint64(data.NumMessages)
		case imap.StatusItemUIDNext:
			data.Values[keyword] = uint64(data.UIDNext)
		case imap.StatusItemUIDValidity:
			data.Values[keyword] = uint64(data.UIDValidity)
		case imap.StatusItemUnseen:
			data.Values[keyword] = uint64(data.NumUnseen)
		case imap.StatusItemRecent:
			data.Values[keyword] = uint64(data.NumRecent)
		case imap.StatusItemSize:
			// STATUS=SIZE is the total octet size of the mailbox.
			// RFC 8438 section 3.
			var size uint64
			for _, msg := range m.messages {
				size += uint64(len(msg.raw))
			}
			data.Values[keyword] = size
		case imap.StatusItemMailboxID:
			data.Values[keyword] = mailboxID(m)
		case imap.StatusItemAppendLimit:
			data.Values[keyword] = uint64(appendLimit)
		case imap.StatusItemHighestModSeq:
			data.Values[keyword] = m.highestModSeq
		case imap.StatusItemDeletedStorage:
			var size uint64
			for _, msg := range m.messages {
				if imap.ContainsFlag(msg.flags, imap.FlagDeleted) {
					size += uint64(len(msg.raw))
				}
			}
			data.Values[keyword] = size
		case imap.StatusItemDeleted:
			var deleted uint64
			for _, msg := range m.messages {
				if imap.ContainsFlag(msg.flags, imap.FlagDeleted) {
					deleted++
				}
			}
			data.Values[keyword] = deleted
		}
	}
	return data
}

func snapshotLocked(m *mailbox, readOnly bool) imapserver.SelectSnapshot {
	uids := make([]imap.UID, len(m.messages))
	var unseen uint32
	for i, msg := range m.messages {
		uids[i] = msg.uid
		if unseen == 0 && !imap.ContainsFlag(msg.flags, imap.FlagSeen) {
			unseen = uint32(i + 1)
		}
	}
	flags := mailboxFlagsLocked(m)
	return imapserver.SelectSnapshot{
		UIDs: uids,
		Status: imap.MailboxStatus{
			Mailbox:        m.name,
			Flags:          cloneFlags(flags),
			PermanentFlags: append(cloneFlags(flags), imap.FlagWildcard),
			NumMessages:    uint32(len(m.messages)),
			UIDNext:        m.uidNext,
			UIDValidity:    m.uidValidity,
			Unseen:         unseen,
			HighestModSeq:  m.highestModSeq,
			ReadOnly:       readOnly,
		},
		Revision: revision(m.revision),
	}
}

// mailboxFlagsLocked returns every flag currently applicable to the mailbox.
// The caller must hold the account lock.
func mailboxFlagsLocked(m *mailbox) []imap.Flag {
	return slices.Clone(m.flags)
}

func defaultMailboxFlags() []imap.Flag {
	return []imap.Flag{imap.FlagSeen, imap.FlagAnswered, imap.FlagFlagged, imap.FlagDeleted, imap.FlagDraft}
}

// ensureMailboxFlagsLocked records newly applicable keywords. The caller must
// hold the account lock. Applicable keywords are deliberately monotonic for a
// mailbox: removing the last current reference does not make a keyword invalid.
func ensureMailboxFlagsLocked(m *mailbox, flags []imap.Flag) bool {
	changed := false
	for _, flag := range flags {
		if !imap.ContainsFlag(m.flags, flag) {
			m.flags = append(m.flags, flag)
			changed = true
		}
	}
	if changed {
		slices.SortFunc(m.flags[5:], func(a, b imap.Flag) int {
			return strings.Compare(strings.ToLower(string(a)), strings.ToLower(string(b)))
		})
	}
	return changed
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

var _ imapserver.Session = (*session)(nil)
