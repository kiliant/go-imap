package memory

import (
	"context"
	"slices"
	"strings"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
)

// Group D support: QUOTA and QUOTASET (RFC 9208), ACL (RFC 4314), METADATA
// (RFC 5464), NAMESPACE (RFC 2342) and UNAUTHENTICATE (RFC 8437).
//
// These are per-account state, so they live on the account rather than on a
// mailbox, and are guarded by the same lock as everything else.

// quotaRootName is the single quota root this backend presents. A real server
// would have several, but the protocol shape — a mailbox maps to zero or more
// named roots, and each root carries resources — is exercised by one.
const quotaRootName = "root"

// QuotaRoots implements [imapserver.QuotaSession].
func (s *session) QuotaRoots(ctx context.Context, name string, _ *imapserver.QuotaOptions) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.account.mu.Lock()
	defer s.account.mu.Unlock()
	if s.account.mailboxes[mailboxKey(name)] == nil {
		return nil, nonexistentMailbox(name)
	}
	return []string{quotaRootName}, nil
}

// GetQuota implements [imapserver.QuotaSession].
func (s *session) GetQuota(ctx context.Context, root string, _ *imapserver.QuotaOptions) (*imap.QuotaData, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if root != quotaRootName {
		return nil, noError(imap.CodeNonExistent, "no such quota root")
	}
	s.account.mu.Lock()
	defer s.account.mu.Unlock()
	var storage, messages uint64
	for _, m := range s.account.mailboxes {
		messages += uint64(len(m.messages))
		for _, msg := range m.messages {
			storage += uint64(len(msg.raw))
		}
	}
	// RFC 9208 section 5 measures STORAGE in kibibytes, rounded up, and MESSAGE
	// as a plain count.
	return &imap.QuotaData{
		Root: root,
		Resources: []imap.QuotaResource{
			{Name: imap.QuotaResourceStorage, Usage: (storage + 1023) / 1024, Limit: s.account.quotaStorage},
			{Name: imap.QuotaResourceMessage, Usage: messages, Limit: s.account.quotaMessages},
		},
	}, nil
}

// SetQuota implements [imapserver.QuotaSetSession].
func (s *session) SetQuota(ctx context.Context, root string, limits []imap.QuotaResourceLimit, _ *imapserver.QuotaOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if root != quotaRootName {
		return noError(imap.CodeNonExistent, "no such quota root")
	}
	s.account.mu.Lock()
	defer s.account.mu.Unlock()
	for _, limit := range limits {
		switch limit.Name {
		case imap.QuotaResourceStorage:
			s.account.quotaStorage = limit.Limit
		case imap.QuotaResourceMessage:
			s.account.quotaMessages = limit.Limit
		default:
			// RFC 9208 section 4.1.3: a resource the server does not support is
			// refused rather than silently ignored, or the client believes it
			// set a limit that does not exist.
			return &imap.Error{
				Type: imap.ErrorTypeNo,
				Code: imap.CodeCannot,
				Text: "unsupported quota resource " + string(limit.Name),
			}
		}
	}
	return nil
}

// GetACL implements [imapserver.ACLSession].
func (s *session) GetACL(ctx context.Context, name string, _ *imapserver.ACLOptions) (*imap.ACLData, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.account.mu.Lock()
	defer s.account.mu.Unlock()
	m := s.account.mailboxes[mailboxKey(name)]
	if m == nil {
		return nil, nonexistentMailbox(name)
	}
	entries := make([]imap.ACLEntry, 0, len(m.acl))
	for identifier, rights := range m.acl {
		entries = append(entries, imap.ACLEntry{Identifier: identifier, Rights: rights})
	}
	slices.SortFunc(entries, func(a, b imap.ACLEntry) int {
		return strings.Compare(a.Identifier, b.Identifier)
	})
	return &imap.ACLData{Mailbox: name, Entries: entries}, nil
}

// MyRights implements [imapserver.ACLSession].
func (s *session) MyRights(ctx context.Context, name string, _ *imapserver.ACLOptions) (imap.ACLRights, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	s.account.mu.Lock()
	defer s.account.mu.Unlock()
	m := s.account.mailboxes[mailboxKey(name)]
	if m == nil {
		return "", nonexistentMailbox(name)
	}
	if rights, ok := m.acl[s.username]; ok {
		return rights, nil
	}
	// The account owner holds every right on their own mailboxes unless an
	// entry says otherwise.
	return ownerRights, nil
}

// ListRights implements [imapserver.ACLSession].
func (s *session) ListRights(ctx context.Context, name, identifier string, _ *imapserver.ACLOptions) (*imap.ListRightsData, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.account.mu.Lock()
	defer s.account.mu.Unlock()
	if s.account.mailboxes[mailboxKey(name)] == nil {
		return nil, nonexistentMailbox(name)
	}
	// RFC 4314 section 3.7: Required is always granted, and each Optional entry
	// is a group that may be granted separately. Here every right beyond "l" is
	// individually grantable.
	optional := make([]imap.ACLRights, 0, len(ownerRights)-1)
	for _, right := range ownerRights {
		if right == 'l' {
			continue
		}
		optional = append(optional, imap.ACLRights(string(right)))
	}
	return &imap.ListRightsData{
		Mailbox:    name,
		Identifier: identifier,
		Required:   "l",
		Optional:   optional,
	}, nil
}

// SetACL implements [imapserver.ACLSetSession].
func (s *session) SetACL(ctx context.Context, name, identifier string, rights imap.ACLRights, options *imapserver.ACLSetOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.account.mu.Lock()
	defer s.account.mu.Unlock()
	m := s.account.mailboxes[mailboxKey(name)]
	if m == nil {
		return nonexistentMailbox(name)
	}
	if m.acl == nil {
		m.acl = make(map[string]imap.ACLRights)
	}
	op := imapserver.ACLRightsSet
	if options != nil {
		op = options.Op
	}
	current := m.acl[identifier]
	if _, ok := m.acl[identifier]; !ok && identifier == s.username {
		current = ownerRights
	}
	switch op {
	case imapserver.ACLRightsAdd:
		m.acl[identifier] = mergeRights(current, rights, true)
	case imapserver.ACLRightsRemove:
		m.acl[identifier] = mergeRights(current, rights, false)
	default:
		m.acl[identifier] = imap.ACLRights(sortedRights(string(rights)))
	}
	return nil
}

// DeleteACL implements [imapserver.ACLSetSession].
//
// Removing an identifier's entry is not the same as granting it no rights: the
// entry's absence means "no explicit entry", which is what RFC 4314 section 3.2
// distinguishes.
func (s *session) DeleteACL(ctx context.Context, name, identifier string, _ *imapserver.ACLOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.account.mu.Lock()
	defer s.account.mu.Unlock()
	m := s.account.mailboxes[mailboxKey(name)]
	if m == nil {
		return nonexistentMailbox(name)
	}
	delete(m.acl, identifier)
	return nil
}

// ownerRights is the full RFC 4314 section 2.1 rights set.
const ownerRights imap.ACLRights = "acdeilprstwx"

func mergeRights(current, delta imap.ACLRights, add bool) imap.ACLRights {
	held := make(map[rune]bool, len(current))
	for _, right := range current {
		held[right] = true
	}
	for _, right := range delta {
		if add {
			held[right] = true
		} else {
			delete(held, right)
		}
	}
	var merged []rune
	for right := range held {
		merged = append(merged, right)
	}
	slices.Sort(merged)
	return imap.ACLRights(merged)
}

func sortedRights(rights string) string {
	runes := []rune(rights)
	slices.Sort(runes)
	return string(slices.Compact(runes))
}

// GetMetadata implements [imapserver.MetadataSession]. An empty mailbox name
// addresses the server annotation space.
func (s *session) GetMetadata(ctx context.Context, name string, entries []imap.MetadataEntryName, options *imapserver.MetadataOptions) (*imap.MailboxMetadata, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.account.mu.Lock()
	defer s.account.mu.Unlock()
	store, err := s.metadataStoreLocked(name, false)
	if err != nil {
		return nil, err
	}
	maxSize, hasMaxSize := uint32(0), false
	depth := ""
	if options != nil {
		maxSize, hasMaxSize, depth = options.MaxSize, options.HasMaxSize, options.Depth
	}
	var reported []imap.MetadataEntry
	for _, requested := range entries {
		for stored, value := range store {
			if !metadataMatches(stored, requested, depth) {
				continue
			}
			// RFC 5464 section 4.2.2: a value beyond MAXSIZE is omitted, not
			// truncated, because a truncated annotation is not the annotation.
			if hasMaxSize && uint32(len(value)) > maxSize {
				continue
			}
			held := value
			reported = append(reported, imap.MetadataEntry{Name: stored, Value: &held})
		}
	}
	slices.SortFunc(reported, func(a, b imap.MetadataEntry) int {
		return strings.Compare(string(a.Name), string(b.Name))
	})
	return &imap.MailboxMetadata{Mailbox: name, Entries: reported}, nil
}

// SetMetadata implements [imapserver.MetadataSession].
func (s *session) SetMetadata(ctx context.Context, name string, entries []imap.MetadataEntry, _ *imapserver.MetadataOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.account.mu.Lock()
	defer s.account.mu.Unlock()
	store, err := s.metadataStoreLocked(name, true)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		// A nil value removes the entry; an empty string stores an empty
		// value. RFC 5464 section 4.3 makes those different operations.
		if entry.Value == nil {
			delete(store, entry.Name)
			continue
		}
		store[entry.Name] = *entry.Value
	}
	return nil
}

// metadataStoreLocked returns the annotation map for a mailbox, or the
// server-scope map when name is empty.
//
// The caller must hold the account lock.
func (s *session) metadataStoreLocked(name string, create bool) (map[imap.MetadataEntryName]string, error) {
	if name == "" {
		if s.account.metadata == nil && create {
			s.account.metadata = make(map[imap.MetadataEntryName]string)
		}
		return s.account.metadata, nil
	}
	m := s.account.mailboxes[mailboxKey(name)]
	if m == nil {
		return nil, nonexistentMailbox(name)
	}
	if m.metadata == nil && create {
		m.metadata = make(map[imap.MetadataEntryName]string)
	}
	return m.metadata, nil
}

// metadataMatches applies RFC 5464 section 4.2.2's DEPTH option: "0" matches
// only the entry itself, "1" its immediate children, "infinity" the whole
// subtree.
func metadataMatches(stored, requested imap.MetadataEntryName, depth string) bool {
	if stored == requested {
		return true
	}
	prefix := string(requested)
	if !strings.HasPrefix(string(stored), prefix+"/") {
		return false
	}
	switch depth {
	case "infinity":
		return true
	case "1":
		return !strings.Contains(strings.TrimPrefix(string(stored), prefix+"/"), "/")
	default:
		return false
	}
}

// Namespace implements [imapserver.NamespaceSession].
func (s *session) Namespace(ctx context.Context, _ *imapserver.NamespaceOptions) (*imap.NamespaceData, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &imap.NamespaceData{
		Personal: []imap.NamespaceDescriptor{{Prefix: "", Delimiter: '/'}},
	}, nil
}

// Unauthenticate implements [imapserver.UnauthenticateSession].
//
// The framework closes the session afterwards, so there is nothing to release
// here; this backend has no per-user resource that outlives the session.
func (s *session) Unauthenticate(ctx context.Context, _ *imapserver.UnauthenticateOptions) error {
	return ctx.Err()
}

// MessageLimits implements [imapserver.MessageLimitSession].
//
// RFC 9738 puts the value inside the advertised capability token. Presence is
// carried by the Has fields, so a backend can enforce one limit and not the
// other, and a genuine limit of zero stays expressible.
func (s *session) MessageLimits(ctx context.Context, _ *imapserver.MessageLimitOptions) (*imapserver.MessageLimits, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &imapserver.MessageLimits{
		MessageLimit:    memoryMessageLimit,
		HasMessageLimit: true,
		SaveLimit:       memorySaveLimit,
		HasSaveLimit:    true,
	}, nil
}

// The reference backend keeps everything in memory, so these are modest on
// purpose: a limit a test can actually reach is worth more than a large one
// that never fires.
const (
	memoryMessageLimit = 10_000
	memorySaveLimit    = 1_000
)

// Notify implements [imapserver.NotifySession].
//
// The registration is recorded on the session and every mutating path publishes
// through it. A nil config is NOTIFY NONE, which drops the registration — the
// framework has already closed the previous updater by then, so nothing else is
// needed to stop delivery.
func (s *session) Notify(ctx context.Context, updater *imapserver.SessionUpdater, config *imapserver.NotifyConfig, _ *imapserver.NotifyOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateNotifyWatches(config); err != nil {
		return err
	}
	s.account.mu.Lock()
	defer s.account.mu.Unlock()
	s.notify, s.notifyConfig = updater, config
	if config == nil {
		return nil
	}
	// STATUS on SET reports every watched mailbox immediately, so a client that
	// has just registered does not have to poll once to learn where it stands.
	// RFC 5465 section 6.
	if config.StatusOnSet {
		for _, m := range s.account.mailboxes {
			if !s.notifyWatchesLocked(m.name) {
				continue
			}
			_ = updater.Push(&imapserver.SessionUpdate{
				Mailbox: m.name,
				Status:  statusDataLocked(m, nil),
			})
		}
	}
	return nil
}

// notifySupportedEvents are the events this backend actually publishes. The
// metadata events of RFC 5464 are absent because nothing here changes metadata
// in a way that would raise them.
var notifySupportedEvents = []imap.NotifyEventName{
	imap.NotifyEventMessageNew,
	imap.NotifyEventMessageExpunge,
	imap.NotifyEventFlagChange,
	imap.NotifyEventMailboxName,
	imap.NotifyEventSubscriptionChange,
}

// validateNotifyWatches refuses a registration this backend cannot honour.
//
// Accepting an unknown specifier or event and then never delivering it is the
// failure mode NOTIFY makes easy: the client is told SET succeeded and reads the
// resulting silence as "nothing has changed", so a watch that never matches is
// indistinguishable from a quiet mailbox. RFC 5465 section 6 defines BADEVENT
// for this, and a backend has to raise it — the framework cannot, because only
// the backend knows what it can publish.
func validateNotifyWatches(config *imapserver.NotifyConfig) error {
	if config == nil {
		return nil
	}
	for _, watch := range config.Watches {
		switch watch.Specifier {
		case imap.NotifySelected, imap.NotifySelectedDelayed, imap.NotifyPersonal,
			imap.NotifySubscribed, imap.NotifyInboxes, imap.NotifySubtree, imap.NotifyMailboxes:
		default:
			// BAD, not NO [BADEVENT]. RFC 5465 section 6 enumerates the mailbox
			// specifiers in the grammar, so an unrecognised one is a malformed
			// command rather than a request for something the server declines to
			// do. BADEVENT is defined for the event list, which is an extensible
			// registry — the distinction tells a client whether to retry with a
			// smaller request or to stop sending that syntax at all.
			return &imap.Error{
				Type: imap.ErrorTypeBad,
				Text: "unknown NOTIFY mailbox specifier " + string(watch.Specifier),
			}
		}
		for _, event := range watch.Events {
			if !slices.Contains(notifySupportedEvents, event) {
				return &imap.Error{
					Type: imap.ErrorTypeNo,
					Code: imap.ResponseCode("BADEVENT"),
					Text: "unsupported NOTIFY event " + string(event),
				}
			}
		}
	}
	return nil
}

// notifyWatchesLocked reports whether the current registration covers a mailbox.
//
// The caller must hold the account lock.
func (s *session) notifyWatchesLocked(name string) bool {
	if s.notifyConfig == nil {
		return false
	}
	for _, group := range s.notifyConfig.Watches {
		switch group.Specifier {
		case imap.NotifyPersonal, imap.NotifySubscribed:
			return true
		case imap.NotifyInboxes:
			if mailboxKey(name) == "INBOX" {
				return true
			}
		case imap.NotifyMailboxes:
			for _, watched := range group.Names {
				if mailboxKey(watched) == mailboxKey(name) {
					return true
				}
			}
		case imap.NotifySubtree:
			for _, watched := range group.Names {
				if name == watched || strings.HasPrefix(name, watched+"/") {
					return true
				}
			}
		}
	}
	return false
}

// notifyMailboxLocked publishes a mailbox's new state to every session watching
// it. It is called from the paths that change a mailbox.
//
// The caller must hold the account lock.
func notifyMailboxLocked(a *account, m *mailbox) {
	for s := range a.sessions {
		if s.notify == nil || !s.notifyWatchesLocked(m.name) {
			continue
		}
		_ = s.notify.Push(&imapserver.SessionUpdate{
			Mailbox: m.name,
			Status:  statusDataLocked(m, nil),
		})
	}
}

var (
	_ imapserver.NotifySession         = (*session)(nil)
	_ imapserver.MessageLimitSession   = (*session)(nil)
	_ imapserver.QuotaSession          = (*session)(nil)
	_ imapserver.QuotaSetSession       = (*session)(nil)
	_ imapserver.ACLSession            = (*session)(nil)
	_ imapserver.ACLSetSession         = (*session)(nil)
	_ imapserver.MetadataSession       = (*session)(nil)
	_ imapserver.NamespaceSession      = (*session)(nil)
	_ imapserver.UnauthenticateSession = (*session)(nil)
)
