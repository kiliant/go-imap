package imapclient

import (
	"reflect"
	"strings"
	"testing"

	"github.com/kiliant/go-imap/internal/imapwire"
)

// Groups C, D and E introduce their own response parsers. A hostile or merely
// broken server must not be able to panic the client through any of them: an
// error is a fine outcome, a panic on the reader goroutine is not recoverable
// by the caller. Every parser reached from an untagged response or a response
// code gets a target here.

// hostileSeeds are shaped to break the grammar in ways every one of these
// parsers has to survive: truncation, unbalanced lists, deep nesting past
// MaxListDepth, literals that never arrive, numbers past 32 and 64 bits, and
// bytes the wire layer must reject rather than propagate.
var hostileSeeds = []string{
	"",
	"\r\n",
	" \r\n",
	" (",
	" ()\r\n",
	" (()",
	" INBOX\r\n",
	" INBOX \r\n",
	" \"\"\r\n",
	" {5}\r\nabcde\r\n",
	" {5}\r\nabc",
	" {99999999999}\r\n",
	" {-1}\r\n",
	" " + deepList(64),
	" 0 0\r\n",
	" 4294967296 4294967297\r\n",
	" 18446744073709551616\r\n",
	" -1 -1\r\n",
	" \x00\r\n",
	" \xff\xfe\r\n",
	" NIL\r\n",
}

// deepList builds n nested opening parens, to drive the decoder past its
// configured MaxListDepth without relying on the fuzzer to discover nesting.
func deepList(n int) string {
	b := make([]byte, 0, n+2)
	for i := 0; i < n; i++ {
		b = append(b, '(')
	}
	b = append(b, '\r', '\n')
	return string(b)
}

// fuzzResponseParser drives fn with the shared hostile seeds plus the
// RFC-realistic ones the caller supplies. The contract under test is only that
// fn returns rather than panics or blocks; parsers legitimately disagree about
// which malformed inputs are errors.
func fuzzResponseParser(f *testing.F, extra []string, fn func(dec *imapwire.Decoder) (any, error)) {
	for _, s := range hostileSeeds {
		f.Add(s)
	}
	for _, s := range extra {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		dec := imapwire.NewDecoderString(s, nil)
		v, err := fn(dec)
		if err != nil {
			return
		}
		// A parser that reports success must hand back something usable. A
		// (nil, nil) return is the worst outcome of the three: the caller
		// dereferences it believing the response parsed.
		if rv := reflect.ValueOf(v); v == nil || (rv.Kind() == reflect.Ptr && rv.IsNil()) {
			t.Fatalf("parser returned a nil value and a nil error for %q", s)
		}
	})
}

// --- Group C ---------------------------------------------------------------

// FuzzReadThreadForest covers the THREAD response of RFC 5256 section 4. The
// parser is mutually recursive over nested thread lists, so unbalanced and
// deeply nested input is the interesting case.
func FuzzReadThreadForest(f *testing.F) {
	fuzzResponseParser(f, []string{
		" (2)(3 6 (4 23)(44 7 96))\r\n",
		" (1)(2)(3)\r\n",
		" ((((1))))\r\n",
		" (1 (2 (3 (4 (5)))))\r\n",
		" (0)\r\n",
		" (1\r\n",
		" 1)\r\n",
	}, func(d *imapwire.Decoder) (any, error) { return readThreadForest(d) })
}

// FuzzReadMultiSearchResponse covers the ESEARCH response carrying a mailbox
// tag under MULTISEARCH, RFC 7377.
func FuzzReadMultiSearchResponse(f *testing.F) {
	fuzzResponseParser(f, []string{
		" (TAG \"a\") UID COUNT 5 ALL 1:10\r\n",
		" (TAG \"a\" MAILBOX \"INBOX\" UIDVALIDITY 1) COUNT 0\r\n",
		" (TAG) COUNT 1\r\n",
		" (MAILBOX) \r\n",
		" (TAG \"a\") MIN 1 MAX 4294967295\r\n",
	}, func(d *imapwire.Decoder) (any, error) { return readMultiSearchResponse(d) })
}

// --- Group D ---------------------------------------------------------------

// FuzzReadQuotaResponse covers the QUOTA response of RFC 9208 section 5.1.
func FuzzReadQuotaResponse(f *testing.F) {
	fuzzResponseParser(f, []string{
		" \"\" (STORAGE 10 512)\r\n",
		" \"root\" (STORAGE 10 512 MESSAGE 4 100)\r\n",
		" \"root\" ()\r\n",
		" \"root\" (STORAGE -1 512)\r\n",
		" \"root\" (STORAGE 10)\r\n",
		" \"root\" (STORAGE 9223372036854775808 1)\r\n",
	}, func(d *imapwire.Decoder) (any, error) { return readQuotaResponse(d) })
}

// FuzzReadQuotaRootResponse covers QUOTAROOT, RFC 9208 section 5.2.
func FuzzReadQuotaRootResponse(f *testing.F) {
	fuzzResponseParser(f, []string{
		" INBOX \"\"\r\n",
		" INBOX \"root\" \"other\"\r\n",
		" INBOX\r\n",
	}, func(d *imapwire.Decoder) (any, error) { return readQuotaRootResponse(d) })
}

// FuzzReadACLResponse covers the ACL response of RFC 4314 section 3.6.
func FuzzReadACLResponse(f *testing.F) {
	fuzzResponseParser(f, []string{
		" INBOX \"user\" lrswipkxte\r\n",
		" INBOX \"user\" \"\" \"other\" lr\r\n",
		" INBOX \"user\"\r\n",
	}, func(d *imapwire.Decoder) (any, error) { return readACLResponse(d) })
}

// FuzzReadListRightsResponse covers LISTRIGHTS, RFC 4314 section 3.7.
func FuzzReadListRightsResponse(f *testing.F) {
	fuzzResponseParser(f, []string{
		" INBOX \"user\" l r s w i p k x t e\r\n",
		" INBOX \"user\" lr\r\n",
		" INBOX \"user\"\r\n",
	}, func(d *imapwire.Decoder) (any, error) { return readListRightsResponse(d) })
}

// FuzzReadMyRightsResponse covers MYRIGHTS, RFC 4314 section 3.8.
func FuzzReadMyRightsResponse(f *testing.F) {
	fuzzResponseParser(f, []string{
		" INBOX lrswipkxte\r\n",
		" INBOX \"\"\r\n",
	}, func(d *imapwire.Decoder) (any, error) { return readMyRightsResponse(d) })
}

// FuzzReadMetadataResponse covers the METADATA response of RFC 5464 section 4.4,
// in both its list and its extended forms.
func FuzzReadMetadataResponse(f *testing.F) {
	fuzzResponseParser(f, []string{
		" INBOX (/shared/comment \"value\")\r\n",
		" INBOX (/shared/comment NIL)\r\n",
		" INBOX (/shared/comment {5}\r\nvalue)\r\n",
		" INBOX (/a \"1\" /b \"2\")\r\n",
		" INBOX (/shared/comment)\r\n",
		" INBOX /shared/comment\r\n",
	}, func(d *imapwire.Decoder) (any, error) { return readMetadataResponse(d) })
}

// FuzzReadJMAPAccessResponse covers the JMAPACCESS response of RFC 9698.
func FuzzReadJMAPAccessResponse(f *testing.F) {
	fuzzResponseParser(f, []string{
		" \"https://example.com/.well-known/jmap\"\r\n",
		" https://example.com/jmap\r\n",
	}, func(d *imapwire.Decoder) (any, error) { return readJMAPAccessResponse(d) })
}

// --- Group E ---------------------------------------------------------------

// FuzzReadLanguageResponse covers the LANGUAGE response of RFC 5255 section 3.3.
func FuzzReadLanguageResponse(f *testing.F) {
	fuzzResponseParser(f, []string{
		" (\"EN\")\r\n",
		" (\"EN-CA\" \"FR\" \"DE\")\r\n",
		" ()\r\n",
		" (\"EN\"\r\n",
	}, func(d *imapwire.Decoder) (any, error) { return readLanguageResponse(d) })
}

// FuzzReadComparatorResponse covers COMPARATOR, RFC 5255 section 4.7.
func FuzzReadComparatorResponse(f *testing.F) {
	fuzzResponseParser(f, []string{
		" \"i;basic\"\r\n",
		" \"i;basic\" (\"i;octet\" \"i;ascii-casemap\")\r\n",
		" i;basic\r\n",
	}, func(d *imapwire.Decoder) (any, error) { return readComparatorResponse(d) })
}

// FuzzReadGenURLAuthResponse covers GENURLAUTH, RFC 4467 section 3.1.
func FuzzReadGenURLAuthResponse(f *testing.F) {
	fuzzResponseParser(f, []string{
		" \"imap://example.com/INBOX;UID=1;URLAUTH=submit+u:internal:0a1b2c\"\r\n",
		" \"a\" \"b\"\r\n",
	}, func(d *imapwire.Decoder) (any, error) { return readGenURLAuthResponse(d) })
}

// FuzzReadURLFetchResponse covers URLFETCH, RFC 4467 section 3.2, whose items
// pair a URL with an optional literal body.
func FuzzReadURLFetchResponse(f *testing.F) {
	fuzzResponseParser(f, []string{
		" \"imap://example.com/INBOX;UID=1\" {3}\r\nabc\r\n",
		" \"imap://example.com/INBOX;UID=1\" NIL\r\n",
		" \"url\" {3}\r\nab\r\n",
		" \"url\"\r\n",
	}, func(d *imapwire.Decoder) (any, error) { return readURLFetchResponse(d) })
}

// --- string-argument parsers ------------------------------------------------

// FuzzParsePartialReturnValue covers the PARTIAL return value of RFC 9394
// section 3.1, in both its UID and sequence-number forms.
func FuzzParsePartialReturnValue(f *testing.F) {
	for _, s := range []string{
		"", "1:10", "-1:-5", "0:0", "1", "x:y", "1:*", ":", "::::",
		"4294967296:1", "1:4294967296", "-0:-0", "10:1", "1:1",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		_, _ = parsePartialReturnValue(s, true)
		_, _ = parsePartialReturnValue(s, false)
	})
}

// FuzzParseQuotaResourcePair covers the resource triple of a QUOTA response
// driven directly, without a surrounding response line.
func FuzzParseQuotaResourcePair(f *testing.F) {
	f.Add("STORAGE", "10", "512")
	f.Add("", "", "")
	f.Add("MESSAGE", "-1", "0")
	f.Add("X", "18446744073709551616", "18446744073709551615")
	f.Add("storage", " 1", "1 ")
	f.Fuzz(func(t *testing.T, name, usage, limit string) {
		_, _ = parseQuotaResourcePair(name, usage, limit)
	})
}

// FuzzDecodeTransferEncoding covers the BINARY (RFC 3516) content-transfer
// decoding applied to a fetched section, which runs on server-supplied bytes
// and a server-supplied encoding name.
func FuzzDecodeTransferEncoding(f *testing.F) {
	f.Add([]byte("aGVsbG8="), "base64")
	f.Add([]byte("=41=42=\r\nC"), "quoted-printable")
	f.Add([]byte(""), "")
	f.Add([]byte("\xff\xfe"), "BASE64")
	f.Add([]byte("not base64 at all"), "base64")
	f.Add([]byte("plain"), "7bit")
	f.Add([]byte("=="), "quoted-printable")
	f.Fuzz(func(t *testing.T, raw []byte, cte string) {
		_, _ = decodeTransferEncoding(raw, cte)
	})
}

// --- response-code argument parsers ----------------------------------------
//
// These are exported: a caller hands them the raw argument text of a response
// code the server sent, so they are directly reachable public attack surface.
// Each takes a single string, so one shared driver covers them all.

func fuzzArgsParser(f *testing.F, extra []string, fn func(args string) error) {
	for _, s := range []string{
		"", " ", "()", "(", ")", "NIL", "0", "-1",
		"4294967296", "18446744073709551616", "9223372036854775808",
		"\x00", "\xff\xfe", "a b c", "(TAG \"x\")", "1:*", "\"unterminated",
		strings.Repeat("(", 64), strings.Repeat("9", 4096),
	} {
		f.Add(s)
	}
	for _, s := range extra {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		_ = fn(s)
	})
}

// FuzzParseInProgressArgs covers the INPROGRESS response code, RFC 9585 §5.
func FuzzParseInProgressArgs(f *testing.F) {
	fuzzArgsParser(f, []string{`("tag" 25 100)`, `("tag" NIL NIL)`, `()`, `("tag" 100 25)`},
		func(s string) error { _, err := ParseInProgressArgs(s); return err })
}

// FuzzParseMessageLimitArgs covers the MESSAGELIMIT response code, RFC 9738.
func FuzzParseMessageLimitArgs(f *testing.F) {
	fuzzArgsParser(f, []string{"100", "0", "4294967295"},
		func(s string) error { _, err := ParseMessageLimitArgs(s); return err })
}

// FuzzParseNoUpdateArgs covers the NOUPDATE response code, RFC 5267 §4.4.
func FuzzParseNoUpdateArgs(f *testing.F) {
	fuzzArgsParser(f, []string{`"tag"`, `tag`, `""`},
		func(s string) error { _, err := ParseNoUpdateArgs(s); return err })
}

// FuzzParseReferralArgs covers the REFERRAL response code, RFC 2221 / RFC 2193.
func FuzzParseReferralArgs(f *testing.F) {
	fuzzArgsParser(f, []string{"imap://example.com/", "imap://a/ imap://b/", "not-a-url"},
		func(s string) error { _, err := ParseReferralArgs(s); return err })
}

// FuzzParseAnnotateArgs covers the ANNOTATE response code, RFC 5257.
func FuzzParseAnnotateArgs(f *testing.F) {
	fuzzArgsParser(f, []string{"TOOBIG", "TOOMANY"},
		func(s string) error { _, err := ParseAnnotateArgs(s); return err })
}

// FuzzParseAnnotationsArgs covers the ANNOTATIONS response code, RFC 5257.
func FuzzParseAnnotationsArgs(f *testing.F) {
	fuzzArgsParser(f, []string{"NONE", "READ-ONLY", "42"},
		func(s string) error { _, err := ParseAnnotationsArgs(s); return err })
}

// FuzzParseMaxConvertMessagesArgs covers MAXCONVERTMESSAGES, RFC 5259.
func FuzzParseMaxConvertMessagesArgs(f *testing.F) {
	fuzzArgsParser(f, []string{"10", "0"},
		func(s string) error { _, err := ParseMaxConvertMessagesArgs(s); return err })
}

// FuzzParseMaxConvertPartsArgs covers MAXCONVERTPARTS, RFC 5259.
func FuzzParseMaxConvertPartsArgs(f *testing.F) {
	fuzzArgsParser(f, []string{"10", "0"},
		func(s string) error { _, err := ParseMaxConvertPartsArgs(s); return err })
}

// FuzzParseUndefinedFilterArgs covers UNDEFINED-FILTER, RFC 5466 §6.
func FuzzParseUndefinedFilterArgs(f *testing.F) {
	fuzzArgsParser(f, []string{"myfilter", `"my filter"`},
		func(s string) error { _, err := ParseUndefinedFilterArgs(s); return err })
}

// The remaining two read an already-parsed ESearchData rather than raw bytes,
// but the map values are verbatim server text, so they are fuzzed through it.

// FuzzParseRelevancyScores covers RELEVANCY, RFC 6203 §3 (SEARCH=FUZZY).
func FuzzParseRelevancyScores(f *testing.F) {
	f.Add("RELEVANCY", "(1 30 100)")
	f.Add("RELEVANCY", "()")
	f.Add("RELEVANCY", "(0 101 255 256)")
	f.Add("RELEVANCY", "notalist")
	f.Fuzz(func(t *testing.T, key, value string) {
		data := &ESearchData{Values: map[ESearchReturnKey]string{ESearchReturnKey(key): value}}
		_, _ = ParseRelevancyScores(data)
		_, _ = ParseRelevancyScores(nil)
	})
}

// FuzzParsePartialSearchData covers the PARTIAL SEARCH return item, RFC 9394.
func FuzzParsePartialSearchData(f *testing.F) {
	f.Add("PARTIAL", "(1:10 1,3,5)")
	f.Add("PARTIAL", "(-1:-5 NIL)")
	f.Add("PARTIAL", "()")
	f.Add("PARTIAL", "(1:10)")
	f.Fuzz(func(t *testing.T, key, value string) {
		data := &ESearchData{Values: map[ESearchReturnKey]string{ESearchReturnKey(key): value}}
		_, _ = ParsePartialSearchData(data)
		_, _ = ParsePartialSearchData(nil)
	})
}
