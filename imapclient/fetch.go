package imapclient

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// FetchCommand is an in-flight FETCH or UID FETCH command.
//
// Next yields each matching untagged FETCH response. A response containing a
// body section is yielded as soon as its literal has been announced: consume
// or discard the section before calling Next again. This deliberate
// back-pressure keeps large messages out of memory and makes abandoning a
// section a clear protocol error instead of silently desynchronising the
// connection.
type FetchCommand struct {
	*Command
	responses chan *imap.FetchMessageData

	// stop is closed once the command has completed, and is never sent on.
	// The response channel itself must never be closed: closing it would race
	// with the reader goroutine's send whenever a caller stops consuming a
	// command that the connection then completes — an abandoned FETCH, or a
	// cancelled Wait — and a send on a closed channel panics on a goroutine
	// the caller cannot recover.
	stop chan struct{}
}

// fetchLiteralReader couples the decoder's literal interlock to the FETCH
// collector. The collector must not parse the rest of the response until the
// caller consumed this reader; otherwise Decoder correctly reports an
// undrained literal before the caller gets a chance to read it.
type fetchLiteralReader struct {
	literal   *imapwire.LiteralReader
	remaining int64
	done      chan struct{}
	once      sync.Once
}

func newFetchLiteralReader(literal *imapwire.LiteralReader) *fetchLiteralReader {
	r := &fetchLiteralReader{literal: literal, remaining: literal.Size(), done: make(chan struct{})}
	if r.remaining == 0 {
		r.finish()
	}
	return r
}

func (r *fetchLiteralReader) finish() { r.once.Do(func() { close(r.done) }) }

func (r *fetchLiteralReader) Read(p []byte) (int, error) {
	n, err := r.literal.Read(p)
	r.remaining -= int64(n)
	if r.remaining == 0 {
		r.finish()
	}
	return n, err
}

func (r *fetchLiteralReader) Close() error {
	err := r.literal.Discard()
	r.remaining = 0
	r.finish()
	return err
}

// Next waits for the next FETCH response claimed by this command. It returns
// io.EOF once the command completed and every response was delivered.
func (cmd *FetchCommand) Next(ctx context.Context) (*imap.FetchMessageData, error) {
	if cmd == nil || cmd.Command == nil {
		return nil, fmt.Errorf("imapclient: nil fetch command")
	}
	select {
	case data := <-cmd.responses:
		return data, nil
	case <-cmd.stop:
		// Responses are sent before the tagged completion is processed, and
		// both happen on the reader goroutine, so nothing is left in flight
		// once the command is done. Prefer a pending response anyway, in case
		// this select saw both cases ready at once.
		select {
		case data := <-cmd.responses:
			return data, nil
		default:
		}
		if err := cmd.Command.err; err != nil {
			return nil, err
		}
		return nil, io.EOF
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Fetch issues FETCH for a sequence-number set. To fetch by UID, use
// [Client.FetchUID]; the distinct methods make mixing the two address spaces
// impossible at the call site.
func (c *Client) Fetch(set imap.SeqSet, items ...imap.FetchItem) *FetchCommand {
	return c.fetch("FETCH", set.String(), func(n imap.SeqNum) bool { return set.Contains(n) }, items)
}

// FetchUID issues UID FETCH for a UID set. FETCH responses still carry the
// server's sequence number; include [imap.FetchItemUID] when the UID is needed
// in the returned data.
func (c *Client) FetchUID(set imap.UIDSet, items ...imap.FetchItem) *FetchCommand {
	return c.fetch("UID FETCH", set.String(), func(imap.SeqNum) bool { return true }, items)
}

func (c *Client) fetch(name, set string, matches func(imap.SeqNum) bool, items []imap.FetchItem) *FetchCommand {
	fc := &FetchCommand{responses: make(chan *imap.FetchMessageData), stop: make(chan struct{})}
	if set == "" || len(items) == 0 {
		fc.Command = rejectedCommand(c, name, "FETCH requires a non-empty set and at least one item")
		close(fc.stop)
		return fc
	}
	if err := validateFetchItems(items); err != nil {
		fc.Command = rejectedCommand(c, name, err.Error())
		close(fc.stop)
		return fc
	}
	fc.Command = c.beginCommand(name, stateSelected, func(enc *imapwire.Encoder) {
		enc.SP().Atom(set).SP().List(len(items), func(i int) { writeFetchItem(enc, items[i]) })
	}, func(resp *untaggedResponse) (bool, error) {
		if !resp.hasNum || resp.name != "FETCH" || !matches(imap.SeqNum(resp.number)) {
			return false, nil
		}
		return true, readFetchResponse(resp, fc.deliver)
	})
	go func() {
		<-fc.Command.done
		close(fc.stop)
	}()
	return fc
}

// deliver hands one response to the caller's Next. It gives up once the command
// has completed, which is what stops the reader goroutine from blocking forever
// on a FETCH nobody is consuming.
func (fc *FetchCommand) deliver(data *imap.FetchMessageData) {
	select {
	case fc.responses <- data:
	case <-fc.stop:
	}
}

func validateFetchItems(items []imap.FetchItem) error {
	for _, item := range items {
		switch v := item.(type) {
		case nil:
			return fmt.Errorf("nil FETCH item")
		case *imap.FetchItemBodySection:
			if v == nil {
				return fmt.Errorf("nil BODY section item")
			}
			if len(v.HeaderFields) != 0 && len(v.HeaderFieldsNot) != 0 {
				return fmt.Errorf("BODY section cannot use both HeaderFields and HeaderFieldsNot")
			}
			if err := validateSection(v.Part, v.Partial); err != nil {
				return err
			}
		case *imap.FetchItemBinarySection:
			if v == nil {
				return fmt.Errorf("nil BINARY section item")
			}
			if err := validateSection(v.Part, v.Partial); err != nil {
				return err
			}
		case *imap.FetchItemBinarySectionSize:
			if v == nil {
				return fmt.Errorf("nil BINARY.SIZE item")
			}
			if err := validateSection(v.Part, nil); err != nil {
				return err
			}
		case *imap.FetchItemBodyStructure:
			if v == nil {
				return fmt.Errorf("nil BODY item")
			}
		case *imap.FetchItemPreview:
			if v == nil {
				return fmt.Errorf("nil PREVIEW item")
			}
		}
	}
	return nil
}

func validateSection(part []int, partial *imap.SectionPartial) error {
	for _, n := range part {
		if n <= 0 {
			return fmt.Errorf("BODY part numbers must be positive")
		}
	}
	if partial != nil && (partial.Offset < 0 || partial.Size <= 0 || partial.Offset > int64(^uint32(0)) || partial.Size > int64(^uint32(0))) {
		return fmt.Errorf("invalid BODY partial range")
	}
	return nil
}

func rejectedCommand(c *Client, name, text string) *Command {
	cmd := &Command{client: c, name: name, done: make(chan struct{})}
	cmd.complete(&imap.Error{Type: imap.ErrorTypeProtocol, Text: text})
	return cmd
}

func writeFetchItem(enc *imapwire.Encoder, item imap.FetchItem) {
	switch item := item.(type) {
	case imap.FetchItemKeyword:
		enc.Atom(string(item))
	case *imap.FetchItemBodyStructure:
		if item == nil {
			enc.Atom("")
		} else if item.Extended {
			enc.Atom("BODYSTRUCTURE")
		} else {
			enc.Atom("BODY")
		}
	case *imap.FetchItemBodySection:
		writeBodySection(enc, "BODY", item.Part, item.Specifier, item.HeaderFields, item.HeaderFieldsNot, item.Partial, item.Peek)
	case *imap.FetchItemBinarySection:
		if item == nil {
			enc.Atom("")
			return
		}
		partial := toWirePartial(item.Partial)
		name := "BINARY"
		if item.Peek {
			name += ".PEEK"
		}
		enc.Atom(name).BodySection(&imapwire.BodySection{Part: toWirePart(item.Part), Partial: partial})
	case *imap.FetchItemBinarySectionSize:
		if item == nil {
			enc.Atom("")
		} else {
			enc.Atom("BINARY.SIZE").BodySection(&imapwire.BodySection{Part: toWirePart(item.Part)})
		}
	case *imap.FetchItemPreview:
		enc.Atom("PREVIEW")
		if item != nil && item.Lazy {
			enc.SP().Atom("LAZY")
		}
	default:
		enc.Atom("")
	}
}

func writeBodySection(enc *imapwire.Encoder, base string, part []int, spec imap.PartSpecifier, fields, fieldsNot []string, partial *imap.SectionPartial, peek bool) {
	name := base
	if peek {
		name += ".PEEK"
	}
	ws := &imapwire.BodySection{Part: toWirePart(part), Partial: toWirePartial(partial)}
	if len(fields) > 0 {
		ws.Specifier, ws.Fields = imapwire.SpecifierHeaderFields, fields
	} else if len(fieldsNot) > 0 {
		ws.Specifier, ws.Fields = imapwire.SpecifierHeaderFieldsNot, fieldsNot
	} else {
		ws.Specifier = string(spec)
	}
	enc.Atom(name).BodySection(ws)
}

func toWirePart(part []int) []uint32 {
	result := make([]uint32, len(part))
	for i, n := range part {
		result[i] = uint32(n)
	}
	return result
}

func toWirePartial(p *imap.SectionPartial) *imapwire.SectionPartial {
	if p == nil {
		return nil
	}
	return &imapwire.SectionPartial{Offset: uint32(p.Offset), Count: uint32(p.Size)}
}

func readFetchResponse(resp *untaggedResponse, emit func(*imap.FetchMessageData)) error {
	dec := resp.dec
	if !dec.ExpectSP() {
		return dec.Err()
	}
	data := &imap.FetchMessageData{SeqNum: imap.SeqNum(resp.number), Items: make(map[imap.FetchDataKey][]imap.FetchData)}
	emitted := false
	emitNow := func() {
		if !emitted {
			emitted = true
			emit(data)
		}
	}
	err := dec.ExpectList(func() error {
		var key string
		if !dec.ExpectFetchItemName(&key) {
			return dec.Err()
		}
		upper := strings.ToUpper(key)
		if upper == "BINARY.SIZE" {
			var section imapwire.BodySection
			if !dec.ExpectBodySection(&section) || !dec.ExpectSP() {
				return dec.Err()
			}
			var n int64
			if !dec.ExpectNumber64(&n) {
				return dec.Err()
			}
			key = formatSectionKey(key, &section)
			data.Items[imap.FetchDataKey(key)] = append(data.Items[imap.FetchDataKey(key)], imap.FetchDataBinarySectionSize(n))
			return nil
		}
		if (upper == "BODY" || upper == "BINARY") && dec.PeekSpecial('[') {
			var section imapwire.BodySection
			if !dec.ExpectBodySection(&section) {
				return dec.Err()
			}
			key = formatSectionKey(key, &section)
			if !dec.ExpectSP() {
				return dec.Err()
			}
			var literal io.Reader
			var drained <-chan struct{}
			lr, ok := dec.Literal()
			if ok {
				stream := newFetchLiteralReader(lr)
				literal, drained = stream, stream.done
			} else {
				var token string
				if !dec.ExpectAtom(&token) {
					return dec.Err()
				}
				if !strings.EqualFold(token, "NIL") {
					return fmt.Errorf("FETCH %s value is neither literal nor NIL", key)
				}
				literal = strings.NewReader("")
			}
			var value imap.FetchData
			if upper == "BODY" {
				value = bodySectionData(&section, literal)
			} else {
				value = binarySectionData(&section, literal)
			}
			data.Items[imap.FetchDataKey(key)] = append(data.Items[imap.FetchDataKey(key)], value)
			emitNow()
			if drained != nil {
				<-drained
			}
			return nil
		}
		if !dec.ExpectSP() {
			return dec.Err()
		}
		var value imap.FetchData
		switch upper {
		case "UID":
			var n uint32
			if !dec.ExpectUniqueID(&n) {
				return dec.Err()
			}
			value = imap.FetchDataUID(n)
		case "FLAGS":
			var flags []string
			if err := dec.ExpectFlagList(&flags); err != nil {
				return err
			}
			v := make(imap.FetchDataFlags, len(flags))
			for i := range flags {
				v[i] = imap.Flag(flags[i])
			}
			value = v
		case "INTERNALDATE":
			var t time.Time
			if !dec.ExpectDateTime(&t) {
				return dec.Err()
			}
			value = &imap.FetchDataInternalDate{Time: t}
		case "RFC822.SIZE":
			var n int64
			if !dec.ExpectNumber64(&n) {
				return dec.Err()
			}
			value = imap.FetchDataRFC822Size(n)
		case "RFC822", "RFC822.HEADER", "RFC822.TEXT":
			lr, ok := dec.Literal()
			if !ok {
				return dec.Err()
			}
			stream := newFetchLiteralReader(lr)
			value = &imap.FetchDataLiteral{Literal: stream}
			data.Items[imap.FetchDataKey(key)] = append(data.Items[imap.FetchDataKey(key)], value)
			emitNow()
			<-stream.done
			return nil
		case "MODSEQ":
			var n int64
			if err := dec.ExpectList(func() error {
				if !dec.ExpectNumber64(&n) {
					return dec.Err()
				}
				return nil
			}); err != nil {
				return err
			}
			value = imap.FetchDataModSeq(n)
		case "EMAILID", "THREADID":
			var v string
			if !dec.ExpectAstring(&v) {
				return dec.Err()
			}
			value = imap.FetchDataObjectID(v)
		case "ENVELOPE":
			env, err := readEnvelope(dec)
			if err != nil {
				return err
			}
			value = &imap.FetchDataEnvelope{Envelope: env}
		case "BODY", "BODYSTRUCTURE":
			// Reached only when no section followed the keyword, so this is the
			// body structure form. BODY omits the extension fields that
			// BODYSTRUCTURE carries; the grammar is otherwise identical.
			bs, err := readBodyStructure(dec, 0)
			if err != nil {
				return err
			}
			value = &imap.FetchDataBodyStructure{BodyStructure: bs}
		default:
			// An item from an extension this client does not model is kept
			// verbatim rather than skipped: dropping it silently is data loss,
			// and the caller may well understand what we do not. A value too
			// large to hold in memory is the one case where it is skipped, and
			// the empty reader then says so.
			var raw []byte
			if err := dec.CaptureValue(&raw); err != nil {
				if dec.Err() != nil {
					return err
				}
				raw = nil
			}
			value = &imap.FetchDataRaw{Reader: bytes.NewReader(raw)}
		}
		data.Items[imap.FetchDataKey(key)] = append(data.Items[imap.FetchDataKey(key)], value)
		return nil
	})
	if err != nil {
		return err
	}
	if !dec.ExpectCRLF() {
		return dec.Err()
	}
	emitNow()
	return nil
}

func bodySectionData(section *imapwire.BodySection, literal io.Reader) imap.FetchData {
	v := &imap.FetchDataBodySection{Part: make([]int, len(section.Part)), Specifier: imap.PartSpecifier(section.Specifier), Literal: literal}
	for i, n := range section.Part {
		v.Part[i] = int(n)
	}
	if section.Specifier == imapwire.SpecifierHeaderFields {
		v.HeaderFields = append([]string(nil), section.Fields...)
	}
	if section.Specifier == imapwire.SpecifierHeaderFieldsNot {
		v.HeaderFieldsNot = append([]string(nil), section.Fields...)
	}
	if section.Partial != nil {
		v.Origin, v.HasOrigin = int64(section.Partial.Offset), true
	}
	return v
}

func binarySectionData(section *imapwire.BodySection, literal io.Reader) imap.FetchData {
	v := &imap.FetchDataBinarySection{Part: make([]int, len(section.Part)), Literal: literal}
	for i, n := range section.Part {
		v.Part[i] = int(n)
	}
	if section.Partial != nil {
		v.Origin, v.HasOrigin = int64(section.Partial.Offset), true
	}
	return v
}

func formatSectionKey(prefix string, section *imapwire.BodySection) string {
	var b strings.Builder
	b.WriteString(prefix)
	b.WriteByte('[')
	for i, n := range section.Part {
		if i > 0 {
			b.WriteByte('.')
		}
		fmt.Fprint(&b, n)
	}
	if section.Specifier != "" {
		if len(section.Part) > 0 {
			b.WriteByte('.')
		}
		b.WriteString(section.Specifier)
	}
	if len(section.Fields) > 0 {
		b.WriteByte(' ')
		b.WriteByte('(')
		b.WriteString(strings.Join(section.Fields, " "))
		b.WriteByte(')')
	}
	b.WriteByte(']')
	if section.Partial != nil {
		fmt.Fprintf(&b, "<%d>", section.Partial.Offset)
	}
	return b.String()
}
