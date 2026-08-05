package imapclient

import (
	"context"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// LanguageOptions configures LANGUAGE. A nil pointer with empty Tags enumerates
// the languages the server supports (RFC 5255 section 3.2).
//
// Construct with keyed fields only; fields may be added in a future release.
type LanguageOptions struct {
	// Tags are language ranges to negotiate, in preference order. An empty
	// slice issues LANGUAGE with no arguments (enumerate).
	Tags []string
	_    struct{}
}

// LanguageData is one LANGUAGE response. It is an alias for
// [imap.LanguageData], which both protocol directions share.
type LanguageData = imap.LanguageData

// ComparatorOptions configures COMPARATOR. A nil pointer with empty Wanted
// returns the active comparator (RFC 5255 section 4.7).
//
// Construct with keyed fields only; fields may be added in a future release.
type ComparatorOptions struct {
	// Wanted is the preferred comparator order. Empty issues COMPARATOR with
	// no arguments and returns the active comparator only.
	Wanted []string
	_      struct{}
}

// ComparatorData is one COMPARATOR response. It is an alias for
// [imap.ComparatorData], which both protocol directions share.
type ComparatorData = imap.ComparatorData

// Language negotiates human-readable response language. LANGUAGE, RFC 5255.
//
// Valid in every session state. Requires the LANGUAGE capability.
func (c *Client) Language(ctx context.Context, options *LanguageOptions) (*LanguageData, error) {
	if ctx == nil {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "LANGUAGE requires a non-nil context"}
	}
	if !c.Supports("LANGUAGE") {
		return nil, capabilityError("LANGUAGE", "LANGUAGE")
	}
	var tags []string
	if options != nil {
		tags = options.Tags
	}
	data := &LanguageData{}
	var got bool
	cmd := c.beginCommand("LANGUAGE", stateNotAuthenticated|stateAuthenticated|stateSelected, func(enc *imapwire.Encoder) {
		for _, tag := range tags {
			enc.SP().Astring(tag)
		}
	}, func(resp *untaggedResponse) (bool, error) {
		if resp.hasNum || resp.cond != nil || resp.name != "LANGUAGE" {
			return false, nil
		}
		parsed, err := readLanguageResponse(resp.dec)
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
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "LANGUAGE completed without a LANGUAGE response"}
	}
	return data, nil
}

// Comparator queries or sets the active collation comparator.
// COMPARATOR, RFC 5255. Requires I18NLEVEL=2 (I18NLEVEL=1 has no COMPARATOR).
func (c *Client) Comparator(ctx context.Context, options *ComparatorOptions) (*ComparatorData, error) {
	if ctx == nil {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "COMPARATOR requires a non-nil context"}
	}
	if !c.Supports("I18NLEVEL=2") {
		return nil, capabilityError("COMPARATOR", "I18NLEVEL=2")
	}
	var wanted []string
	if options != nil {
		wanted = options.Wanted
	}
	data := &ComparatorData{}
	var got bool
	cmd := c.beginCommand("COMPARATOR", stateAuthenticated|stateSelected, func(enc *imapwire.Encoder) {
		for _, name := range wanted {
			enc.SP().Astring(name)
		}
	}, func(resp *untaggedResponse) (bool, error) {
		if resp.hasNum || resp.cond != nil || resp.name != "COMPARATOR" {
			return false, nil
		}
		parsed, err := readComparatorResponse(resp.dec)
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
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "COMPARATOR completed without a COMPARATOR response"}
	}
	return data, nil
}

// I18NLevel returns the highest I18NLEVEL=n the server advertises, or 0 when
// neither I18NLEVEL=1 nor I18NLEVEL=2 is present. RFC 5255.
func (c *Client) I18NLevel() int {
	if c.Supports("I18NLEVEL=2") {
		return 2
	}
	if c.Supports("I18NLEVEL=1") {
		return 1
	}
	return 0
}

func readLanguageResponse(dec *imapwire.Decoder) (*LanguageData, error) {
	if !dec.ExpectSP() {
		return nil, dec.Err()
	}
	data := &LanguageData{}
	err := dec.ExpectList(func() error {
		var tag string
		if !dec.Quoted(&tag) && !dec.ExpectAstring(&tag) {
			return dec.Err()
		}
		data.Tags = append(data.Tags, tag)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !dec.ExpectCRLF() {
		return nil, dec.Err()
	}
	return data, nil
}

func readComparatorResponse(dec *imapwire.Decoder) (*ComparatorData, error) {
	if !dec.ExpectSP() {
		return nil, dec.Err()
	}
	var active string
	if !dec.Quoted(&active) && !dec.ExpectAstring(&active) {
		return nil, dec.Err()
	}
	data := &ComparatorData{Active: active}
	if dec.SP() {
		err := dec.ExpectList(func() error {
			var name string
			if !dec.Quoted(&name) && !dec.ExpectAstring(&name) {
				return dec.Err()
			}
			data.Matching = append(data.Matching, name)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	if !dec.ExpectCRLF() {
		return nil, dec.Err()
	}
	return data, nil
}
