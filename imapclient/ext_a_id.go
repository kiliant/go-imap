package imapclient

import (
	"context"
	"fmt"
	"strings"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// IDField is one name/value pair exchanged by the ID command. ID, RFC 2971.
//
// Name is required. Value is nil when the wire value is NIL — distinct from an
// empty string. Field names are open-ended; RFC 2971 section 3.3 defines
// conventional names (name, version, os, …) but servers and clients may send
// others without an API change.
//
// Construct with keyed fields only; fields may be added in a future release.
type IDField struct {
	Name  string
	Value *string
	_     struct{}
}

// IDOptions configures ID. A nil pointer selects the defaults (send ID NIL).
//
// Construct with keyed fields only; fields may be added in a future release.
type IDOptions struct {
	// Fields is the client parameter list. A nil or empty slice sends ID NIL,
	// which still invites a server response (RFC 2971 section 3.1). To send an
	// empty list instead, pass a non-nil empty Fields via a future API if
	// needed — the NIL form is the privacy-preserving default.
	Fields []IDField
	_      struct{}
}

// IDData is the server identification from an untagged ID response.
//
// Fields is nil when the server sent ID NIL. A non-nil (possibly empty) slice
// is a parameter list. Received is false when the command completed without an
// untagged ID — allowed by RFC 2971 section 3.1 ("OPTIONAL untagged response").
//
// Construct with keyed fields only; fields may be added in a future release.
type IDData struct {
	Received bool
	Fields   []IDField
	_        struct{}
}

// IDString is a convenience for building a present ID field value.
func IDString(s string) *string { return &s }

// ID exchanges client and server identification information. ID, RFC 2971.
//
// Valid in every session state. Requires the ID capability. A nil options
// pointer (or empty Fields) sends ID NIL. The returned data reflects the
// untagged ID response when the server supplied one.
//
// Implementations MUST NOT make operational decisions from ID data (RFC 2971
// section 3). Field names longer than 30 octets, values longer than 1024
// octets, more than 30 pairs, or duplicate names are rejected locally before
// they hit the wire.
func (c *Client) ID(ctx context.Context, options *IDOptions) (*IDData, error) {
	if ctx == nil {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "ID requires a non-nil context"}
	}
	if !c.Supports("ID") {
		return nil, capabilityError("ID", "ID")
	}
	var fields []IDField
	if options != nil {
		fields = options.Fields
	}
	if err := validateIDFields(fields); err != nil {
		return nil, err
	}
	data := &IDData{}
	var got bool
	cmd := c.beginCommand("ID", stateNotAuthenticated|stateAuthenticated|stateSelected, func(enc *imapwire.Encoder) {
		enc.SP()
		if len(fields) == 0 {
			enc.NIL()
			return
		}
		enc.List(len(fields), func(i int) {
			enc.String(fields[i].Name).SP()
			if fields[i].Value == nil {
				enc.NIL()
			} else {
				enc.NString(*fields[i].Value, false)
			}
		})
	}, func(resp *untaggedResponse) (bool, error) {
		if resp.hasNum || resp.cond != nil || resp.name != "ID" {
			return false, nil
		}
		parsed, err := readIDResponse(resp.dec)
		if err != nil {
			return true, err
		}
		*data = *parsed
		got = true
		return true, nil
	})
	if err := cmd.Wait(ctx); err != nil {
		return nil, err
	}
	if !got {
		return &IDData{}, nil
	}
	return data, nil
}

func validateIDFields(fields []IDField) error {
	if len(fields) > 30 {
		return &imap.Error{Type: imap.ErrorTypeProtocol, Text: "ID allows at most 30 field-value pairs"}
	}
	seen := make(map[string]struct{}, len(fields))
	for _, f := range fields {
		name := f.Name
		if name == "" {
			return &imap.Error{Type: imap.ErrorTypeProtocol, Text: "ID field name must not be empty"}
		}
		if len(name) > 30 {
			return &imap.Error{Type: imap.ErrorTypeProtocol, Text: "ID field name must not exceed 30 octets"}
		}
		key := strings.ToLower(name)
		if _, ok := seen[key]; ok {
			return &imap.Error{
				Type: imap.ErrorTypeProtocol,
				Text: fmt.Sprintf("ID must not send field %q more than once", name),
			}
		}
		seen[key] = struct{}{}
		if f.Value != nil && len(*f.Value) > 1024 {
			return &imap.Error{Type: imap.ErrorTypeProtocol, Text: "ID field value must not exceed 1024 octets"}
		}
	}
	return nil
}

func readIDResponse(dec *imapwire.Decoder) (*IDData, error) {
	if !dec.ExpectSP() {
		return nil, dec.Err()
	}
	data := &IDData{Received: true}
	if !dec.PeekSpecial('(') {
		var unused string
		var isNil bool
		if !dec.ExpectNString(&unused, &isNil) || !isNil {
			if dec.Err() != nil {
				return nil, dec.Err()
			}
			return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "ID response must be NIL or a list"}
		}
		if !dec.ExpectCRLF() {
			return nil, dec.Err()
		}
		return data, nil
	}
	fields, err := readIDFields(dec)
	if err != nil {
		return nil, err
	}
	data.Fields = fields
	if !dec.ExpectCRLF() {
		return nil, dec.Err()
	}
	return data, nil
}

func readIDFields(dec *imapwire.Decoder) ([]IDField, error) {
	fields := make([]IDField, 0)
	err := dec.ExpectList(func() error {
		if len(fields) >= 30 {
			return &imap.Error{Type: imap.ErrorTypeProtocol, Text: "ID response has more than 30 field-value pairs"}
		}
		var name string
		// Grammar uses string; accept astring so atom field names still parse.
		if !dec.Quoted(&name) && !dec.ExpectAstring(&name) {
			return dec.Err()
		}
		if len(name) > 30 {
			return &imap.Error{Type: imap.ErrorTypeProtocol, Text: "ID field name must not exceed 30 octets"}
		}
		if !dec.ExpectSP() {
			return dec.Err()
		}
		var value string
		var isNil bool
		if !dec.ExpectNString(&value, &isNil) {
			return dec.Err()
		}
		if !isNil && len(value) > 1024 {
			return &imap.Error{Type: imap.ErrorTypeProtocol, Text: "ID field value must not exceed 1024 octets"}
		}
		field := IDField{Name: name}
		if !isNil {
			v := value
			field.Value = &v
		}
		fields = append(fields, field)
		return nil
	})
	return fields, err
}
