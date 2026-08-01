package imapclient

import (
	"testing"

	"github.com/kiliant/go-imap/internal/imapwire"
)

// Parsers in the core client and in group A that a hostile server can reach but
// that had no target of their own. The wire codec has its own targets in
// internal/imapwire; these cover the layer above it, where response text is
// turned into the exported types.

// FuzzDecodeSASL covers the base64 decoding of a server SASL challenge. It runs
// before authentication completes, so it is reachable by anyone who can answer
// on the socket.
func FuzzDecodeSASL(f *testing.F) {
	for _, s := range []string{
		"", "=", "+", "aGVsbG8=", "aGVsbG8", "!!!!", "====",
		"YWJj ZGVm", "\x00", "\xff",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		_, _ = decodeSASL(s)
	})
}

// FuzzDecodeNamespaceGroup covers one parenthesised group of the NAMESPACE
// response, RFC 2342 section 5.
func FuzzDecodeNamespaceGroup(f *testing.F) {
	for _, s := range []string{
		"NIL", `(("" "/"))`, `(("" "/")("#news." "."))`,
		`(("" NIL))`, `((""))`, `(()`, `(`, "", `(("" "/" "X" "Y"))`,
		`(("" "/" "X"))`, `(("prefix" "//"))`,
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		dec := imapwire.NewDecoderString(s, nil)
		_, _ = decodeNamespaceGroup(dec)
	})
}

// FuzzReadFetchObjectID covers the OBJECTID fetch-item value parser, RFC 8474.
func FuzzReadFetchObjectID(f *testing.F) {
	for _, s := range []string{
		"", " ", "M00000001", `"M00000001"`, "{9}\r\nM00000001",
		"NIL", "()", "\x00",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		dec := imapwire.NewDecoderString(s, nil)
		_, _ = readFetchObjectID(dec)
	})
}

// FuzzReadESearchItems covers the ESEARCH return-item list, RFC 4731 section 3
// and RFC 4466 section 2.6, which every extension adds keys to.
func FuzzReadESearchItems(f *testing.F) {
	for _, s := range []string{
		"", " ", "MIN 1 MAX 10 COUNT 3 ALL 1:10",
		"UID MIN 1", "COUNT 0", "ALL 1,3,5:*",
		"MODSEQ 123456", "MIN", "COUNT NOTANUMBER",
		"MIN 4294967296", "MODSEQ 18446744073709551616",
		"UNKNOWNKEY somevalue", "ALL (",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		dec := imapwire.NewDecoderString(s, nil)
		data := &ESearchData{Values: make(map[ESearchReturnKey]string)}
		_ = readESearchItems(dec, data, false)
	})
}

// FuzzParseUIDValidity and FuzzParseStaticUIDSet cover the two halves of a
// UIDPLUS response code that are parsed from raw text, RFC 4315 section 3.
func FuzzParseUIDValidity(f *testing.F) {
	for _, s := range []string{"", "0", "1", "4294967295", "4294967296", "-1", "0x1", " 1", "1 "} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		_, _ = parseUIDValidity(s)
	})
}

func FuzzParseStaticUIDSet(f *testing.F) {
	for _, s := range []string{
		"", "1", "1:10", "1,3,5", "1:*", "*", "*:1", "0", "10:1",
		",", ":", "1:", ":1", "4294967296", "1:2:3",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		set, err := parseStaticUIDSet(s)
		if err != nil {
			return
		}
		// A UIDPLUS set is static: RFC 4315 section 3 forbids "*" in it, so a
		// successful parse must not yield a range containing the wildcard,
		// which would silently address every message in the mailbox. numset.go
		// spells the wildcard as a zero Start or a zero Stop.
		for _, r := range set {
			if r.Start == 0 || r.Stop == 0 {
				t.Fatalf("parseStaticUIDSet(%q) accepted the wildcard range %v", s, r)
			}
		}
	})
}
