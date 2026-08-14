package imapcodec

import (
	"bytes"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// LiteralAdapter turns an imapwire literal into the reader exposed in a FETCH
// value. wait, when non-nil, is called before semantic decoding resumes. The
// client uses it to preserve streaming back-pressure.
type LiteralAdapter func(*imapwire.LiteralReader) (reader io.Reader, wait func())

// ReadFetchResponse decodes the part of an untagged FETCH response following
// its FETCH atom, including the leading space and trailing CRLF.
func ReadFetchResponse(dec *imapwire.Decoder, seqNum imap.SeqNum, adapt LiteralAdapter, emit func(*imap.FetchMessageData)) error {
	if !dec.ExpectSP() {
		return dec.Err()
	}
	data := &imap.FetchMessageData{SeqNum: seqNum, Items: make(map[imap.FetchDataKey][]imap.FetchData)}
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
			key = FormatSectionKey(key, &section)
			data.Items[imap.FetchDataKey(key)] = append(data.Items[imap.FetchDataKey(key)], imap.FetchDataBinarySectionSize(n))
			return nil
		}
		if (upper == "BODY" || upper == "BINARY") && dec.PeekSpecial('[') {
			var section imapwire.BodySection
			if !dec.ExpectBodySection(&section) {
				return dec.Err()
			}
			key = FormatSectionKey(key, &section)
			if !dec.ExpectSP() {
				return dec.Err()
			}
			literal, wait, err := readFetchLiteral(dec, adapt, true)
			if err != nil {
				return fmt.Errorf("FETCH %s: %w", key, err)
			}
			var value imap.FetchData
			if upper == "BODY" {
				value = BodySectionData(&section, literal)
			} else {
				value = BinarySectionData(&section, literal)
			}
			data.Items[imap.FetchDataKey(key)] = append(data.Items[imap.FetchDataKey(key)], value)
			emitNow()
			if wait != nil {
				wait()
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
			literal, wait, err := readFetchLiteral(dec, adapt, false)
			if err != nil {
				return err
			}
			value = &imap.FetchDataLiteral{Literal: literal}
			data.Items[imap.FetchDataKey(key)] = append(data.Items[imap.FetchDataKey(key)], value)
			emitNow()
			if wait != nil {
				wait()
			}
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
		case "EMAILID":
			id, err := ReadFetchObjectID(dec)
			if err != nil {
				return err
			}
			value = imap.FetchDataObjectID(id)
		case "THREADID":
			if dec.PeekSpecial('(') {
				id, err := ReadFetchObjectID(dec)
				if err != nil {
					return err
				}
				value = imap.FetchDataObjectID(id)
			} else {
				var unused string
				var isNil bool
				if !dec.ExpectNString(&unused, &isNil) || !isNil {
					return fmt.Errorf("THREADID value is neither (objectid) nor NIL")
				}
				value = imap.FetchDataObjectID("")
			}
		case "SAVEDATE":
			if dec.PeekSpecial('"') {
				var t time.Time
				if !dec.ExpectDateTime(&t) {
					return dec.Err()
				}
				value = &imap.FetchDataSaveDate{Date: &t}
			} else {
				var unused string
				var isNil bool
				if !dec.ExpectNString(&unused, &isNil) || !isNil {
					return fmt.Errorf("SAVEDATE value is neither date-time nor NIL")
				}
				value = &imap.FetchDataSaveDate{}
			}
		case "PREVIEW":
			var s string
			var isNil bool
			if !dec.ExpectNString(&s, &isNil) {
				return dec.Err()
			}
			if isNil {
				value = &imap.FetchDataPreview{}
			} else {
				value = &imap.FetchDataPreview{Text: &s}
			}
		case "ENVELOPE":
			env, err := ReadEnvelope(dec)
			if err != nil {
				return err
			}
			value = &imap.FetchDataEnvelope{Envelope: env}
		case "BODY", "BODYSTRUCTURE":
			bs, err := ReadBodyStructure(dec)
			if err != nil {
				return err
			}
			value = &imap.FetchDataBodyStructure{BodyStructure: bs}
		default:
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

func readFetchLiteral(dec *imapwire.Decoder, adapt LiteralAdapter, allowNil bool) (io.Reader, func(), error) {
	lr, ok := dec.Literal()
	if !ok {
		if !allowNil {
			return nil, nil, dec.Err()
		}
		var token string
		if !dec.ExpectAtom(&token) {
			return nil, nil, dec.Err()
		}
		if !strings.EqualFold(token, "NIL") {
			return nil, nil, fmt.Errorf("value is neither literal nor NIL")
		}
		return strings.NewReader(""), nil, nil
	}
	if adapt != nil {
		reader, wait := adapt(lr)
		if reader == nil {
			return nil, nil, fmt.Errorf("literal adapter returned a nil reader")
		}
		return reader, wait, nil
	}
	b, err := io.ReadAll(lr)
	if err != nil {
		return nil, nil, err
	}
	return bytes.NewReader(b), nil, nil
}

// ReadFetchObjectID decodes the parenthesised OBJECTID response value.
func ReadFetchObjectID(dec *imapwire.Decoder) (string, error) {
	var id string
	if err := dec.ExpectList(func() error {
		if !dec.ExpectAtom(&id) {
			return dec.Err()
		}
		return nil
	}); err != nil {
		return "", err
	}
	if err := validateObjectID(id); err != nil {
		return "", err
	}
	return id, nil
}

func validateObjectID(id string) error {
	if id == "" || len(id) > 255 {
		return fmt.Errorf("object identifier %q is not 1-255 characters", id)
	}
	for i := range len(id) {
		b := id[i]
		if !((b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_' || b == '-') {
			return fmt.Errorf("object identifier %q contains an illegal character", id)
		}
	}
	return nil
}

// BodySectionData converts the wire section descriptor to the shared value.
func BodySectionData(section *imapwire.BodySection, literal io.Reader) imap.FetchData {
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

// BinarySectionData converts the wire section descriptor to the shared value.
func BinarySectionData(section *imapwire.BodySection, literal io.Reader) imap.FetchData {
	v := &imap.FetchDataBinarySection{Part: make([]int, len(section.Part)), Literal: literal}
	for i, n := range section.Part {
		v.Part[i] = int(n)
	}
	if section.Partial != nil {
		v.Origin, v.HasOrigin = int64(section.Partial.Offset), true
	}
	return v
}

// FormatSectionKey returns the complete response item key for section.
func FormatSectionKey(prefix string, section *imapwire.BodySection) string {
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

// LiteralSizer supplies a size for a streaming FETCH value when its reader
// does not expose a Len method.
type LiteralSizer func(key imap.FetchDataKey, value imap.FetchData) (int64, error)

type fetchEntry struct {
	key   imap.FetchDataKey
	value imap.FetchData
	size  int64
}

// WriteFetchResponse writes one complete untagged FETCH response.
func WriteFetchResponse(enc *imapwire.Encoder, data *imap.FetchMessageData, size LiteralSizer) error {
	if data == nil || data.SeqNum == 0 {
		return fmt.Errorf("FETCH response requires a non-zero sequence number")
	}
	entries, err := fetchEntries(data, size)
	if err != nil {
		return err
	}
	enc.BeginResponse(imapwire.ResponseUntagged, "").Number(uint32(data.SeqNum)).SP().Atom("FETCH").SP()
	return writeFetchEntries(enc, entries)
}

// WriteUIDFetchResponse writes the UIDFETCH response form of UIDONLY, RFC 9586
// section 3.2: the message is identified by UID and no sequence number appears.
//
// It shares the item encoding with WriteFetchResponse rather than duplicating
// it, because the item grammar is identical between the two and two copies of it
// would drift.
func WriteUIDFetchResponse(enc *imapwire.Encoder, uid imap.UID, data *imap.FetchMessageData, size LiteralSizer) error {
	if data == nil {
		return fmt.Errorf("UIDFETCH response requires data")
	}
	if uid == 0 {
		return fmt.Errorf("UIDFETCH response requires a non-zero UID")
	}
	entries, err := fetchEntries(data, size)
	if err != nil {
		return err
	}
	enc.BeginResponse(imapwire.ResponseUntagged, "").Atom("UIDFETCH").SP().Number(uint32(uid)).SP()
	return writeFetchEntries(enc, entries)
}

// fetchEntries resolves a response's items into wire entries, measuring every
// literal so the encoder can announce its length.
func fetchEntries(data *imap.FetchMessageData, size LiteralSizer) ([]fetchEntry, error) {
	keys := make([]string, 0, len(data.Items))
	for key := range data.Items {
		keys = append(keys, string(key))
	}
	sort.Strings(keys)
	var entries []fetchEntry
	for _, rawKey := range keys {
		key := imap.FetchDataKey(rawKey)
		if !validFetchDataKey(rawKey) {
			return nil, fmt.Errorf("invalid FETCH data key %q", rawKey)
		}
		for _, value := range data.Items[key] {
			entry := fetchEntry{key: key, value: value, size: -1}
			if reader := fetchDataReader(value); reader != nil {
				if n, ok := reader.(interface{ Len() int }); ok {
					entry.size = int64(n.Len())
				} else if size != nil {
					var err error
					entry.size, err = size(key, value)
					if err != nil {
						return nil, err
					}
				}
				if entry.size < 0 {
					return nil, fmt.Errorf("FETCH %s literal size is unknown", key)
				}
			}
			entries = append(entries, entry)
		}
	}
	return entries, nil
}

// writeFetchEntries writes the parenthesised item list shared by the FETCH and
// UIDFETCH response forms. The caller has already written the response prefix.
func writeFetchEntries(enc *imapwire.Encoder, entries []fetchEntry) error {
	enc.Special('(')
	for i, entry := range entries {
		if i > 0 {
			enc.SP()
		}
		enc.RawValue([]byte(entry.key)).SP()
		if err := writeFetchData(enc, entry, strings.ToUpper(string(entry.key))); err != nil {
			return err
		}
	}
	enc.Special(')').CRLF()
	return enc.Err()
}

func validFetchDataKey(key string) bool {
	if key == "" {
		return false
	}
	dec := imapwire.NewDecoderString(key, nil)
	var name string
	if !dec.ExpectFetchItemName(&name) {
		return false
	}
	upper := strings.ToUpper(name)
	if (upper == "BODY" || upper == "BODY.PEEK" || upper == "BINARY" || upper == "BINARY.PEEK" || upper == "BINARY.SIZE") && dec.PeekSpecial('[') {
		var section imapwire.BodySection
		if !dec.ExpectBodySection(&section) {
			return false
		}
	}
	return dec.AtEOF()
}

func fetchDataReader(value imap.FetchData) io.Reader {
	switch value := value.(type) {
	case *imap.FetchDataLiteral:
		return value.Literal
	case *imap.FetchDataBodySection:
		return value.Literal
	case *imap.FetchDataBinarySection:
		return value.Literal
	}
	return nil
}

func writeFetchData(enc *imapwire.Encoder, entry fetchEntry, upperKey string) error {
	switch value := entry.value.(type) {
	case imap.FetchDataUID:
		enc.Number(uint32(value))
	case imap.FetchDataFlags:
		enc.List(len(value), func(i int) { enc.Flag(string(value[i])) })
	case *imap.FetchDataInternalDate:
		if value == nil {
			return fmt.Errorf("FETCH INTERNALDATE has nil value")
		}
		enc.DateTime(value.Time)
	case imap.FetchDataRFC822Size:
		enc.Number64(int64(value))
	case *imap.FetchDataEnvelope:
		if value == nil {
			return fmt.Errorf("FETCH ENVELOPE has nil value")
		}
		WriteEnvelope(enc, value.Envelope)
	case *imap.FetchDataBodyStructure:
		if value == nil {
			return fmt.Errorf("FETCH BODYSTRUCTURE has nil value")
		}
		WriteBody(enc, value.BodyStructure, strings.HasPrefix(upperKey, "BODYSTRUCTURE"))
	case *imap.FetchDataLiteral:
		return writeFetchLiteral(enc, value.Literal, entry.size, false)
	case *imap.FetchDataBodySection:
		return writeFetchLiteral(enc, value.Literal, entry.size, false)
	case *imap.FetchDataBinarySection:
		return writeFetchLiteral(enc, value.Literal, entry.size, true)
	case imap.FetchDataBinarySectionSize:
		enc.Number64(int64(value))
	case imap.FetchDataModSeq:
		if uint64(value) > 1<<63-1 {
			return fmt.Errorf("FETCH MODSEQ exceeds 63 bits")
		}
		enc.List(1, func(int) { enc.Number64(int64(value)) })
	case imap.FetchDataObjectID:
		if value == "" && strings.HasPrefix(upperKey, "THREADID") {
			enc.NIL()
		} else {
			if err := validateObjectID(string(value)); err != nil {
				return err
			}
			enc.List(1, func(int) { enc.Atom(string(value)) })
		}
	case *imap.FetchDataSaveDate:
		if value == nil || value.Date == nil {
			enc.NIL()
		} else {
			enc.DateTime(*value.Date)
		}
	case *imap.FetchDataPreview:
		if value == nil || value.Text == nil {
			enc.NIL()
		} else {
			enc.String(*value.Text)
		}
	case *imap.FetchDataRaw:
		if value == nil {
			return fmt.Errorf("FETCH raw value is nil")
		}
		return enc.RawReader(value.Reader)
	default:
		return fmt.Errorf("unsupported FETCH data type %T", entry.value)
	}
	return enc.Err()
}

func writeFetchLiteral(enc *imapwire.Encoder, reader io.Reader, size int64, binary bool) error {
	if reader == nil {
		return fmt.Errorf("nil FETCH literal reader")
	}
	lw, err := enc.ResponseLiteral(size, binary)
	if err != nil {
		return err
	}
	if _, err := io.CopyN(lw, reader, size); err != nil {
		return err
	}
	return lw.Close()
}

// WriteFetchItem writes one FETCH request item.
func WriteFetchItem(enc *imapwire.Encoder, item imap.FetchItem) {
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
		if item == nil {
			enc.Atom("")
			return
		}
		writeBodySection(enc, "BODY", item.Part, item.Specifier, item.HeaderFields, item.HeaderFieldsNot, item.Partial, item.Peek)
	case *imap.FetchItemBinarySection:
		if item == nil {
			enc.Atom("")
			return
		}
		name := "BINARY"
		if item.Peek {
			name += ".PEEK"
		}
		enc.Atom(name).BodySection(&imapwire.BodySection{Part: toWirePart(item.Part), Partial: toWirePartial(item.Partial)})
	case *imap.FetchItemBinarySectionSize:
		if item == nil {
			enc.Atom("")
		} else {
			enc.Atom("BINARY.SIZE").BodySection(&imapwire.BodySection{Part: toWirePart(item.Part)})
		}
	case *imap.FetchItemPreview:
		enc.Atom("PREVIEW")
		if item != nil && item.Lazy {
			enc.SP().List(1, func(int) { enc.Atom("LAZY") })
		}
	default:
		enc.Atom("")
	}
}

// ReadFetchItems decodes a parenthesised FETCH item list.
func ReadFetchItems(dec *imapwire.Decoder) ([]imap.FetchItem, error) {
	var items []imap.FetchItem
	err := dec.ExpectList(func() error {
		item, err := ReadFetchItem(dec)
		if err == nil {
			items = append(items, item)
		}
		return err
	})
	return items, err
}

// ReadFetchItem decodes exactly one FETCH request item.
func ReadFetchItem(dec *imapwire.Decoder) (imap.FetchItem, error) {
	var name string
	if !dec.ExpectFetchItemName(&name) {
		return nil, dec.Err()
	}
	upper := strings.ToUpper(name)
	switch upper {
	case "BODY", "BODYSTRUCTURE":
		if !dec.PeekSpecial('[') {
			return &imap.FetchItemBodyStructure{Extended: upper == "BODYSTRUCTURE"}, nil
		}
	case "BODY.PEEK":
		if !dec.PeekSpecial('[') {
			return nil, fmt.Errorf("BODY.PEEK requires a section")
		}
	case "BINARY", "BINARY.PEEK":
		if !dec.PeekSpecial('[') {
			return nil, fmt.Errorf("%s requires a section", upper)
		}
	case "BINARY.SIZE":
		var section imapwire.BodySection
		if !dec.ExpectBodySection(&section) {
			return nil, dec.Err()
		}
		if section.Specifier != "" || section.Partial != nil {
			return nil, fmt.Errorf("BINARY.SIZE section has a specifier or partial")
		}
		return &imap.FetchItemBinarySectionSize{Part: fromWirePart(section.Part)}, nil
	case "PREVIEW":
		item := &imap.FetchItemPreview{}
		if dec.SPListAhead() {
			if !dec.ExpectSP() {
				return nil, dec.Err()
			}
			err := dec.ExpectList(func() error {
				var modifier string
				if !dec.ExpectAtom(&modifier) {
					return dec.Err()
				}
				if !strings.EqualFold(modifier, "LAZY") {
					return fmt.Errorf("unknown PREVIEW modifier %q", modifier)
				}
				item.Lazy = true
				return nil
			})
			if err != nil {
				return nil, err
			}
		}
		return item, nil
	default:
		return imap.FetchItemKeyword(upper), nil
	}

	var section imapwire.BodySection
	if !dec.ExpectBodySection(&section) {
		return nil, dec.Err()
	}
	partial := fromWirePartial(section.Partial)
	if strings.HasPrefix(upper, "BINARY") {
		if section.Specifier != "" || len(section.Fields) != 0 {
			return nil, fmt.Errorf("BINARY section has a body specifier")
		}
		return &imap.FetchItemBinarySection{
			Part:    fromWirePart(section.Part),
			Partial: partial,
			Peek:    upper == "BINARY.PEEK",
		}, nil
	}
	item := &imap.FetchItemBodySection{
		Part:      fromWirePart(section.Part),
		Specifier: imap.PartSpecifier(section.Specifier),
		Partial:   partial,
		Peek:      upper == "BODY.PEEK",
	}
	switch section.Specifier {
	case imapwire.SpecifierHeaderFields:
		item.Specifier = imap.PartSpecifierHeader
		item.HeaderFields = append([]string(nil), section.Fields...)
	case imapwire.SpecifierHeaderFieldsNot:
		item.Specifier = imap.PartSpecifierHeader
		item.HeaderFieldsNot = append([]string(nil), section.Fields...)
	}
	return item, nil
}

// WriteFetchItems encodes a parenthesised FETCH item list.
func WriteFetchItems(enc *imapwire.Encoder, items []imap.FetchItem) {
	enc.List(len(items), func(i int) { WriteFetchItem(enc, items[i]) })
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

func fromWirePart(part []uint32) []int {
	result := make([]int, len(part))
	for i, n := range part {
		result[i] = int(n)
	}
	return result
}

func toWirePartial(p *imap.SectionPartial) *imapwire.SectionPartial {
	if p == nil {
		return nil
	}
	return &imapwire.SectionPartial{Offset: uint32(p.Offset), Count: uint32(p.Size)}
}

func fromWirePartial(p *imapwire.SectionPartial) *imap.SectionPartial {
	if p == nil {
		return nil
	}
	return &imap.SectionPartial{Offset: int64(p.Offset), Size: int64(p.Count)}
}
