package message_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver/message"
)

const raw = "Subject: Example\r\nFrom: sender@example.test\r\nX-Test: one\r\nX-Test: two\r\n\r\nHello\r\nworld\r\n"

func analyze(t *testing.T, raw string) *message.Message {
	t.Helper()
	m, err := message.Analyze(context.Background(), strings.NewReader(raw), int64(len(raw)), nil)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestExactSections(t *testing.T) {
	m := analyze(t, raw)
	if m.Size() != int64(len(raw)) || m.Envelope().Subject != "Example" || m.BodyStructure().MediaType() != "text/plain" {
		t.Fatal("incorrect analysed message metadata")
	}
	for _, tc := range []struct {
		name string
		item imap.FetchItemBodySection
		want string
	}{
		{"complete", imap.FetchItemBodySection{}, raw},
		{"text", imap.FetchItemBodySection{Specifier: imap.PartSpecifierText}, "Hello\r\nworld\r\n"},
		{"single-part", imap.FetchItemBodySection{Part: []int{1}}, "Hello\r\nworld\r\n"},
		{"header", imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader}, strings.Split(raw, "\r\n\r\n")[0] + "\r\n\r\n"},
		{"fields", imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader, HeaderFields: []string{"x-test"}}, "X-Test: one\r\nX-Test: two\r\n\r\n"},
		{"fields-not", imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader, HeaderFieldsNot: []string{"Subject", "from"}}, "X-Test: one\r\nX-Test: two\r\n\r\n"},
		{"partial", imap.FetchItemBodySection{Specifier: imap.PartSpecifierText, Partial: &imap.SectionPartial{Offset: 7, Size: 100}}, "world\r\n"},
		{"past-end", imap.FetchItemBodySection{Partial: &imap.SectionPartial{Offset: 1000, Size: 10}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, size, err := m.OpenBodySection(context.Background(), &tc.item)
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(r)
			if err != nil || string(got) != tc.want || size != int64(len(tc.want)) {
				t.Fatalf("section = %q, size %d, error %v; want %q", got, size, err, tc.want)
			}
		})
	}
	for _, item := range []*imap.FetchItemBodySection{nil, {Part: []int{2}}, {Part: []int{0}}, {HeaderFields: []string{"Subject"}}} {
		if _, _, err := m.OpenBodySection(context.Background(), item); err == nil {
			t.Fatalf("invalid section accepted: %#v", item)
		}
	}
}

func TestMalformedMIME(t *testing.T) {
	for _, raw := range []string{"", "Subject: no separator", "Content-Type: message/rfc822", "Content-Type: invalid\r\nBroken header\r\n\r\nbody", "Content-Type: multipart/mixed; boundary=x\r\n\r\n--x\r\n\r\nunfinished"} {
		m := analyze(t, raw)
		r, size, err := m.OpenBodySection(context.Background(), &imap.FetchItemBodySection{})
		if err != nil || size != int64(len(raw)) || m.BodyStructure() == nil || m.Envelope() == nil {
			t.Fatalf("best-effort analysis of %q: %v", raw, err)
		}
		got, err := io.ReadAll(r)
		if err != nil || string(got) != raw {
			t.Fatalf("malformed source bytes changed: %q, %v", got, err)
		}
	}
}

func TestInvalidAndZeroInputs(t *testing.T) {
	ctx := context.Background()
	//lint:ignore SA1012 Deliberately verify the documented nil-context refusal.
	if _, err := message.Analyze(nil, strings.NewReader(raw), int64(len(raw)), nil); err == nil {
		t.Fatal("nil context accepted")
	}
	if _, err := message.Analyze(ctx, nil, 0, nil); err == nil {
		t.Fatal("nil source accepted")
	}
	if _, err := message.Analyze(ctx, strings.NewReader(raw), -1, nil); err == nil {
		t.Fatal("negative size accepted")
	}
	for _, m := range []*message.Message{nil, new(message.Message)} {
		if m.Size() != 0 || m.Envelope() != nil || m.BodyStructure() != nil {
			t.Fatal("nil/zero accessors did not return zero values")
		}
		if _, _, err := m.OpenBodySection(ctx, &imap.FetchItemBodySection{}); err == nil {
			t.Fatal("unanalysed section accepted")
		}
		if _, err := message.Match(ctx, m, message.Metadata{}, imap.SearchAll, nil); err == nil {
			t.Fatal("unanalysed match accepted")
		}
	}
	m := analyze(t, raw)
	//lint:ignore SA1012 Deliberately verify the documented nil-context refusal.
	if _, _, err := m.OpenBodySection(nil, &imap.FetchItemBodySection{}); err == nil {
		t.Fatal("nil section context accepted")
	}
	//lint:ignore SA1012 Deliberately verify the documented nil-context refusal.
	if _, err := message.Match(nil, m, message.Metadata{}, imap.SearchAll, nil); err == nil {
		t.Fatal("nil match context accepted")
	}
}

func TestMetadataSearchAndCharsets(t *testing.T) {
	m := analyze(t, "Subject: Café\r\n\r\nhello\r\n")
	date := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	metadata := message.Metadata{SeqNum: 2, UID: 7, Flags: []imap.Flag{imap.FlagSeen}, InternalDate: date,
		SaveDate: &date, RFC822Size: 99, ModSeq: 10, PrivateModSeqs: map[string]uint64{"/flags/\\seen": 9}, EmailID: "email", ThreadID: "thread"}
	for _, criterion := range []imap.SearchCriteria{
		imap.SearchAnd{imap.SearchSeen, imap.SearchUID{Set: imap.UIDSetNum(7)}, imap.SearchSeqNum{Set: imap.SeqSetNum(2)}},
		imap.SearchSize{Key: imap.SearchSizeKeyLarger, Size: 98},
		imap.SearchDate{Key: imap.SearchDateKeySavedOn, Date: date},
		imap.SearchModSeq{ModSeq: 9, EntryName: "/flags/\\seen", EntryType: imap.SearchModSeqMetadataPrivate},
		imap.SearchObjectID{Key: imap.SearchObjectIDKeyEmail, Value: "email"},
		imap.SearchSavedResult{},
	} {
		matched, err := message.Match(context.Background(), m, metadata, criterion, &message.MatchOptions{SavedUIDs: imap.UIDSetNum(7)})
		if err != nil || !matched {
			t.Fatalf("match %#v = %t, %v", criterion, matched, err)
		}
	}
	// Zero is supplied metadata, not a request to infer Message.Size().
	matched, err := message.Match(context.Background(), m, message.Metadata{}, imap.SearchSize{Key: imap.SearchSizeKeySmaller, Size: 1}, nil)
	if err != nil || !matched {
		t.Fatalf("explicit zero RFC822Size = %t, %v", matched, err)
	}
	for _, charset := range []string{"UTF-8", "iso-8859-1"} {
		needle := "Café"
		if charset == "iso-8859-1" {
			needle = "Caf\xe9"
		}
		matched, err := message.Match(context.Background(), m, metadata, imap.SearchString{Key: imap.SearchKeySubject, Value: needle}, &message.MatchOptions{Charset: charset})
		if err != nil || !matched {
			t.Fatalf("charset %q = %t, %v", charset, matched, err)
		}
	}
	if _, err := message.Match(context.Background(), m, metadata, imap.SearchAll, &message.MatchOptions{Charset: "future-charset"}); !errors.Is(err, message.ErrUnsupportedCharset) {
		t.Fatalf("unsupported charset = %v", err)
	}
	for _, criterion := range []imap.SearchCriteria{
		imap.SearchKeyword("FUTURE"),
		imap.SearchFilter("unsubstituted"),
		imap.SearchOr{Left: imap.SearchAll, Right: imap.SearchKeyword("FUTURE")},
		imap.SearchAnd{imap.SearchUnseen, imap.SearchDate{Key: "FUTURE"}},
		imap.SearchNot{Criteria: imap.SearchFuzzy{Criteria: imap.SearchString{Key: "FUTURE"}}},
		nil,
	} {
		if _, err := message.Match(context.Background(), m, metadata, criterion, nil); !errors.Is(err, message.ErrUnsupportedCriteria) {
			t.Fatalf("unsupported %#v = %v", criterion, err)
		}
	}
}

type syntheticSource struct {
	header string
	size   int64
	read   int64
	hook   func(int64) error
}

func (r *syntheticSource) ReadAt(p []byte, offset int64) (int, error) {
	if r.hook != nil {
		if err := r.hook(offset); err != nil {
			return 0, err
		}
	}
	if offset >= r.size {
		return 0, io.EOF
	}
	n := int(min(int64(len(p)), r.size-offset))
	for i := range p[:n] {
		p[i] = 'x'
		if at := offset + int64(i); at < int64(len(r.header)) {
			p[i] = r.header[at]
		}
	}
	r.read += int64(n)
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func TestLargeSourceRemainsStreaming(t *testing.T) {
	source := &syntheticSource{header: "Content-Type: application/octet-stream\r\n\r\n", size: 200 << 20}
	m, err := message.Analyze(context.Background(), source, source.size, nil)
	if err != nil {
		t.Fatal(err)
	}
	r, size, err := m.OpenBodySection(context.Background(), &imap.FetchItemBodySection{})
	if err != nil || size != source.size {
		t.Fatalf("large section size = %d, %v", size, err)
	}
	if _, err := io.CopyN(io.Discard, r, 1024); err != nil {
		t.Fatal(err)
	}
	if source.read > 64<<10 {
		t.Fatalf("analysis/open read %d bytes from a 200 MiB opaque message", source.read)
	}
}

func TestCancellationAndSourceErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	m, err := message.Analyze(ctx, strings.NewReader(raw), int64(len(raw)), nil)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := message.Analyze(ctx, strings.NewReader(raw), int64(len(raw)), nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled analysis = %v", err)
	}
	if _, err := message.Match(ctx, m, message.Metadata{}, imap.SearchAll, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled match = %v", err)
	}
	r, _, err := m.OpenBodySection(context.Background(), &imap.FetchItemBodySection{})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := io.ReadAll(r); err != nil || string(got) != raw {
		t.Fatalf("analysis context affected future reads: %q, %v", got, err)
	}
	sectionCtx, cancelSection := context.WithCancel(context.Background())
	r, _, err = m.OpenBodySection(sectionCtx, &imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader, HeaderFields: []string{"Missing"}})
	if err != nil {
		t.Fatal(err)
	}
	cancelSection()
	if _, err := io.ReadAll(r); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled section = %v", err)
	}
	readFailure := errors.New("source failure")
	source := &syntheticSource{header: "Content-Type: text/plain\r\n\r\n", size: 200 << 10}
	source.hook = func(offset int64) error {
		if offset >= 32<<10 && offset < source.size-1 {
			return readFailure
		}
		return nil
	}
	if _, err := message.Analyze(context.Background(), source, source.size, nil); !errors.Is(err, readFailure) {
		t.Fatalf("body analysis lost I/O error: %v", err)
	}
	if _, err := message.Analyze(context.Background(), strings.NewReader(raw), int64(len(raw)+1), nil); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("short source = %v", err)
	}
	opaque := "Content-Type: application/octet-stream\r\n\r\n" + strings.Repeat("x", 64<<10)
	if _, err := message.Analyze(context.Background(), strings.NewReader(opaque), int64(len(opaque)+1), nil); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("short opaque source = %v", err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	source.hook = func(offset int64) error {
		if offset >= 32<<10 && offset < source.size-1 {
			cancel()
		}
		return nil
	}
	if _, err := message.Analyze(ctx, source, source.size, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("mid-analysis cancellation = %v", err)
	}
	source.hook = nil
	m, err = message.Analyze(context.Background(), source, source.size, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancelMatch := context.WithCancel(context.Background())
	defer cancelMatch()
	source.hook = func(int64) error { cancelMatch(); return nil }
	if _, err := message.Match(ctx, m, message.Metadata{}, imap.SearchString{Key: imap.SearchKeyBody, Value: "absent"}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("mid-search cancellation = %v", err)
	}
}

func TestCopiesAndConcurrentOperations(t *testing.T) {
	m := analyze(t, "From: sender@example.test\r\nTo: to@example.test\r\nCc: cc@example.test\r\nBcc: bcc@example.test\r\nIn-Reply-To: <parent@example.test>\r\nContent-Type: multipart/mixed; boundary=x\r\nContent-Language: en\r\nContent-Disposition: inline; filename=outer\r\n\r\n--x\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Disposition: inline; filename=text\r\nContent-Language: fr\r\n\r\nhello\r\n--x\r\nContent-Type: message/rfc822\r\n\r\nFrom: nested@example.test\r\nSubject: Nested\r\n\r\nbody\r\n--x--\r\n")
	for _, get := range []func() any{func() any { return m.Envelope() }, func() any { return m.BodyStructure() }} {
		baseline, err := json.Marshal(get())
		if err != nil {
			t.Fatal(err)
		}
		mutate(reflect.ValueOf(get()))
		after, err := json.Marshal(get())
		if err != nil || !bytes.Equal(baseline, after) {
			t.Fatalf("accessor result mutated shared analysis: %s -> %s (%v)", baseline, after, err)
		}
	}
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mutate(reflect.ValueOf(m.Envelope()))
			mutate(reflect.ValueOf(m.BodyStructure()))
			r, _, err := m.OpenBodySection(context.Background(), &imap.FetchItemBodySection{Part: []int{1}})
			if err != nil {
				t.Error(err)
				return
			}
			if _, err := io.Copy(io.Discard, r); err != nil {
				t.Error(err)
			}
			if matched, err := message.Match(context.Background(), m, message.Metadata{}, imap.SearchString{Key: imap.SearchKeyBody, Value: "hello"}, nil); err != nil || !matched {
				t.Errorf("concurrent match = %t, %v", matched, err)
			}
		}()
	}
	wg.Wait()
}

func mutate(v reflect.Value) {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			mutate(v.Elem())
		}
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Field(i).CanSet() {
				mutate(v.Field(i))
			}
		}
	case reflect.Slice:
		for i := range v.Len() {
			mutate(v.Index(i))
		}
	case reflect.Map:
		for _, key := range v.MapKeys() {
			v.SetMapIndex(key, reflect.ValueOf("changed"))
		}
	case reflect.String:
		if v.CanSet() {
			v.SetString("changed")
		}
	case reflect.Int, reflect.Int64:
		if v.CanSet() {
			v.SetInt(99)
		}
	}
}

func TestBodyStructureCloneShapeCoverage(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "../../bodystructure.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, declaration := range f.Decls {
		fn, ok := declaration.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "bodyStructure" || fn.Recv == nil {
			continue
		}
		receiver := fn.Recv.List[0].Type.(*ast.StarExpr).X.(*ast.Ident).Name
		if receiver != "BodyStructureSinglePart" && receiver != "BodyStructureMultiPart" {
			t.Errorf("new bodystructure shape %s needs a deep copy and mutation coverage", receiver)
		}
	}
}
