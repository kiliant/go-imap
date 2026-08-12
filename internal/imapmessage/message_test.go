package imapmessage

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapcodec"
	"github.com/kiliant/go-imap/internal/imapwire"
)

const fixture = "Date: Wed, 12 Aug 2026 09:30:00 +0200\r\n" +
	"Subject: =?UTF-8?Q?Caf=C3=A9_status?=\r\n" +
	"From: Team: Ann <ann@example.test>, bob@example.test;\r\n" +
	"To: reader@example.test\r\n" +
	"Cc: copy@example.test\r\n" +
	"Bcc: blind@example.test\r\n" +
	"X-Project: go-imap\r\n" +
	"Message-ID: <root@example.test>\r\n" +
	"Content-Type: multipart/mixed; boundary=\"b\"\r\n" +
	"\r\n" +
	"preamble\r\n" +
	"--b\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"Content-Transfer-Encoding: quoted-printable\r\n" +
	"X-Part: one\r\n" +
	"\r\n" +
	"Hello Caf=C3=A9\r\nSecond line\r\n" +
	"--b\r\n" +
	"Content-Type: application/octet-stream\r\n" +
	"Content-Disposition: attachment; filename=\"x.bin\"\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"\r\n" +
	"AAEC\r\n" +
	"--b\r\n" +
	"Content-Type: message/rfc822\r\n" +
	"\r\n" +
	"Date: Tue, 11 Aug 2026 08:00:00 +0200\r\n" +
	"Subject: Nested\r\n" +
	"From: nested@example.test\r\n" +
	"\r\n" +
	"Nested body\r\n" +
	"--b--\r\n" +
	"epilogue\r\n"

func analyzeFixture(t *testing.T) *Message {
	t.Helper()
	m, err := Analyze(bytes.NewReader([]byte(fixture)), int64(len(fixture)))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestAnalyzeEnvelopeAndBodyStructure(t *testing.T) {
	m := analyzeFixture(t)
	if m.Envelope.RawDate != "Wed, 12 Aug 2026 09:30:00 +0200" || m.Envelope.Subject != "Café status" {
		t.Fatalf("envelope = %#v", m.Envelope)
	}
	if len(m.Envelope.From) != 4 || !m.Envelope.From[0].IsGroupStart() || m.Envelope.From[0].Mailbox != "Team" || !m.Envelope.From[3].IsGroupEnd() {
		t.Fatalf("group addresses = %#v", m.Envelope.From)
	}
	mp, ok := m.BodyStructure.(*imap.BodyStructureMultiPart)
	if !ok || len(mp.Children) != 3 || mp.Subtype != "mixed" || mp.Extended.Params["boundary"] != "b" {
		t.Fatalf("body structure = %#v", m.BodyStructure)
	}
	text := mp.Children[0].(*imap.BodyStructureSinglePart)
	wantText := "Hello Caf=C3=A9\r\nSecond line"
	if text.Size != uint32(len(wantText)) || text.Text == nil || text.Text.NumLines != 1 {
		t.Fatalf("text structure = %#v, want size %d and 1 line", text, len(wantText))
	}
	attachment := mp.Children[1].(*imap.BodyStructureSinglePart)
	if attachment.Size != 4 || attachment.Filename() != "x.bin" || !strings.EqualFold(attachment.Encoding, "base64") {
		t.Fatalf("attachment structure = %#v", attachment)
	}
	nested := mp.Children[2].(*imap.BodyStructureSinglePart)
	if nested.Message == nil || nested.Message.Envelope.Subject != "Nested" || nested.Message.BodyStructure.MediaType() != "text/plain" {
		t.Fatalf("nested structure = %#v", nested)
	}
}

func TestGeneratedStructuresRoundTripThroughCodec(t *testing.T) {
	m := analyzeFixture(t)

	var envelopeWire bytes.Buffer
	enc := imapwire.NewEncoder(&envelopeWire, &imapwire.EncoderOptions{ServerResponse: true})
	imapcodec.WriteEnvelope(enc, m.Envelope)
	if err := enc.Flush(); err != nil {
		t.Fatal(err)
	}
	gotEnvelope, err := imapcodec.ReadEnvelope(imapwire.NewDecoder(bytes.NewReader(envelopeWire.Bytes()), nil))
	if err != nil {
		t.Fatalf("ENVELOPE %q: %v", envelopeWire.Bytes(), err)
	}
	assertEnvelopeEqual(t, gotEnvelope, m.Envelope)

	var structureWire bytes.Buffer
	enc = imapwire.NewEncoder(&structureWire, &imapwire.EncoderOptions{ServerResponse: true})
	imapcodec.WriteBodyStructure(enc, m.BodyStructure)
	if err := enc.Flush(); err != nil {
		t.Fatal(err)
	}
	gotStructure, err := imapcodec.ReadBodyStructure(imapwire.NewDecoder(bytes.NewReader(structureWire.Bytes()), nil))
	if err != nil {
		t.Fatalf("BODYSTRUCTURE %q: %v", structureWire.Bytes(), err)
	}
	normalizeStructureDates(t, gotStructure, m.BodyStructure)
	if !reflect.DeepEqual(gotStructure, m.BodyStructure) {
		t.Fatalf("BODYSTRUCTURE round trip:\n got %#v\nwant %#v", gotStructure, m.BodyStructure)
	}
}

func TestMalformedHeadersStillProduceEncodableStructure(t *testing.T) {
	for _, raw := range [][]byte{
		[]byte("Content-Transfer-Encoding:\x00"),
		[]byte("Content-Type: message/rfc822"),
		[]byte("Subject: =?UTF-8?B?AA==?=\r\nContent-Description: =?UTF-8?B?AA==?=\r\n\r\nbody"),
		[]byte("Content-Type: text/plain; name*=utf-8''%00\r\n\r\nbody"),
	} {
		m, err := Analyze(bytes.NewReader(raw), int64(len(raw)))
		if err != nil {
			t.Fatal(err)
		}
		var wire bytes.Buffer
		enc := imapwire.NewEncoder(&wire, &imapwire.EncoderOptions{ServerResponse: true})
		imapcodec.WriteBodyStructure(enc, m.BodyStructure)
		if err := enc.Flush(); err != nil {
			t.Fatalf("malformed stored message %q became unfetchable: %v", raw, err)
		}
	}
}

func TestBodySectionExtraction(t *testing.T) {
	m := analyzeFixture(t)
	rootHeaderEnd := strings.Index(fixture, "\r\n\r\n") + 4
	rootHeader := fixture[:rootHeaderEnd]
	part1MIME := "Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n" +
		"X-Part: one\r\n\r\n"
	nestedHeader := "Date: Tue, 11 Aug 2026 08:00:00 +0200\r\nSubject: Nested\r\nFrom: nested@example.test\r\n\r\n"
	tests := []struct {
		name string
		item *imap.FetchItemBodySection
		want string
	}{
		{"whole", &imap.FetchItemBodySection{}, fixture},
		{"root-header", &imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader}, rootHeader},
		{"header-fields", &imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader, HeaderFields: []string{"Subject", "X-Project"}}, "Subject: =?UTF-8?Q?Caf=C3=A9_status?=\r\nX-Project: go-imap\r\n\r\n"},
		{"header-fields-not", &imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader, HeaderFieldsNot: []string{"Subject", "Content-Type"}}, strings.ReplaceAll(strings.ReplaceAll(rootHeader, "Subject: =?UTF-8?Q?Caf=C3=A9_status?=\r\n", ""), "Content-Type: multipart/mixed; boundary=\"b\"\r\n", "")},
		{"part-body", &imap.FetchItemBodySection{Part: []int{1}}, "Hello Caf=C3=A9\r\nSecond line"},
		{"part-mime", &imap.FetchItemBodySection{Part: []int{1}, Specifier: imap.PartSpecifierMIME}, part1MIME},
		{"message-header", &imap.FetchItemBodySection{Part: []int{3}, Specifier: imap.PartSpecifierHeader}, nestedHeader},
		{"message-text", &imap.FetchItemBodySection{Part: []int{3}, Specifier: imap.PartSpecifierText}, "Nested body"},
		{"nested-single", &imap.FetchItemBodySection{Part: []int{3, 1}}, "Nested body"},
		{"partial", &imap.FetchItemBodySection{Part: []int{1}, Partial: &imap.SectionPartial{Offset: 6, Size: 9}}, "Caf=C3=A9"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, size, err := m.OpenBodySection(tt.item)
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(r)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want || size != int64(len(tt.want)) {
				t.Fatalf("section = %q (%d), want %q (%d)", got, size, tt.want, len(tt.want))
			}
		})
	}
}

func TestWholeMessageSectionStreams200MiB(t *testing.T) {
	const size = int64(200 << 20)
	source := &countingReaderAt{size: size, prefix: []byte("Content-Type: application/octet-stream\r\n\r\n")}
	m, err := Analyze(source, size)
	if err != nil {
		t.Fatal(err)
	}
	if source.bytesRead > 1<<20 {
		t.Fatalf("Analyze read %d bytes of an opaque 200 MiB body", source.bytesRead)
	}
	before := source.bytesRead
	r, gotSize, err := m.OpenBodySection(&imap.FetchItemBodySection{})
	if err != nil || gotSize != size {
		t.Fatalf("OpenBodySection size = %d, err = %v", gotSize, err)
	}
	if source.bytesRead != before {
		t.Fatal("opening BODY[] eagerly read message bytes")
	}
	buf := make([]byte, len(source.prefix)+8)
	if _, err := io.ReadFull(r, buf); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf[:len(source.prefix)], source.prefix) {
		t.Fatalf("prefix = %q", buf)
	}
}

type countingReaderAt struct {
	size      int64
	prefix    []byte
	bytesRead int64
}

func (r *countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off >= r.size {
		return 0, io.EOF
	}
	n := int(min(int64(len(p)), r.size-off))
	for i := 0; i < n; i++ {
		at := off + int64(i)
		if at < int64(len(r.prefix)) {
			p[i] = r.prefix[at]
		} else {
			p[i] = 'x'
		}
	}
	r.bytesRead += int64(n)
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func TestMatchEverySearchCriterionType(t *testing.T) {
	m := analyzeFixture(t)
	saveDate := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	metadata := Metadata{
		SeqNum: 7, UID: 44,
		Flags:          []imap.Flag{imap.FlagSeen, imap.FlagRecent, "$Label"},
		InternalDate:   time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC),
		SaveDate:       &saveDate,
		RFC822Size:     int64(len(fixture)),
		ModSeq:         100,
		PrivateModSeqs: map[string]uint64{"/flags/\\draft": 80},
		SharedModSeqs:  map[string]uint64{"/flags/\\draft": 120},
		EmailID:        "email_1", ThreadID: "thread_1",
	}
	options := &MatchOptions{
		Now:       metadata.InternalDate.Add(30 * time.Minute),
		SavedUIDs: imap.UIDSetNum(44),
	}
	tests := []struct {
		name      string
		criterion imap.SearchCriteria
	}{
		{"SearchAnd", imap.SearchAnd{imap.SearchAll, imap.SearchSeen}},
		{"SearchOr", imap.SearchOr{Left: imap.SearchDeleted, Right: imap.SearchSeen}},
		{"SearchNot", imap.SearchNot{Criteria: imap.SearchDeleted}},
		{"SearchKeyword", imap.SearchSeen},
		{"SearchFlagKeyword", imap.SearchFlagKeyword{Flag: "$label"}},
		{"SearchHeaderField", imap.SearchHeaderField{Field: "X-Project", Value: "IMAP"}},
		{"SearchString", imap.SearchString{Key: imap.SearchKeyBody, Value: "café"}},
		{"SearchDate", imap.SearchDate{Key: imap.SearchDateKeySentOn, Date: time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC)}},
		{"SearchSize", imap.SearchSize{Key: imap.SearchSizeKeyLarger, Size: 100}},
		{"SearchSeqNum", imap.SearchSeqNum{Set: imap.SeqSetNum(7)}},
		{"SearchUID", imap.SearchUID{Set: imap.UIDSetNum(44)}},
		{"SearchSavedResult", imap.SearchSavedResult{}},
		{"SearchWithin", imap.SearchWithin{Key: imap.SearchWithinKeyYounger, Seconds: 3600}},
		{"SearchObjectID", imap.SearchObjectID{Key: imap.SearchObjectIDKeyEmail, Value: "email_1"}},
		{"SearchModSeq", imap.SearchModSeq{ModSeq: 100, EntryName: "/flags/\\draft", EntryType: imap.SearchModSeqMetadataAll}},
		{"SearchFuzzy", imap.SearchFuzzy{Criteria: imap.SearchString{Key: imap.SearchKeySubject, Value: "status"}}},
	}
	covered := make(map[string]bool)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Match(m, metadata, tt.criterion, options)
			if err != nil || !got {
				t.Fatalf("Match(%#v) = %v, %v", tt.criterion, got, err)
			}
		})
		covered[tt.name] = true
	}
	for _, typeName := range searchCriteriaTypes(t) {
		if !covered[typeName] {
			t.Errorf("SearchCriteria implementation %s has no Match test", typeName)
		}
	}
}

func TestSearchKeysDatesAndCharsets(t *testing.T) {
	m := analyzeFixture(t)
	internalDate := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	saveDate := internalDate.Add(-24 * time.Hour)
	metadata := Metadata{Flags: []imap.Flag{imap.FlagSeen, imap.FlagRecent}, InternalDate: internalDate, SaveDate: &saveDate, RFC822Size: int64(len(fixture))}

	for _, criterion := range []imap.SearchCriteria{
		imap.SearchAnswered, imap.SearchDeleted, imap.SearchDraft, imap.SearchFlagged,
		imap.SearchNew, imap.SearchOld, imap.SearchUnseen,
	} {
		got, err := Match(m, metadata, imap.SearchNot{Criteria: criterion}, nil)
		if err != nil || !got {
			t.Errorf("NOT %v = %v, %v", criterion, got, err)
		}
	}
	for _, criterion := range []imap.SearchCriteria{
		imap.SearchAll, imap.SearchSeen, imap.SearchRecent, imap.SearchUnanswered,
		imap.SearchUndeleted, imap.SearchUndraft, imap.SearchUnflagged,
		imap.SearchSaveDateSupported,
	} {
		got, err := Match(m, metadata, criterion, nil)
		if err != nil || !got {
			t.Errorf("%v = %v, %v", criterion, got, err)
		}
	}

	stringCases := []imap.SearchString{
		{Key: imap.SearchKeyBcc, Value: "blind"}, {Key: imap.SearchKeyCc, Value: "copy"},
		{Key: imap.SearchKeyFrom, Value: "ann"}, {Key: imap.SearchKeySubject, Value: "café"},
		{Key: imap.SearchKeyTo, Value: "reader"}, {Key: imap.SearchKeyBody, Value: "second"},
		{Key: imap.SearchKeyText, Value: "café"},
	}
	for _, criterion := range stringCases {
		got, err := Match(m, metadata, criterion, nil)
		if err != nil || !got {
			t.Errorf("%v = %v, %v", criterion.Key, got, err)
		}
	}
	latin1 := imap.SearchString{Key: imap.SearchKeySubject, Value: string([]byte{'C', 'a', 'f', 0xe9})}
	if got, err := Match(m, metadata, latin1, &MatchOptions{Charset: "iso-8859-1"}); err != nil || !got {
		t.Fatalf("Latin-1 search = %v, %v", got, err)
	}
	if _, err := Match(m, metadata, latin1, &MatchOptions{Charset: "x-unknown"}); err == nil {
		t.Fatal("unsupported charset was accepted")
	}

	dateCases := []imap.SearchDate{
		{Key: imap.SearchDateKeyBefore, Date: internalDate.AddDate(0, 0, 1)},
		{Key: imap.SearchDateKeyOn, Date: internalDate},
		{Key: imap.SearchDateKeySince, Date: internalDate.AddDate(0, 0, -1)},
		{Key: imap.SearchDateKeySentBefore, Date: internalDate.AddDate(0, 0, 1)},
		{Key: imap.SearchDateKeySentOn, Date: internalDate},
		{Key: imap.SearchDateKeySentSince, Date: internalDate.AddDate(0, 0, -1)},
		{Key: imap.SearchDateKeySavedBefore, Date: saveDate.AddDate(0, 0, 1)},
		{Key: imap.SearchDateKeySavedOn, Date: saveDate},
		{Key: imap.SearchDateKeySavedSince, Date: saveDate.AddDate(0, 0, -1)},
	}
	for _, criterion := range dateCases {
		got, err := Match(m, metadata, criterion, nil)
		if err != nil || !got {
			t.Errorf("%v = %v, %v", criterion.Key, got, err)
		}
	}
}

func searchCriteriaTypes(t *testing.T) []string {
	t.Helper()
	_, current, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(current), "..", "..", "search.go")
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "searchCriteria" || fn.Recv == nil || len(fn.Recv.List) != 1 {
			continue
		}
		typ := fn.Recv.List[0].Type
		if star, ok := typ.(*ast.StarExpr); ok {
			typ = star.X
		}
		if ident, ok := typ.(*ast.Ident); ok {
			names = append(names, ident.Name)
		}
	}
	sort.Strings(names)
	return names
}

func assertEnvelopeEqual(t *testing.T, got, want *imap.Envelope) {
	t.Helper()
	if got == nil || want == nil {
		if got != want {
			t.Fatalf("envelopes = %#v, %#v", got, want)
		}
		return
	}
	if !got.Date.Equal(want.Date) {
		t.Fatalf("envelope dates = %v, want %v", got.Date, want.Date)
	}
	got.Date = want.Date
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("envelope round trip:\n got %#v\nwant %#v", got, want)
	}
}

func normalizeStructureDates(t *testing.T, got, want imap.BodyStructure) {
	t.Helper()
	switch got := got.(type) {
	case *imap.BodyStructureSinglePart:
		want, ok := want.(*imap.BodyStructureSinglePart)
		if !ok {
			t.Fatalf("body structure types differ: %T, %T", got, want)
		}
		if got.Message != nil && want.Message != nil {
			if !got.Message.Envelope.Date.Equal(want.Message.Envelope.Date) {
				t.Fatalf("nested envelope dates differ")
			}
			got.Message.Envelope.Date = want.Message.Envelope.Date
			normalizeStructureDates(t, got.Message.BodyStructure, want.Message.BodyStructure)
		}
	case *imap.BodyStructureMultiPart:
		want, ok := want.(*imap.BodyStructureMultiPart)
		if !ok || len(got.Children) != len(want.Children) {
			t.Fatalf("body structure shapes differ: %T, %T", got, want)
		}
		for i := range got.Children {
			normalizeStructureDates(t, got.Children[i], want.Children[i])
		}
	default:
		t.Fatalf("unknown body structure %T", got)
	}
}

func ExampleMessage_OpenBodySection() {
	m, _ := Analyze(bytes.NewReader([]byte("Subject: hi\r\n\r\nbody")), 19)
	r, size, _ := m.OpenBodySection(&imap.FetchItemBodySection{Specifier: imap.PartSpecifierText})
	b, _ := io.ReadAll(r)
	fmt.Printf("%d %s\n", size, b)
	// Output: 4 body
}
