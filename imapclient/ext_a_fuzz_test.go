package imapclient

import (
	"testing"

	"github.com/kiliant/go-imap/internal/imapwire"
)

// FuzzESearchResponse drives the ESEARCH response parser with arbitrary input.
// A hostile or merely broken server must produce an error, never a panic, and
// never an unbounded allocation.
func FuzzESearchResponse(f *testing.F) {
	for _, seed := range []string{
		` (TAG "A0001") UID MIN 7 MAX 3800 COUNT 3 ALL 2,10:11` + "\r\n",
		` (TAG "A0001") COUNT 0` + "\r\n",
		` COUNT 0` + "\r\n",
		` UID ALL 1:*` + "\r\n",
		` (TAG "A0001") MODSEQ 18446744073709551615` + "\r\n",
		` (TAG "A0001") X-VENDOR ("a" ("b" "c"))` + "\r\n",
		` (NOTTAG "A0001") COUNT 1` + "\r\n",
		` (TAG {5}` + "\r\nabcde) COUNT 1\r\n",
		"\r\n",
		"",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, line string) {
		cmd := &ESearchCommand{data: &ESearchData{Values: make(map[ESearchReturnKey]string)}}
		collector := esearchCollector(cmd)
		resp := &untaggedResponse{name: "ESEARCH", dec: imapwire.NewDecoderString(line, nil)}
		_, _ = collector(resp)
	})
}

// FuzzReadIDResponse covers the ID response of RFC 2971 section 3.2.
func FuzzReadIDResponse(f *testing.F) {
	for _, seed := range []string{
		" NIL\r\n",
		` ("name" "Cyrus" "version" "1.5")` + "\r\n",
		` ("name" NIL "os" "sunos")` + "\r\n",
		" ()\r\n",
		` ("name" "go-imap"` + "\r\n",
		" (\"a\" \"b\"\r\n",
		" NIL",
		"",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, line string) {
		_, _ = readIDResponse(imapwire.NewDecoderString(line, nil))
	})
}

// FuzzUIDPlusResponseCode drives the COPYUID and APPENDUID response-code
// parsers, which read attacker-influenced text out of a status response.
func FuzzUIDPlusResponseCode(f *testing.F) {
	for _, seed := range []string{
		"38505 304,319:320 3956:3958",
		"38505 3955",
		"1 1:* 1:2",
		"0 1 2",
		"4294967296 1 2",
		"1 1:2 3",
		"    ",
		"",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, args string) {
		if data, err := parseCopyUID(args); err == nil {
			if !data.Received() {
				t.Fatalf("parseCopyUID(%q) succeeded with no UIDVALIDITY", args)
			}
			if countUIDs(data.SourceUIDs) != countUIDs(data.DestinationUIDs) {
				t.Fatalf("parseCopyUID(%q) produced unpaired sets", args)
			}
		}
		if data, err := parseAppendUID(args); err == nil {
			if !data.Received() || data.DestinationUIDs.IsEmpty() {
				t.Fatalf("parseAppendUID(%q) succeeded with no destination", args)
			}
		}
	})
}
