package maildir

import (
	"context"
	"slices"
	"strings"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
)

func metadataStore(state *accountState, name string) (map[imap.MetadataEntryName]string, error) {
	if name == "" {
		return state.Metadata, nil
	}
	m := state.Mailboxes[mailboxKey(name)]
	if m == nil {
		return nil, missingMailbox()
	}
	return m.Metadata, nil
}
func (s *session) GetMetadata(ctx context.Context, name string, entries []imap.MetadataEntryName, options *imapserver.MetadataOptions) (*imap.MailboxMetadata, error) {
	if err := s.enter(ctx); err != nil {
		return nil, err
	}
	defer s.account.leave()
	store, err := metadataStore(s.account.state, name)
	if err != nil {
		return nil, err
	}
	result := &imap.MailboxMetadata{Mailbox: name}
	seen := make(map[imap.MetadataEntryName]bool)
	depth := ""
	if options != nil {
		depth = options.Depth
	}
	for _, requested := range entries {
		for key, value := range store {
			if seen[key] {
				continue
			}
			match := key == requested
			if strings.HasPrefix(string(key), string(requested)+"/") {
				tail := strings.TrimPrefix(string(key), string(requested)+"/")
				match = depth == "infinity" || depth == "1" && !strings.Contains(tail, "/")
			}
			if !match || options != nil && options.HasMaxSize && uint64(len(value)) > uint64(options.MaxSize) {
				continue
			}
			copy := value
			result.Entries = append(result.Entries, imap.MetadataEntry{Name: key, Value: &copy})
			seen[key] = true
		}
	}
	slices.SortFunc(result.Entries, func(a, b imap.MetadataEntry) int { return strings.Compare(string(a.Name), string(b.Name)) })
	return result, nil
}
func (s *session) SetMetadata(ctx context.Context, name string, entries []imap.MetadataEntry, _ *imapserver.MetadataOptions) error {
	if err := s.enter(ctx); err != nil {
		return err
	}
	defer s.account.leave()
	next, err := cloneState(s.account.state)
	if err != nil {
		return err
	}
	store, err := metadataStore(next, name)
	if err != nil {
		return err
	}
	changed := false
	for _, entry := range entries {
		if entry.Value == nil {
			_, present := store[entry.Name]
			changed = changed || present
			delete(store, entry.Name)
		} else {
			old, present := store[entry.Name]
			changed = changed || !present || old != *entry.Value
			store[entry.Name] = *entry.Value
		}
	}
	if changed && name != "" {
		// QRESYNC also advances the mailbox clock for mailbox annotations;
		// message modification sequences remain unchanged (RFC 7162 3.2).
		if _, err := bump(next.Mailboxes[mailboxKey(name)]); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.account.commit(next, nil)
}

var _ imapserver.MetadataSession = (*session)(nil)
