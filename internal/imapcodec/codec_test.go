package imapcodec

import (
	"bytes"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

func TestEnvelopeRoundTrip(t *testing.T) {
	want := &imap.Envelope{
		Date:      time.Date(2026, 8, 12, 9, 30, 0, 0, time.UTC),
		RawDate:   "Wed, 12 Aug 2026 09:30:00 +0000",
		Subject:   "Café status",
		From:      []imap.Address{{Name: "Ann Example", Mailbox: "ann", Host: "example.test"}},
		Sender:    []imap.Address{{Mailbox: "ann", Host: "example.test"}},
		ReplyTo:   []imap.Address{{Mailbox: "reply", Host: "example.test"}},
		To:        []imap.Address{{Mailbox: "Team", Host: ""}, {Mailbox: "member", Host: "example.test"}, {}},
		InReplyTo: []string{"<one@example.test>", "<two@example.test>"},
		MessageID: "<message@example.test>",
	}
	var buf bytes.Buffer
	enc := imapwire.NewEncoder(&buf, &imapwire.EncoderOptions{ServerResponse: true})
	WriteEnvelope(enc, want)
	if err := enc.Flush(); err != nil {
		t.Fatal(err)
	}
	got, err := ReadEnvelope(imapwire.NewDecoder(bytes.NewReader(buf.Bytes()), nil))
	if err != nil {
		t.Fatalf("decode %q: %v", buf.Bytes(), err)
	}
	if !got.Date.Equal(want.Date) {
		t.Fatalf("date = %v, want %v", got.Date, want.Date)
	}
	got.Date = want.Date
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip:\n got %#v\nwant %#v\nwire %q", got, want, buf.Bytes())
	}
}

func TestBodyStructureRoundTrip(t *testing.T) {
	want := &imap.BodyStructureMultiPart{
		Subtype: "mixed",
		Children: []imap.BodyStructure{
			&imap.BodyStructureSinglePart{
				Type: "text", Subtype: "plain", Params: map[string]string{"charset": "utf-8"},
				Encoding: "7bit", Size: 12, Text: &imap.BodyStructureText{NumLines: 2},
				Extended: &imap.BodyStructureSinglePartExt{Disp: &imap.BodyStructureDisposition{Value: "inline"}, Lang: []string{"en"}},
			},
			&imap.BodyStructureSinglePart{
				Type: "application", Subtype: "octet-stream", Params: map[string]string{"name": "data.bin"},
				Encoding: "base64", Size: 24,
				Extended: &imap.BodyStructureSinglePartExt{MD5: "abc", Disp: &imap.BodyStructureDisposition{Value: "attachment", Params: map[string]string{"filename": "data.bin"}}},
			},
		},
		Extended: &imap.BodyStructureMultiPartExt{Params: map[string]string{"boundary": "b"}, Lang: []string{"en", "de"}, Location: "cid:root"},
	}
	var buf bytes.Buffer
	enc := imapwire.NewEncoder(&buf, &imapwire.EncoderOptions{ServerResponse: true})
	WriteBodyStructure(enc, want)
	if err := enc.Flush(); err != nil {
		t.Fatal(err)
	}
	got, err := ReadBodyStructure(imapwire.NewDecoder(bytes.NewReader(buf.Bytes()), nil))
	if err != nil {
		t.Fatalf("decode %q: %v", buf.Bytes(), err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip:\n got %#v\nwant %#v\nwire %q", got, want, buf.Bytes())
	}
}

func TestWriteBodyOmitsExtensionsRecursively(t *testing.T) {
	value := &imap.BodyStructureMultiPart{
		Subtype: "mixed",
		Children: []imap.BodyStructure{&imap.BodyStructureSinglePart{
			Type: "text", Subtype: "plain", Encoding: "7bit", Text: &imap.BodyStructureText{},
			Extended: &imap.BodyStructureSinglePartExt{MD5: "child-md5"},
		}},
		Extended: &imap.BodyStructureMultiPartExt{Params: map[string]string{"boundary": "b"}},
	}
	var buf bytes.Buffer
	enc := imapwire.NewEncoder(&buf, &imapwire.EncoderOptions{ServerResponse: true})
	WriteBody(enc, value, false)
	if err := enc.Flush(); err != nil {
		t.Fatal(err)
	}
	wire := buf.String()
	if strings.Contains(wire, "child-md5") || strings.Contains(wire, "boundary") || strings.Contains(wire, `"b"`) {
		t.Fatalf("BODY leaked extension fields: %q", wire)
	}
	got, err := ReadBodyStructure(imapwire.NewDecoderString(wire, nil))
	if err != nil {
		t.Fatal(err)
	}
	mp := got.(*imap.BodyStructureMultiPart)
	if mp.Extended != nil || mp.Children[0].(*imap.BodyStructureSinglePart).Extended != nil {
		t.Fatalf("decoded BODY has extension data: %#v", got)
	}
}

func TestSearchCriteriaRoundTrip(t *testing.T) {
	date := time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)
	tests := []imap.SearchCriteria{
		imap.SearchAll,
		imap.SearchAnd{imap.SearchSeen, imap.SearchString{Key: imap.SearchKeySubject, Value: "status"}},
		imap.SearchOr{Left: imap.SearchNot{Criteria: imap.SearchDeleted}, Right: imap.SearchFuzzy{Criteria: imap.SearchString{Key: imap.SearchKeyText, Value: "needle"}}},
		imap.SearchFlagKeyword{Flag: "$Label", Not: true},
		imap.SearchHeaderField{Field: "X-Project", Value: "imap"},
		imap.SearchDate{Key: imap.SearchDateKeySince, Date: date},
		imap.SearchSize{Key: imap.SearchSizeKeyLarger, Size: 42},
		imap.SearchSeqNum{Set: imap.SeqSetRange(2, 0)},
		imap.SearchUID{Set: imap.UIDSetRange(10, 20)},
		imap.SearchSavedResult{},
		imap.SearchWithin{Key: imap.SearchWithinKeyYounger, Seconds: 60},
		imap.SearchObjectID{Key: imap.SearchObjectIDKeyEmail, Value: "abc_123"},
		imap.SearchModSeq{ModSeq: 99},
		imap.SearchModSeq{ModSeq: 100, EntryName: "/flags/\\draft", EntryType: imap.SearchModSeqMetadataPrivate},
	}
	for _, want := range tests {
		t.Run(reflect.TypeOf(want).String(), func(t *testing.T) {
			var buf bytes.Buffer
			enc := imapwire.NewEncoder(&buf, &imapwire.EncoderOptions{LiteralPlus: true})
			WriteSearchCriteria(enc, want)
			if err := enc.Flush(); err != nil {
				t.Fatal(err)
			}
			got, err := ReadSearchCriteria(imapwire.NewDecoder(bytes.NewReader(buf.Bytes()), nil))
			if err != nil {
				t.Fatalf("decode %q: %v", buf.Bytes(), err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("round trip:\n got %#v\nwant %#v\nwire %q", got, want, buf.Bytes())
			}
		})
	}
}

func TestFetchItemsRoundTrip(t *testing.T) {
	want := []imap.FetchItem{
		imap.FetchItemUID,
		&imap.FetchItemBodyStructure{Extended: true},
		&imap.FetchItemBodySection{Part: []int{1, 2}, Specifier: imap.PartSpecifierHeader, HeaderFields: []string{"From", "Subject"}, Partial: &imap.SectionPartial{Offset: 3, Size: 9}, Peek: true},
		&imap.FetchItemBinarySection{Part: []int{2}, Partial: &imap.SectionPartial{Offset: 1, Size: 4}, Peek: true},
		&imap.FetchItemBinarySectionSize{Part: []int{2}},
		&imap.FetchItemPreview{Lazy: true},
	}
	var buf bytes.Buffer
	enc := imapwire.NewEncoder(&buf, &imapwire.EncoderOptions{LiteralPlus: true})
	WriteFetchItems(enc, want)
	if err := enc.Flush(); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFetchItems(imapwire.NewDecoder(bytes.NewReader(buf.Bytes()), nil))
	if err != nil {
		t.Fatalf("decode %q: %v", buf.Bytes(), err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip:\n got %#v\nwant %#v\nwire %q", got, want, buf.Bytes())
	}
}

func TestFetchResponseRoundTrip(t *testing.T) {
	preview := "hello"
	want := &imap.FetchMessageData{SeqNum: 7, Items: map[imap.FetchDataKey][]imap.FetchData{
		"UID":           {imap.FetchDataUID(44)},
		"FLAGS":         {imap.FetchDataFlags{imap.FlagSeen, "$label"}},
		"RFC822.SIZE":   {imap.FetchDataRFC822Size(12)},
		"MODSEQ":        {imap.FetchDataModSeq(99)},
		"EMAILID":       {imap.FetchDataObjectID("abc_123")},
		"PREVIEW":       {&imap.FetchDataPreview{Text: &preview}},
		"BODY[TEXT]<0>": {&imap.FetchDataBodySection{Specifier: imap.PartSpecifierText, Origin: 0, HasOrigin: true, Literal: bytes.NewReader([]byte("hello"))}},
		"BINARY[1]":     {&imap.FetchDataBinarySection{Part: []int{1}, Literal: bytes.NewReader([]byte{0, 1, 2})}},
		"X-FUTURE":      {&imap.FetchDataRaw{Reader: strings.NewReader("(one two)")}},
	}}
	var buf bytes.Buffer
	enc := imapwire.NewEncoder(&buf, &imapwire.EncoderOptions{ServerResponse: true})
	if err := WriteFetchResponse(enc, want, nil); err != nil {
		t.Fatal(err)
	}
	if err := enc.Flush(); err != nil {
		t.Fatal(err)
	}
	dec := imapwire.NewDecoder(bytes.NewReader(buf.Bytes()), nil)
	kind, _, err := dec.BeginResponse()
	if err != nil || kind != imapwire.ResponseUntagged || !dec.ExpectSP() {
		t.Fatalf("framing: %v, %v", err, dec.Err())
	}
	var seq uint32
	var name string
	if !dec.ExpectNumber(&seq) || !dec.ExpectSP() || !dec.ExpectAtom(&name) || name != "FETCH" {
		t.Fatal(dec.Err())
	}
	var got *imap.FetchMessageData
	if err := ReadFetchResponse(dec, imap.SeqNum(seq), nil, func(data *imap.FetchMessageData) { got = data }); err != nil {
		t.Fatalf("decode %q: %v", buf.Bytes(), err)
	}
	if got == nil || got.SeqNum != want.SeqNum || len(got.Items) != len(want.Items) {
		t.Fatalf("response = %#v", got)
	}
	assertReaderValue(t, got.Items["BODY[TEXT]<0>"][0].(*imap.FetchDataBodySection).Literal, "hello")
	assertReaderValue(t, got.Items["BINARY[1]"][0].(*imap.FetchDataBinarySection).Literal, string([]byte{0, 1, 2}))
	assertReaderValue(t, got.Items["X-FUTURE"][0].(*imap.FetchDataRaw).Reader, "(one two)")
}

func TestFetchResponseRejectsKeyInjection(t *testing.T) {
	data := &imap.FetchMessageData{SeqNum: 1, Items: map[imap.FetchDataKey][]imap.FetchData{
		"UID) * BYE forged": {imap.FetchDataUID(1)},
	}}
	enc := imapwire.NewEncoder(io.Discard, &imapwire.EncoderOptions{ServerResponse: true})
	if err := WriteFetchResponse(enc, data, nil); err == nil {
		t.Fatal("structural FETCH key injection was accepted")
	}
}

func TestStatusRoundTrip(t *testing.T) {
	want := &imap.StatusData{Mailbox: "Archive", Values: map[imap.StatusItemKeyword]any{
		imap.StatusItemMessages:      uint64(7),
		imap.StatusItemUIDNext:       uint64(12),
		imap.StatusItemUIDValidity:   uint64(99),
		imap.StatusItemHighestModSeq: uint64(123),
		imap.StatusItemMailboxID:     "mailbox_1",
		imap.StatusItemAppendLimit:   "NIL",
	}}
	var buf bytes.Buffer
	enc := imapwire.NewEncoder(&buf, &imapwire.EncoderOptions{ServerResponse: true})
	if err := WriteStatusResponse(enc, want); err != nil {
		t.Fatal(err)
	}
	if err := enc.Flush(); err != nil {
		t.Fatal(err)
	}
	dec := imapwire.NewDecoder(bytes.NewReader(buf.Bytes()), nil)
	kind, _, err := dec.BeginResponse()
	var name string
	if err != nil || kind != imapwire.ResponseUntagged || !dec.ExpectSP() || !dec.ExpectAtom(&name) || name != "STATUS" {
		t.Fatalf("framing: %v (%v)", err, dec.Err())
	}
	got, err := ReadStatusResponse(dec)
	if err != nil {
		t.Fatalf("decode %q: %v", buf.Bytes(), err)
	}
	if !reflect.DeepEqual(got.Values, want.Values) || got.NumMessages != 7 || got.UIDNext != 12 || got.UIDValidity != 99 || got.HighestModSeq != 123 {
		t.Fatalf("round trip: %#v", got)
	}
}

func assertReaderValue(t *testing.T, r io.Reader, want string) {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != want {
		t.Fatalf("reader = %q, want %q", b, want)
	}
}
