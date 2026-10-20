package maildir

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
)

// metadataValue is a stored METADATA value. RFC 5464 section 3.2 allows any
// octets, which a JSON string cannot carry: encoding/json replaces invalid
// UTF-8 with U+FFFD, so a binary value read back correctly until the next
// rewrite or restart and was corrupted from then on. Valid UTF-8 keeps the
// plain string form, which is also what earlier state files hold; anything
// else is stored as {"base64": ...}.
type metadataValue string

type binaryMetadataValue struct {
	Base64 *[]byte `json:"base64"`
}

func (v metadataValue) MarshalJSON() ([]byte, error) {
	if utf8.ValidString(string(v)) {
		return json.Marshal(string(v))
	}
	octets := []byte(v)
	return json.Marshal(binaryMetadataValue{Base64: &octets})
}

func (v *metadataValue) UnmarshalJSON(data []byte) error {
	if trimmed := bytes.TrimSpace(data); len(trimmed) > 0 && trimmed[0] == '{' {
		var binary binaryMetadataValue
		decoder := json.NewDecoder(bytes.NewReader(trimmed))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&binary); err != nil || binary.Base64 == nil {
			return fmt.Errorf("maildir: invalid binary metadata value")
		}
		*v = metadataValue(*binary.Base64)
		return nil
	}
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return err
	}
	*v = metadataValue(text)
	return nil
}

// validEntryName applies RFC 5464 section 3.2, whose invalid names "result in
// a BAD response". The framework passes names through unchecked, and the names
// are JSON object keys here, so one outside ASCII would not survive a rewrite.
func validEntryName(name imap.MetadataEntryName) bool {
	text := string(name)
	if text == "" || strings.HasSuffix(text, "/") || strings.Contains(text, "//") {
		return false
	}
	for i := 0; i < len(text); i++ {
		if c := text[i]; c <= 0x19 || c >= 0x80 || c == '*' || c == '%' {
			return false
		}
	}
	return true
}

func invalidEntryName() error {
	return &imap.Error{Type: imap.ErrorTypeBad, Text: "invalid metadata entry name"}
}

func metadataStore(state *accountState, name string) (map[imap.MetadataEntryName]metadataValue, error) {
	if name == "" {
		return state.Metadata, nil
	}
	m := state.Mailboxes[mailboxKey(name)]
	if m == nil {
		return nil, missingMailbox()
	}
	return m.Metadata, nil
}
func (s *session) GetMetadata(ctx context.Context, name string, entries []imap.MetadataEntryName, options *imapserver.GetMetadataOptions) (*imap.MailboxMetadata, error) {
	for _, entry := range entries {
		if !validEntryName(entry) {
			return nil, invalidEntryName()
		}
	}
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
			copy := string(value)
			result.Entries = append(result.Entries, imap.MetadataEntry{Name: key, Value: &copy})
			seen[key] = true
		}
	}
	slices.SortFunc(result.Entries, func(a, b imap.MetadataEntry) int { return strings.Compare(string(a.Name), string(b.Name)) })
	return result, nil
}
func (s *session) SetMetadata(ctx context.Context, name string, entries []imap.MetadataEntry, _ *imapserver.SetMetadataOptions) error {
	for _, entry := range entries {
		if !validEntryName(entry.Name) {
			return invalidEntryName()
		}
	}
	if err := s.enter(ctx); err != nil {
		return err
	}
	defer s.account.leave()
	for _, entry := range entries {
		if entry.Value != nil && uint64(len(*entry.Value)) > uint64(s.account.backend.maxMetadataValueBytes) {
			return &imap.Error{Type: imap.ErrorTypeNo, Code: imap.CodeMetadata, CodeArgs: fmt.Sprintf("MAXSIZE %d", s.account.backend.maxMetadataValueBytes), Text: "metadata value exceeds storage limit"}
		}
	}
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
			changed = changed || !present || string(old) != *entry.Value
			store[entry.Name] = metadataValue(*entry.Value)
		}
	}
	previous, err := metadataStore(s.account.state, name)
	if err != nil {
		return err
	}
	if len(store) > s.account.backend.maxMetadataEntries && len(store) >= len(previous) {
		return &imap.Error{Type: imap.ErrorTypeNo, Code: imap.CodeMetadata, CodeArgs: "TOOMANY", Text: "metadata entry count exceeds storage limit"}
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
