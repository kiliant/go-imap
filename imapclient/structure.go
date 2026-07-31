package imapclient

import (
	"fmt"
	"strings"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// Semantic decoding of the ENVELOPE and BODY/BODYSTRUCTURE fetch items.
//
// It lives here rather than in internal/imapwire because the wire codec deals
// only in primitives and must not import the vocabulary package; see
// docs/ARCHITECTURE.md. The decoder below therefore drives the primitive
// matchers and builds the exported types.
//
// Both productions carry extension points that servers already use, so an
// unrecognised trailing field is skipped rather than rejected: a server sending
// more body extension data than RFC 3501 defines is not a hostile server, and
// refusing the response would make its mailboxes unreadable.

// maxBodyStructureDepth bounds nesting independently of the decoder's own list
// depth limit. A multipart tree this deep is not a real message.
const maxBodyStructureDepth = 64

// readEnvelope decodes the envelope production of RFC 3501 section 9.
func readEnvelope(dec *imapwire.Decoder) (*imap.Envelope, error) {
	env := &imap.Envelope{}
	field := 0
	err := dec.ExpectList(func() error {
		defer func() { field++ }()
		switch field {
		case 0:
			// The envelope date is the raw Date header, not an IMAP date-time,
			// and an unparseable one is common enough that it must not fail the
			// whole response.
			var raw string
			var isNil bool
			if !dec.ExpectNString(&raw, &isNil) {
				return dec.Err()
			}
			if !isNil {
				env.Date = parseMessageDate(raw)
			}
			return nil
		case 1:
			var subject string
			var isNil bool
			if !dec.ExpectNString(&subject, &isNil) {
				return dec.Err()
			}
			if !isNil {
				env.Subject = imap.DecodeHeader(subject)
			}
			return nil
		case 2, 3, 4, 5, 6, 7:
			addrs, err := readAddressList(dec)
			if err != nil {
				return err
			}
			switch field {
			case 2:
				env.From = addrs
			case 3:
				env.Sender = addrs
			case 4:
				env.ReplyTo = addrs
			case 5:
				env.To = addrs
			case 6:
				env.Cc = addrs
			case 7:
				env.Bcc = addrs
			}
			return nil
		case 8:
			var raw string
			var isNil bool
			if !dec.ExpectNString(&raw, &isNil) {
				return dec.Err()
			}
			if !isNil {
				env.InReplyTo = imap.ParseMessageIDList(raw)
			}
			return nil
		case 9:
			var raw string
			var isNil bool
			if !dec.ExpectNString(&raw, &isNil) {
				return dec.Err()
			}
			env.MessageID = strings.TrimSpace(raw)
			return nil
		default:
			// Not defined by any RFC, but skipping keeps the stream aligned if
			// one ever is.
			return dec.DiscardValue()
		}
	})
	if err != nil {
		return nil, err
	}
	if field < 10 {
		return nil, fmt.Errorf("ENVELOPE has %d fields, want 10", field)
	}
	return env, nil
}

// readAddressList decodes "(1*address)" or NIL.
func readAddressList(dec *imapwire.Decoder) ([]imap.Address, error) {
	if !dec.PeekSpecial('(') {
		var s string
		var isNil bool
		if !dec.ExpectNString(&s, &isNil) {
			return nil, dec.Err()
		}
		if !isNil {
			return nil, fmt.Errorf("ENVELOPE address list is neither a list nor NIL")
		}
		return nil, nil
	}
	var addrs []imap.Address
	// The grammar juxtaposes addresses without a separator — "1*address" — so
	// one call has to consume the whole run. Servers that insert a space
	// anyway are handled by the enclosing list, which calls this again.
	err := dec.ExpectList(func() error {
		for {
			addr, err := readAddress(dec)
			if err != nil {
				return err
			}
			addrs = append(addrs, addr)
			if !dec.PeekSpecial('(') {
				return nil
			}
		}
	})
	if err != nil {
		return nil, err
	}
	return addrs, nil
}

// readAddress decodes the address production: name, source route, mailbox and
// host. The source route is parsed and dropped; see [imap.Address].
func readAddress(dec *imapwire.Decoder) (imap.Address, error) {
	var addr imap.Address
	field := 0
	err := dec.ExpectList(func() error {
		defer func() { field++ }()
		var value string
		var isNil bool
		if field > 3 {
			return dec.DiscardValue()
		}
		if !dec.ExpectNString(&value, &isNil) {
			return dec.Err()
		}
		switch field {
		case 0:
			addr.Name = imap.DecodeHeader(value)
		case 1: // adl, the deprecated RFC 822 source route
		case 2:
			addr.Mailbox = value
		case 3:
			addr.Host = value
		}
		return nil
	})
	if err != nil {
		return imap.Address{}, err
	}
	if field < 4 {
		return imap.Address{}, fmt.Errorf("ENVELOPE address has %d fields, want 4", field)
	}
	return addr, nil
}

// parseMessageDate parses the Date header spellings that reach an envelope.
// A header the server could not normalise is reported as the zero time rather
// than as an error; see [imap.Envelope].
func parseMessageDate(s string) time.Time {
	s = strings.TrimSpace(s)
	layouts := []string{
		"Mon, 2 Jan 2006 15:04:05 -0700",
		"Mon, 2 Jan 2006 15:04:05 -0700 (MST)",
		"2 Jan 2006 15:04:05 -0700",
		"Mon, 2 Jan 2006 15:04:05 MST",
		"2 Jan 2006 15:04:05 MST",
		"Mon, 2 Jan 2006 15:04 -0700",
		"2 Jan 2006 15:04 -0700",
		"Mon, 2 Jan 2006 15:04:05",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// readBodyStructure decodes the body production of RFC 3501 section 9, which
// covers both the BODY and BODYSTRUCTURE fetch items. The two differ only in
// whether the server appends the extension fields.
func readBodyStructure(dec *imapwire.Decoder, depth int) (imap.BodyStructure, error) {
	if depth > maxBodyStructureDepth {
		return nil, fmt.Errorf("BODYSTRUCTURE nested deeper than %d parts", maxBodyStructureDepth)
	}
	var bs imap.BodyStructure
	first := true
	err := dec.ExpectList(func() error {
		if !first {
			return readBodyStructureTail(dec, bs)
		}
		first = false
		// A multipart body opens with its nested parts; every other body opens
		// with the media type string.
		if dec.PeekSpecial('(') {
			mp, err := readMultiPart(dec, depth)
			if err != nil {
				return err
			}
			bs = mp
			return nil
		}
		sp, err := readSinglePart(dec, depth)
		if err != nil {
			return err
		}
		bs = sp
		return nil
	})
	if err != nil {
		return nil, err
	}
	if bs == nil {
		return nil, fmt.Errorf("empty BODYSTRUCTURE")
	}
	return bs, nil
}

// readBodyStructureTail consumes one element after the part has been decoded.
// The element positions the grammar defines are consumed by readSinglePart and
// readMultiPart themselves, so anything arriving here is extension data from a
// later RFC than this parser knows.
func readBodyStructureTail(dec *imapwire.Decoder, bs imap.BodyStructure) error {
	if bs == nil {
		return fmt.Errorf("malformed BODYSTRUCTURE")
	}
	return dec.DiscardValue()
}

func readMultiPart(dec *imapwire.Decoder, depth int) (*imap.BodyStructureMultiPart, error) {
	mp := &imap.BodyStructureMultiPart{}
	// "1*body SP media-subtype": the nested parts are juxtaposed with no
	// separator between them.
	for dec.PeekSpecial('(') {
		child, err := readBodyStructure(dec, depth+1)
		if err != nil {
			return nil, err
		}
		mp.Children = append(mp.Children, child)
	}
	if !dec.ExpectSP() || !dec.ExpectString(&mp.Subtype) {
		return nil, dec.Err()
	}
	if !dec.SP() {
		return mp, nil
	}
	ext := &imap.BodyStructureMultiPartExt{}
	params, err := readBodyParams(dec)
	if err != nil {
		return nil, err
	}
	ext.Params = params
	mp.Extended = ext
	if !dec.SP() {
		return mp, nil
	}
	if ext.Disp, err = readBodyDisposition(dec); err != nil {
		return nil, err
	}
	if !dec.SP() {
		return mp, nil
	}
	if ext.Lang, err = readBodyLang(dec); err != nil {
		return nil, err
	}
	if !dec.SP() {
		return mp, nil
	}
	if ext.Location, err = readNString(dec); err != nil {
		return nil, err
	}
	return mp, nil
}

func readSinglePart(dec *imapwire.Decoder, depth int) (*imap.BodyStructureSinglePart, error) {
	sp := &imap.BodyStructureSinglePart{}
	if !dec.ExpectString(&sp.Type) || !dec.ExpectSP() || !dec.ExpectString(&sp.Subtype) || !dec.ExpectSP() {
		return nil, dec.Err()
	}
	params, err := readBodyParams(dec)
	if err != nil {
		return nil, err
	}
	sp.Params = params
	if !dec.ExpectSP() {
		return nil, dec.Err()
	}
	if sp.ID, err = readNString(dec); err != nil {
		return nil, err
	}
	if !dec.ExpectSP() {
		return nil, dec.Err()
	}
	description, err := readNString(dec)
	if err != nil {
		return nil, err
	}
	sp.Description = imap.DecodeHeader(description)
	if !dec.ExpectSP() {
		return nil, dec.Err()
	}
	if sp.Encoding, err = readNString(dec); err != nil {
		return nil, err
	}
	if !dec.ExpectSP() || !dec.ExpectNumber(&sp.Size) {
		return nil, dec.Err()
	}

	switch {
	case strings.EqualFold(sp.Type, "message") && strings.EqualFold(sp.Subtype, "rfc822"):
		msg := &imap.BodyStructureMessageRFC822{}
		if !dec.ExpectSP() {
			return nil, dec.Err()
		}
		if msg.Envelope, err = readEnvelope(dec); err != nil {
			return nil, err
		}
		if !dec.ExpectSP() {
			return nil, dec.Err()
		}
		if msg.BodyStructure, err = readBodyStructure(dec, depth+1); err != nil {
			return nil, err
		}
		if !dec.ExpectSP() || !dec.ExpectNumber64(&msg.NumLines) {
			return nil, dec.Err()
		}
		sp.Message = msg
	case strings.EqualFold(sp.Type, "text"):
		text := &imap.BodyStructureText{}
		if !dec.ExpectSP() || !dec.ExpectNumber64(&text.NumLines) {
			return nil, dec.Err()
		}
		sp.Text = text
	}

	if !dec.SP() {
		return sp, nil
	}
	ext := &imap.BodyStructureSinglePartExt{}
	if ext.MD5, err = readNString(dec); err != nil {
		return nil, err
	}
	sp.Extended = ext
	if !dec.SP() {
		return sp, nil
	}
	if ext.Disp, err = readBodyDisposition(dec); err != nil {
		return nil, err
	}
	if !dec.SP() {
		return sp, nil
	}
	if ext.Lang, err = readBodyLang(dec); err != nil {
		return nil, err
	}
	if !dec.SP() {
		return sp, nil
	}
	if ext.Location, err = readNString(dec); err != nil {
		return nil, err
	}
	return sp, nil
}

// readBodyParams decodes body-fld-param: a flat list of alternating names and
// values, or NIL.
func readBodyParams(dec *imapwire.Decoder) (map[string]string, error) {
	if !dec.PeekSpecial('(') {
		if _, err := readNStringExpectingNil(dec, "body parameter list"); err != nil {
			return nil, err
		}
		return nil, nil
	}
	var pairs []string
	err := dec.ExpectList(func() error {
		var value string
		if !dec.ExpectString(&value) {
			return dec.Err()
		}
		pairs = append(pairs, value)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(pairs)%2 != 0 {
		return nil, fmt.Errorf("body parameter list has an odd number of elements")
	}
	raw := make(map[string]string, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		raw[strings.ToLower(pairs[i])] = pairs[i+1]
	}
	return imap.DecodeParams(raw), nil
}

// readBodyDisposition decodes body-fld-dsp: "(" string SP body-fld-param ")"
// or NIL.
func readBodyDisposition(dec *imapwire.Decoder) (*imap.BodyStructureDisposition, error) {
	if !dec.PeekSpecial('(') {
		if _, err := readNStringExpectingNil(dec, "body disposition"); err != nil {
			return nil, err
		}
		return nil, nil
	}
	disp := &imap.BodyStructureDisposition{}
	field := 0
	err := dec.ExpectList(func() error {
		defer func() { field++ }()
		switch field {
		case 0:
			if !dec.ExpectString(&disp.Value) {
				return dec.Err()
			}
			return nil
		case 1:
			params, err := readBodyParams(dec)
			if err != nil {
				return err
			}
			disp.Params = params
			return nil
		default:
			return dec.DiscardValue()
		}
	})
	if err != nil {
		return nil, err
	}
	return disp, nil
}

// readBodyLang decodes body-fld-lang: one string, a list of strings, or NIL.
func readBodyLang(dec *imapwire.Decoder) ([]string, error) {
	if !dec.PeekSpecial('(') {
		value, err := readNString(dec)
		if err != nil {
			return nil, err
		}
		if value == "" {
			return nil, nil
		}
		return []string{value}, nil
	}
	var langs []string
	err := dec.ExpectList(func() error {
		var value string
		if !dec.ExpectString(&value) {
			return dec.Err()
		}
		langs = append(langs, value)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return langs, nil
}

func readNString(dec *imapwire.Decoder) (string, error) {
	var value string
	if !dec.ExpectNString(&value, nil) {
		return "", dec.Err()
	}
	return value, nil
}

func readNStringExpectingNil(dec *imapwire.Decoder, what string) (string, error) {
	var value string
	var isNil bool
	if !dec.ExpectNString(&value, &isNil) {
		return "", dec.Err()
	}
	if !isNil {
		return "", fmt.Errorf("%s is neither a list nor NIL", what)
	}
	return value, nil
}
