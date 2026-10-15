package imapclient

import (
	"reflect"
	"testing"

	"github.com/kiliant/go-imap/internal/imapwire"
)

func TestResponseDataIsDecodedOnce(t *testing.T) {
	for _, test := range []struct{ name, wire string }{
		{"STATUS", " INBOX (MESSAGES 3)\r\n"},
		{"LIST", " () \"/\" INBOX\r\n"},
		{"LSUB", " () \"/\" INBOX\r\n"},
		{"XLIST", " () \"/\" INBOX\r\n"},
		{"ESEARCH", " (TAG \"a\") COUNT 3\r\n"},
		{"METADATA", " INBOX /shared/comment\r\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			resp := &untaggedResponse{name: test.name, dec: imapwire.NewDecoderString(test.wire+"NEXT\r\n", nil)}
			if err := resp.decodeData(); err != nil {
				t.Fatal(err)
			}
			first := resp.data
			if err := resp.decodeData(); err != nil || resp.data != first {
				t.Fatalf("second decode changed cached response: %v", err)
			}
			var next string
			if !resp.dec.ExpectAtom(&next) || next != "NEXT" || !resp.dec.ExpectCRLF() {
				t.Fatalf("decode consumed next response: %q, %v", next, resp.dec.Err())
			}
		})
	}
}

func TestResponseDataCachesErrors(t *testing.T) {
	resp := &untaggedResponse{name: "STATUS", dec: imapwire.NewDecoderString(" INBOX (MESSAGES invalid)\r\n", nil)}
	err := resp.decodeData()
	if err == nil || !resp.data.decoded || resp.decodeData() != err {
		t.Fatalf("parse error not cached: %v", err)
	}
}

func TestResponseDataLeavesOtherResponsesStreaming(t *testing.T) {
	for _, resp := range []*untaggedResponse{
		{name: "FETCH", hasNum: true}, {name: "X-UNKNOWN"},
	} {
		resp.dec = imapwire.NewDecoderString(" untouched\r\n", nil)
		if err := resp.decodeData(); err != nil || resp.data.decoded || !resp.dec.SP() {
			t.Fatalf("streaming response was decoded: %#v, %v", resp, err)
		}
	}
}

func TestESearchPreservesRepeatedReturnItems(t *testing.T) {
	parsed, err := readESearchResponse(imapwire.NewDecoderString(" (TAG \"old\") UID ADDTO (0 9,3:4) REMOVEFROM (2 8) ADDTO (1 7) X-VENDOR (\"a\" NIL)\r\n", nil))
	if err != nil {
		t.Fatal(err)
	}
	want := []esearchReturnItem{{"ADDTO", "(0 9,3:4)"}, {"REMOVEFROM", "(2 8)"}, {"ADDTO", "(1 7)"}, {"X-VENDOR", `("a" NIL)`}}
	if parsed.result.Tag != "old" || !parsed.result.Data.UID || !reflect.DeepEqual(parsed.items, want) || parsed.result.Data.Values["ADDTO"] != "(1 7)" {
		t.Fatalf("ordered ESEARCH = %#v, %#v", parsed, parsed.items)
	}
}

func TestMetadataResponseForms(t *testing.T) {
	for _, mailbox := range []string{`""`, "INBOX"} {
		parsed, err := readMetadataData(imapwire.NewDecoderString(" "+mailbox+" /shared/comment /private/comment\r\n", nil))
		if err != nil || !parsed.notification || len(parsed.changes) != 2 || len(parsed.values.Entries) != 0 {
			t.Fatalf("names-only metadata = %#v, %v", parsed, err)
		}
		parsed, err = readMetadataData(imapwire.NewDecoderString(" "+mailbox+" (/shared/comment NIL /private/comment \"\")\r\n", nil))
		if err != nil || parsed.notification || len(parsed.changes) != 0 || len(parsed.values.Entries) != 2 {
			t.Fatalf("value-bearing metadata = %#v, %v", parsed, err)
		}
		entries := parsed.values.Entries
		if entries[0].Value != nil || entries[1].Value == nil || *entries[1].Value != "" {
			t.Fatalf("NIL and empty values conflated: %#v", entries)
		}
	}
}

func TestResponseDataRespectsLiteralLimits(t *testing.T) {
	for _, test := range []struct{ name, wire string }{
		{"STATUS", " {5}\r\nINBOX (MESSAGES 3)\r\n"},
		{"LIST", " () \"/\" {5}\r\nINBOX\r\n"},
		{"ESEARCH", " X-VENDOR {5}\r\nvalue\r\n"},
		{"METADATA", " INBOX (/shared/comment {5}\r\nvalue)\r\n"},
		{"METADATA", " INBOX {5}\r\nentry\r\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			resp := &untaggedResponse{name: test.name, dec: imapwire.NewDecoderString(test.wire, &imapwire.Options{MaxBufferedLiteralSize: 4})}
			if err := resp.decodeData(); err == nil {
				t.Fatal("oversized buffered literal accepted")
			}
		})
	}
}
