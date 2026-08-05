package imapclient

import (
	"context"
	"fmt"
	"strings"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// Aliases for the METADATA vocabulary, which now lives in [package imap] so
// that both protocol directions share it. RFC 5464.
type (
	// MetadataEntryName is a METADATA entry name. See [imap.MetadataEntryName].
	MetadataEntryName = imap.MetadataEntryName
	// MetadataEntry is one entry/value pair. See [imap.MetadataEntry].
	MetadataEntry = imap.MetadataEntry
	// MailboxMetadata is METADATA for one mailbox. See [imap.MailboxMetadata].
	MailboxMetadata = imap.MailboxMetadata
)

// GetMetadataOptions configures GETMETADATA. A nil pointer selects the
// defaults (no MAXSIZE, no DEPTH).
//
// Construct with keyed fields only; fields may be added in a future release.
type GetMetadataOptions struct {
	// MaxSize, when non-nil, asks the server to omit values larger than this
	// many octets (RFC 5464 MAXSIZE option).
	MaxSize *uint32

	// Depth controls DEPTH 0 / 1 / infinity. An empty string omits DEPTH.
	// Accepted values are "0", "1", and "infinity" (case-insensitive).
	Depth string

	_ struct{}
}

// SetMetadataOptions configures SETMETADATA. A nil pointer selects the defaults.
//
// Construct with keyed fields only; fields may be added in a future release.
type SetMetadataOptions struct {
	_ struct{}
}

// GetMetadata retrieves mailbox (or server) metadata entries.
// METADATA / METADATA-SERVER, RFC 5464.
//
// entries names the entry names (or prefixes when options.Depth is set) to
// fetch; at least one is required. A nil options pointer selects the defaults.
//
// An empty mailbox selects the server annotation space and requires
// METADATA-SERVER. A non-empty mailbox requires METADATA (or METADATA-SERVER,
// which implies it on some servers — this client accepts either advertisement
// for mailbox annotations).
func (c *Client) GetMetadata(ctx context.Context, mailbox string, entries []MetadataEntryName, options *GetMetadataOptions) (*MailboxMetadata, error) {
	if ctx == nil {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "GETMETADATA requires a non-nil context"}
	}
	if len(entries) == 0 {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "GETMETADATA requires at least one entry"}
	}
	for _, entry := range entries {
		if strings.TrimSpace(string(entry)) == "" {
			return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "GETMETADATA entry name must not be empty"}
		}
	}
	if mailbox == "" {
		if !c.Supports("METADATA-SERVER") {
			return nil, capabilityError("GETMETADATA for server annotations", "METADATA-SERVER")
		}
	} else if !c.Supports("METADATA") && !c.Supports("METADATA-SERVER") {
		return nil, capabilityError("GETMETADATA", "METADATA")
	}
	depth := ""
	var maxSize *uint32
	if options != nil {
		depth = options.Depth
		maxSize = options.MaxSize
	}
	if err := validateMetadataDepth(depth); err != nil {
		return nil, err
	}
	data := &MailboxMetadata{Mailbox: mailbox}
	var got bool
	limit := c.maxUntaggedResponses()
	count := 0
	cmd := c.beginCommand("GETMETADATA", stateAuthenticated|stateSelected, func(enc *imapwire.Encoder) {
		enc.SP()
		if maxSize != nil || depth != "" {
			enc.Special('(')
			first := true
			if maxSize != nil {
				enc.Atom("MAXSIZE").SP().Number(*maxSize)
				first = false
			}
			if depth != "" {
				if !first {
					enc.SP()
				}
				enc.Atom("DEPTH").SP().Atom(strings.ToLower(depth))
			}
			enc.Special(')').SP()
		}
		enc.Mailbox(mailbox).SP().List(len(entries), func(i int) {
			enc.Astring(string(entries[i]))
		})
	}, func(resp *untaggedResponse) (bool, error) {
		if resp.hasNum || resp.cond != nil || resp.name != "METADATA" {
			return false, nil
		}
		if err := countUntaggedResponse(&count, limit, "GETMETADATA"); err != nil {
			return true, err
		}
		parsed, err := readMetadataResponse(resp.dec)
		if err != nil {
			return true, err
		}
		// RFC 5464 permits multiple METADATA responses per GETMETADATA;
		// merge entries rather than keeping only the last line.
		if parsed.Mailbox != "" {
			data.Mailbox = parsed.Mailbox
		}
		data.Entries = append(data.Entries, parsed.Entries...)
		got = true
		return true, nil
	})
	if err := cmd.Wait(ctx); err != nil {
		return nil, err
	}
	if !got {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "GETMETADATA completed without a METADATA response"}
	}
	return data, nil
}

// SetMetadata sets or clears mailbox (or server) metadata entries.
// SETMETADATA, RFC 5464.
//
// A nil Value on an entry removes that entry. A nil options pointer selects
// the defaults.
func (c *Client) SetMetadata(ctx context.Context, mailbox string, entries []MetadataEntry, options *SetMetadataOptions) error {
	_ = options
	if ctx == nil {
		return &imap.Error{Type: imap.ErrorTypeProtocol, Text: "SETMETADATA requires a non-nil context"}
	}
	if len(entries) == 0 {
		return &imap.Error{Type: imap.ErrorTypeProtocol, Text: "SETMETADATA requires at least one entry"}
	}
	if mailbox == "" {
		if !c.Supports("METADATA-SERVER") {
			return capabilityError("SETMETADATA for server annotations", "METADATA-SERVER")
		}
	} else if !c.Supports("METADATA") && !c.Supports("METADATA-SERVER") {
		return capabilityError("SETMETADATA", "METADATA")
	}
	for _, entry := range entries {
		if strings.TrimSpace(string(entry.Name)) == "" {
			return &imap.Error{Type: imap.ErrorTypeProtocol, Text: "SETMETADATA entry name must not be empty"}
		}
	}
	cmd := c.beginCommand("SETMETADATA", stateAuthenticated|stateSelected, func(enc *imapwire.Encoder) {
		enc.SP().Mailbox(mailbox).SP().List(len(entries), func(i int) {
			enc.Astring(string(entries[i].Name)).SP()
			if entries[i].Value == nil {
				enc.NIL()
			} else {
				enc.NString(*entries[i].Value, false)
			}
		})
	}, nil)
	return cmd.Wait(ctx)
}

// ListReturnMetadata configures LIST RETURN (METADATA …). LIST-METADATA, RFC 9590.
//
// Do not place this type in [ListOptions.ReturnOptions]: [Client.List] and
// [Client.ListMailboxes] reject types they do not own. Pass it to
// [Client.ListMailboxesExt].
//
// Construct with keyed fields only; fields may be added in a future release.
type ListReturnMetadata struct {
	// Entries are the metadata entry names requested in the RETURN option.
	Entries []MetadataEntryName

	// Handler receives one [MailboxMetadata] per LIST response that carried
	// METADATA extended data. A nil Handler still requests the option.
	// Called on the reader goroutine; must not block.
	Handler func(*MailboxMetadata)
	_       struct{}
}

func validateListReturnMetadata(m *ListReturnMetadata) error {
	if m == nil || len(m.Entries) == 0 {
		return &imap.Error{Type: imap.ErrorTypeProtocol, Text: "LIST RETURN (METADATA) requires at least one entry"}
	}
	for _, entry := range m.Entries {
		if strings.TrimSpace(string(entry)) == "" {
			return &imap.Error{Type: imap.ErrorTypeProtocol, Text: "LIST RETURN (METADATA) entry name must not be empty"}
		}
	}
	return nil
}

func validateMetadataDepth(depth string) error {
	if depth == "" {
		return nil
	}
	switch strings.ToLower(depth) {
	case "0", "1", "infinity":
		return nil
	default:
		return &imap.Error{Type: imap.ErrorTypeProtocol, Text: fmt.Sprintf("invalid GETMETADATA DEPTH %q", depth)}
	}
}

func readMetadataResponse(dec *imapwire.Decoder) (*MailboxMetadata, error) {
	if !dec.ExpectSP() {
		return nil, dec.Err()
	}
	var mailbox string
	if !dec.ExpectMailbox(&mailbox) || !dec.ExpectSP() {
		return nil, dec.Err()
	}
	entries, err := readMetadataEntryValues(dec)
	if err != nil {
		return nil, err
	}
	if !dec.ExpectCRLF() {
		return nil, dec.Err()
	}
	return &MailboxMetadata{Mailbox: mailbox, Entries: entries}, nil
}

func readMetadataEntryValues(dec *imapwire.Decoder) ([]MetadataEntry, error) {
	var entries []MetadataEntry
	err := dec.ExpectList(func() error {
		var name string
		if !dec.ExpectAstring(&name) || !dec.ExpectSP() {
			return dec.Err()
		}
		var value string
		var isNil bool
		if !dec.ExpectNString(&value, &isNil) {
			return dec.Err()
		}
		entry := MetadataEntry{Name: MetadataEntryName(name)}
		if !isNil {
			v := value
			entry.Value = &v
		}
		entries = append(entries, entry)
		return nil
	})
	return entries, err
}

// MetadataString is a convenience for building a present METADATA value.
func MetadataString(s string) *string { return &s }
