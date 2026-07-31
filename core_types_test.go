package imap

import (
	"bytes"
	"errors"
	"reflect"
	"testing"
)

func TestOpenSetExtensionItems(t *testing.T) {
	var fetchItems = []FetchItem{
		FetchItemModSeq,
		FetchItemEmailID,
		FetchItemThreadID,
		FetchItemSaveDate,
		&FetchItemBinarySection{Part: []int{1}},
		&FetchItemBinarySectionSize{Part: []int{1}},
		&FetchItemPreview{Lazy: true},
		FetchItemKeyword("FUTURE-ITEM"),
	}
	var criteria = []SearchCriteria{
		SearchAnd{SearchSeen, SearchNot{Criteria: SearchDeleted}},
		SearchOr{Left: SearchAll, Right: SearchRecent},
		SearchWithin{Key: SearchWithinKeyYounger, Seconds: 60},
		SearchModSeq{ModSeq: 42},
		SearchObjectID{Key: SearchObjectIDKeyEmail, Value: "id"},
		SearchDate{Key: SearchDateKeySavedSince},
		SearchFuzzy{Criteria: SearchString{Key: SearchKeySubject, Value: "subject"}},
		SearchKeyword("FUTURE-KEY"),
	}
	var statusItems = []StatusItem{
		StatusItemHighestModSeq,
		StatusItemSize,
		StatusItemMailboxID,
		StatusItemDeleted,
		StatusItemDeletedStorage,
		StatusItemKeyword("FUTURE-ITEM"),
	}
	if len(fetchItems) != 8 || len(criteria) != 8 || len(statusItems) != 6 {
		t.Fatal("open-set compile-time coverage was unexpectedly changed")
	}
}

func TestFetchMessageDataIsKeyedAndLossless(t *testing.T) {
	unknown := &FetchDataRaw{Reader: bytes.NewBufferString("(FUTURE VALUE)")}
	msg := FetchMessageData{
		SeqNum: 9,
		Items: map[FetchDataKey][]FetchData{
			"UID":         {FetchDataUID(42)},
			"FLAGS":       {FetchDataFlags{FlagSeen}},
			"FUTURE-ITEM": {unknown, &FetchDataRaw{Reader: bytes.NewBufferString("second")}},
		},
	}
	if got := len(msg.Items["FUTURE-ITEM"]); got != 2 {
		t.Fatalf("duplicate unknown values = %d, want 2", got)
	}
	var values = []FetchData{
		FetchDataUID(1),
		FetchDataFlags{FlagSeen},
		&FetchDataInternalDate{},
		FetchDataRFC822Size(123),
		&FetchDataEnvelope{},
		&FetchDataBodyStructure{},
		&FetchDataLiteral{Literal: nil},
		&FetchDataBodySection{},
		&FetchDataBinarySection{},
		FetchDataBinarySectionSize(456),
		FetchDataModSeq(7),
		FetchDataObjectID("id"),
		&FetchDataSaveDate{},
		&FetchDataPreview{},
		unknown,
	}
	if len(values) != 15 {
		t.Fatal("typed FETCH data compile-time coverage was unexpectedly changed")
	}
}

func TestError(t *testing.T) {
	cause := errors.New("cause")
	err := &Error{
		Type:     ErrorTypeNo,
		Code:     CodeAuthenticationFailed,
		CodeArgs: "MECHANISM PLAIN",
		Text:     "credentials rejected",
		Tag:      "A42",
		Err:      cause,
	}
	if got, want := err.Error(), "imap: NO (tag A42) [AUTHENTICATIONFAILED MECHANISM PLAIN]: credentials rejected"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
	if !errors.Is(err, cause) {
		t.Fatal("Error does not unwrap its cause")
	}
	if !errors.Is(err, &Error{Code: CodeAuthenticationFailed}) {
		t.Fatal("Error does not match its response code")
	}
	if errors.Is(err, &Error{Type: ErrorTypeBad}) {
		t.Fatal("Error unexpectedly matches another type")
	}
	var got *Error
	if !errors.As(err, &got) || got != err {
		t.Fatal("errors.As did not recover *imap.Error")
	}

	unknown := ResponseCode("FUTURE-CODE")
	if unknown != "FUTURE-CODE" {
		t.Fatal("ResponseCode does not preserve an unknown code")
	}
}

func TestFlagAndMailboxAttrAreOpen(t *testing.T) {
	if !FlagSeen.Equal(Flag("\\seen")) || !ContainsFlag([]Flag{Flag("custom")}, Flag("CUSTOM")) {
		t.Fatal("flag comparison is not case-insensitive")
	}
	if !FlagSeen.IsSystem() || Flag("custom").IsSystem() {
		t.Fatal("Flag.IsSystem returned the wrong classification")
	}
	if !MailboxAttrHasChildren.Equal(MailboxAttr("\\haschildren")) || !ContainsAttr([]MailboxAttr{MailboxAttrArchive}, MailboxAttr("\\archive")) {
		t.Fatal("mailbox attribute comparison is not case-insensitive")
	}
	if Flag("future-keyword") != "future-keyword" || MailboxAttr("\\Future") != "\\Future" {
		t.Fatal("open string-backed values were not preserved")
	}
}

func TestAddress(t *testing.T) {
	if got := (&Address{Name: "A, B", Mailbox: "user", Host: "example.org"}).String(); got != `"A, B" <user@example.org>` {
		t.Fatalf("Address.String() = %q", got)
	}
	start := &Address{Mailbox: "group"}
	end := &Address{}
	if !start.IsGroupStart() || start.IsGroupEnd() || !end.IsGroupEnd() || end.IsGroupStart() {
		t.Fatal("group marker detection failed")
	}
}

func TestBodyStructureHelpers(t *testing.T) {
	plain := &BodyStructureSinglePart{
		Type:    "TEXT",
		Subtype: "PLAIN",
		Params:  map[string]string{"NAME": "fallback.txt"},
		Extended: &BodyStructureSinglePartExt{Disp: &BodyStructureDisposition{
			Value:  "attachment",
			Params: map[string]string{"filename": "report.txt"},
		}},
	}
	html := &BodyStructureSinglePart{Type: "text", Subtype: "html"}
	root := &BodyStructureMultiPart{Subtype: "ALTERNATIVE", Children: []BodyStructure{plain, html}}
	if got := root.MediaType(); got != "multipart/alternative" {
		t.Fatalf("MediaType() = %q", got)
	}
	if got := plain.Filename(); got != "report.txt" {
		t.Fatalf("Filename() = %q", got)
	}
	var paths [][]int
	root.Walk(func(path []int, _ BodyStructure) bool {
		paths = append(paths, append([]int(nil), path...))
		return true
	})
	if want := [][]int{nil, {1}, {2}}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("Walk paths = %v, want %v", paths, want)
	}
}
