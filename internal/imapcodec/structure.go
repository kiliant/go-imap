// Package imapcodec encodes and decodes the semantic protocol vocabulary in
// package imap. The lower-level imapwire package deliberately knows only IMAP
// grammar primitives; this package is the shared semantic layer used by both
// the client and server directions.
package imapcodec

import (
	"fmt"
	"mime"
	"net/mail"
	"sort"
	"strings"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

const maxBodyStructureDepth = 64

// ReadEnvelope decodes the envelope production.
func ReadEnvelope(dec *imapwire.Decoder) (*imap.Envelope, error) {
	env := &imap.Envelope{}
	field := 0
	err := dec.ExpectList(func() error {
		defer func() { field++ }()
		switch field {
		case 0:
			var raw string
			var isNil bool
			if !dec.ExpectNString(&raw, &isNil) {
				return dec.Err()
			}
			if !isNil {
				env.RawDate = raw
				env.Date = parseMessageDate(raw)
			}
		case 1:
			var subject string
			var isNil bool
			if !dec.ExpectNString(&subject, &isNil) {
				return dec.Err()
			}
			if !isNil {
				env.Subject = imap.DecodeHeader(subject)
			}
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
		case 8:
			var raw string
			var isNil bool
			if !dec.ExpectNString(&raw, &isNil) {
				return dec.Err()
			}
			if !isNil {
				env.InReplyTo = imap.ParseMessageIDList(raw)
			}
		case 9:
			var raw string
			if !dec.ExpectNString(&raw, nil) {
				return dec.Err()
			}
			env.MessageID = strings.TrimSpace(raw)
		default:
			return dec.DiscardValue()
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if field < 10 {
		return nil, fmt.Errorf("ENVELOPE has %d fields, want 10", field)
	}
	return env, nil
}

// WriteEnvelope encodes the envelope production.
func WriteEnvelope(enc *imapwire.Encoder, env *imap.Envelope) {
	if env == nil {
		env = &imap.Envelope{}
	}
	enc.Special('(')
	if env.RawDate != "" {
		enc.String(env.RawDate)
	} else if env.Date.IsZero() {
		enc.NIL()
	} else {
		enc.String(env.Date.Format("Mon, 02 Jan 2006 15:04:05 -0700"))
	}
	enc.SP().NString(encodeHeader(env.Subject), env.Subject == "")
	for _, addrs := range [][]imap.Address{env.From, env.Sender, env.ReplyTo, env.To, env.Cc, env.Bcc} {
		enc.SP()
		writeAddressList(enc, addrs)
	}
	enc.SP().NString(strings.Join(env.InReplyTo, " "), len(env.InReplyTo) == 0)
	enc.SP().NString(env.MessageID, env.MessageID == "")
	enc.Special(')')
}

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
	return addrs, err
}

func writeAddressList(enc *imapwire.Encoder, addrs []imap.Address) {
	if len(addrs) == 0 {
		enc.NIL()
		return
	}
	enc.Special('(')
	for i := range addrs {
		writeAddress(enc, &addrs[i])
	}
	enc.Special(')')
}

func readAddress(dec *imapwire.Decoder) (imap.Address, error) {
	var addr imap.Address
	field := 0
	err := dec.ExpectList(func() error {
		defer func() { field++ }()
		if field > 3 {
			return dec.DiscardValue()
		}
		var value string
		if !dec.ExpectNString(&value, nil) {
			return dec.Err()
		}
		switch field {
		case 0:
			addr.Name = imap.DecodeHeader(value)
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

func writeAddress(enc *imapwire.Encoder, addr *imap.Address) {
	enc.Special('(')
	enc.NString(encodeHeader(addr.Name), addr.Name == "")
	enc.SP().NIL() // obsolete source route
	enc.SP().NString(addr.Mailbox, addr.Mailbox == "")
	enc.SP().NString(addr.Host, addr.Host == "")
	enc.Special(')')
}

func encodeHeader(s string) string {
	if s == "" || isASCII(s) {
		return s
	}
	return mime.QEncoding.Encode("utf-8", s)
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

func parseMessageDate(s string) time.Time {
	s = strings.TrimSpace(s)
	if parsed, err := mail.ParseDate(s); err == nil {
		return parsed
	}
	for _, layout := range []string{
		"Mon, 2 Jan 2006 15:04:05 -0700",
		"Mon, 2 Jan 2006 15:04:05 -0700 (MST)",
		"2 Jan 2006 15:04:05 -0700",
		"Mon, 2 Jan 2006 15:04:05 MST",
		"2 Jan 2006 15:04:05 MST",
		"Mon, 2 Jan 2006 15:04 -0700",
		"2 Jan 2006 15:04 -0700",
		"Mon, 2 Jan 2006 15:04:05",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// ReadBodyStructure decodes a BODY or BODYSTRUCTURE body production.
func ReadBodyStructure(dec *imapwire.Decoder) (imap.BodyStructure, error) {
	return readBodyStructure(dec, 0)
}

func readBodyStructure(dec *imapwire.Decoder, depth int) (imap.BodyStructure, error) {
	if depth > maxBodyStructureDepth {
		return nil, fmt.Errorf("BODYSTRUCTURE nested deeper than %d parts", maxBodyStructureDepth)
	}
	var bs imap.BodyStructure
	first := true
	err := dec.ExpectList(func() error {
		if !first {
			if bs == nil {
				return fmt.Errorf("malformed BODYSTRUCTURE")
			}
			return dec.DiscardValue()
		}
		first = false
		if dec.PeekSpecial('(') {
			mp, err := readMultiPart(dec, depth)
			bs = mp
			return err
		}
		sp, err := readSinglePart(dec, depth)
		bs = sp
		return err
	})
	if err != nil {
		return nil, err
	}
	if bs == nil {
		return nil, fmt.Errorf("empty BODYSTRUCTURE")
	}
	return bs, nil
}

// WriteBodyStructure encodes one body production.
func WriteBodyStructure(enc *imapwire.Encoder, bs imap.BodyStructure) {
	WriteBody(enc, bs, true)
}

// WriteBody encodes one BODY (extended false) or BODYSTRUCTURE (extended true)
// body production. BODY omits extension data recursively even when the shared
// value carries it.
func WriteBody(enc *imapwire.Encoder, bs imap.BodyStructure, extended bool) {
	writeBodyStructure(enc, bs, 0, extended)
}

func writeBodyStructure(enc *imapwire.Encoder, bs imap.BodyStructure, depth int, extended bool) {
	if depth > maxBodyStructureDepth {
		enc.Atom("")
		return
	}
	switch bs := bs.(type) {
	case *imap.BodyStructureSinglePart:
		writeSinglePart(enc, bs, depth, extended)
	case *imap.BodyStructureMultiPart:
		writeMultiPart(enc, bs, depth, extended)
	default:
		enc.Atom("")
	}
}

func readMultiPart(dec *imapwire.Decoder, depth int) (*imap.BodyStructureMultiPart, error) {
	mp := &imap.BodyStructureMultiPart{}
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

func writeMultiPart(enc *imapwire.Encoder, mp *imap.BodyStructureMultiPart, depth int, extended bool) {
	if mp == nil || len(mp.Children) == 0 {
		enc.Atom("")
		return
	}
	enc.Special('(')
	for _, child := range mp.Children {
		writeBodyStructure(enc, child, depth+1, extended)
	}
	enc.SP().String(mp.Subtype)
	if ext := mp.Extended; extended && ext != nil {
		enc.SP()
		writeBodyParams(enc, ext.Params)
		enc.SP()
		writeBodyDisposition(enc, ext.Disp)
		enc.SP()
		writeBodyLang(enc, ext.Lang)
		enc.SP().NString(ext.Location, ext.Location == "")
	}
	enc.Special(')')
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
		if msg.Envelope, err = ReadEnvelope(dec); err != nil {
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

func writeSinglePart(enc *imapwire.Encoder, sp *imap.BodyStructureSinglePart, depth int, extended bool) {
	if sp == nil {
		enc.Atom("")
		return
	}
	enc.Special('(').String(sp.Type).SP().String(sp.Subtype).SP()
	writeBodyParams(enc, sp.Params)
	enc.SP().NString(sp.ID, sp.ID == "")
	enc.SP().NString(encodeHeader(sp.Description), sp.Description == "")
	enc.SP().NString(sp.Encoding, sp.Encoding == "")
	enc.SP().Number(sp.Size)
	switch {
	case strings.EqualFold(sp.Type, "message") && strings.EqualFold(sp.Subtype, "rfc822"):
		if sp.Message == nil {
			enc.Atom("")
			break
		}
		enc.SP()
		WriteEnvelope(enc, sp.Message.Envelope)
		enc.SP()
		writeBodyStructure(enc, sp.Message.BodyStructure, depth+1, extended)
		enc.SP().Number64(sp.Message.NumLines)
	case strings.EqualFold(sp.Type, "text"):
		if sp.Text == nil {
			enc.Atom("")
			break
		}
		enc.SP().Number64(sp.Text.NumLines)
	}
	if ext := sp.Extended; extended && ext != nil {
		enc.SP().NString(ext.MD5, ext.MD5 == "")
		enc.SP()
		writeBodyDisposition(enc, ext.Disp)
		enc.SP()
		writeBodyLang(enc, ext.Lang)
		enc.SP().NString(ext.Location, ext.Location == "")
	}
	enc.Special(')')
}

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

func writeBodyParams(enc *imapwire.Encoder, params map[string]string) {
	if len(params) == 0 {
		enc.NIL()
		return
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	enc.Special('(')
	for i, k := range keys {
		if i > 0 {
			enc.SP()
		}
		enc.String(k).SP().String(params[k])
	}
	enc.Special(')')
}

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
		case 1:
			params, err := readBodyParams(dec)
			if err != nil {
				return err
			}
			disp.Params = params
		default:
			return dec.DiscardValue()
		}
		return nil
	})
	return disp, err
}

func writeBodyDisposition(enc *imapwire.Encoder, disp *imap.BodyStructureDisposition) {
	if disp == nil {
		enc.NIL()
		return
	}
	enc.Special('(').String(disp.Value).SP()
	writeBodyParams(enc, disp.Params)
	enc.Special(')')
}

func readBodyLang(dec *imapwire.Decoder) ([]string, error) {
	if !dec.PeekSpecial('(') {
		value, err := readNString(dec)
		if err != nil || value == "" {
			return nil, err
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
	return langs, err
}

func writeBodyLang(enc *imapwire.Encoder, langs []string) {
	switch len(langs) {
	case 0:
		enc.NIL()
	case 1:
		enc.String(langs[0])
	default:
		enc.List(len(langs), func(i int) { enc.String(langs[i]) })
	}
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
