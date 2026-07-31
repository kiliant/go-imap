package imap

import (
	"bytes"
	"errors"
	"io"
	"mime"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// errUnsupportedCharset is returned by the internal charset reader for a
// charset this library has no table for. It is never returned to callers: the
// decoding entry points fall back to returning their input unchanged.
var errUnsupportedCharset = errors.New("imap: unsupported charset")

// DecodeHeader decodes the RFC 2047 encoded-words in a message header value,
// returning UTF-8 text.
//
// It applies the RFC 2047 section 6.2 rule that whitespace between two adjacent
// encoded-words is not part of the text, and leaves unencoded runs untouched.
//
// Decoding is total: it never returns an error and never panics. Because this
// library carries no external dependencies it has tables only for US-ASCII,
// UTF-8, ISO-8859-1 and Windows-1252. If any encoded-word names another
// charset, or if the input is malformed, s is returned unchanged rather than
// partially decoded — an undecoded header is recoverable by the caller, a
// half-decoded one is not.
//
// RFC 2047, RFC 2231 section 5 (the extended encoded-word syntax carrying a
// language tag) — the language tag is accepted and discarded.
func DecodeHeader(s string) string {
	if !strings.Contains(s, "=?") {
		return s
	}
	dec := mime.WordDecoder{CharsetReader: charsetReader}
	out, err := dec.DecodeHeader(s)
	if err != nil {
		return s
	}
	return out
}

// DecodeParams assembles the RFC 2231 continuations and extended-value
// encodings in a MIME parameter list and returns the decoded parameters.
//
// It handles all three forms defined by RFC 2231:
//
//	name*=utf-8''%C2%A3.txt          extended value, single section
//	name*0="long "; name*1="value"   continuation, no encoding
//	name*0*=utf-8''%C2%A3; name*1*=x continuation with encoding
//
// Parameter names are matched case-insensitively and returned lower-cased, as
// RFC 2045 section 5.1 specifies. Sections are assembled in ascending order;
// RFC 2231 section 3 requires them to be contiguous from zero, so assembly
// stops at the first gap and whatever was assembled up to that point is kept
// rather than the whole parameter being dropped.
//
// Decoding is total. A percent escape that is not two hexadecimal digits is
// kept verbatim, and a value whose charset this library has no table for is
// returned with its octets unchanged; see [DecodeHeader] for the charsets
// supported.
//
// The RFC 2231 language tag is parsed and discarded: the decoded value is
// always UTF-8 text.
//
// params may be nil, in which case nil is returned.
func DecodeParams(params map[string]string) map[string]string {
	if params == nil {
		return nil
	}

	// section holds one piece of a possibly continued parameter.
	type section struct {
		num     int
		value   string
		encoded bool
		part    bool
	}
	sectionPriority := func(sec section) int {
		if sec.encoded {
			return 2
		}
		if sec.part {
			return 1
		}
		return 0
	}
	grouped := make(map[string]map[int]section, len(params))
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		name, num, encoded, part := splitParamKey(k)
		if grouped[name] == nil {
			grouped[name] = make(map[int]section)
		}
		candidate := section{num: num, value: params[k], encoded: encoded, part: part}
		current, exists := grouped[name][num]
		if !exists || sectionPriority(candidate) > sectionPriority(current) {
			grouped[name][num] = candidate
		}
	}

	out := make(map[string]string, len(grouped))
	for name, byNum := range grouped {
		secs := make([]section, 0, len(byNum))
		for _, sec := range byNum {
			secs = append(secs, sec)
		}
		sort.Slice(secs, func(i, j int) bool { return secs[i].num < secs[j].num })

		var (
			b       strings.Builder
			charset string
			anyEnc  bool
		)
		for i, sec := range secs {
			if sec.num != i {
				// A gap: RFC 2231 section 3 forbids it. Keep what we
				// have rather than discarding the parameter.
				break
			}
			v := sec.value
			if sec.encoded {
				anyEnc = true
				if i == 0 {
					charset, v = splitExtendedValue(v)
				}
				v = percentDecode(v)
			}
			b.WriteString(v)
		}
		v := b.String()
		if anyEnc {
			v = convertCharset(charset, v)
		}
		out[name] = v
	}
	return out
}

// splitParamKey splits an RFC 2231 parameter key into its attribute name, its
// section number and whether the section is percent-encoded.
func splitParamKey(k string) (name string, num int, encoded, part bool) {
	name = strings.ToLower(k)
	if strings.HasSuffix(name, "*") {
		encoded = true
		name = name[:len(name)-1]
	}
	if i := strings.LastIndexByte(name, '*'); i >= 0 {
		if n, err := strconv.Atoi(name[i+1:]); err == nil && n >= 0 {
			return name[:i], n, encoded, true
		}
	}
	return name, 0, encoded, false
}

// splitExtendedValue splits the charset'language' prefix of an RFC 2231
// extended value. A value without the prefix is returned unchanged with an
// empty charset.
func splitExtendedValue(v string) (charset, rest string) {
	i := strings.IndexByte(v, '\'')
	if i < 0 {
		return "", v
	}
	j := strings.IndexByte(v[i+1:], '\'')
	if j < 0 {
		return "", v
	}
	// v[i+1 : i+1+j] is the language tag, which is metadata about the value
	// rather than part of it.
	return v[:i], v[i+1+j+1:]
}

// percentDecode decodes %XX escapes. An escape that is not two hexadecimal
// digits is kept verbatim, so decoding cannot fail.
func percentDecode(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			hi, ok1 := unhex(s[i+1])
			lo, ok2 := unhex(s[i+2])
			if ok1 && ok2 {
				b.WriteByte(hi<<4 | lo)
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func unhex(c byte) (byte, bool) {
	switch {
	case '0' <= c && c <= '9':
		return c - '0', true
	case 'a' <= c && c <= 'f':
		return c - 'a' + 10, true
	case 'A' <= c && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// convertCharset converts s from the named charset to UTF-8, returning s
// unchanged when the charset is unknown.
func convertCharset(charset, s string) string {
	r, err := charsetReader(charset, strings.NewReader(s))
	if err != nil {
		return s
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return s
	}
	return string(b)
}

// charsetReader is the [mime.WordDecoder] CharsetReader for the charsets this
// library can decode without an external dependency.
func charsetReader(charset string, input io.Reader) (io.Reader, error) {
	if base, _, ok := strings.Cut(charset, "*"); ok {
		charset = base
	}
	switch strings.ToLower(charset) {
	case "", "utf-8", "utf8", "us-ascii", "ascii", "iso-ir-6", "unknown-8bit":
		return input, nil
	case "iso-8859-1", "iso8859-1", "iso_8859-1", "latin1", "latin-1", "l1", "cp819":
		return newTableReader(input, nil), nil
	case "windows-1252", "cp1252", "cp-1252", "windows1252":
		return newTableReader(input, &cp1252High), nil
	default:
		return nil, errUnsupportedCharset
	}
}

// cp1252High maps the 0x80–0x9F range of Windows-1252, which is the only range
// in which it differs from ISO-8859-1. Unassigned positions map to U+FFFD.
var cp1252High = [32]rune{
	0x20AC, 0xFFFD, 0x201A, 0x0192, 0x201E, 0x2026, 0x2020, 0x2021,
	0x02C6, 0x2030, 0x0160, 0x2039, 0x0152, 0xFFFD, 0x017D, 0xFFFD,
	0xFFFD, 0x2018, 0x2019, 0x201C, 0x201D, 0x2022, 0x2013, 0x2014,
	0x02DC, 0x2122, 0x0161, 0x203A, 0x0153, 0xFFFD, 0x017E, 0x0178,
}

// newTableReader converts a single-byte charset to UTF-8. Bytes below 0x80 map
// to themselves; bytes from 0x80 upward map through high, or to the identical
// code point when high is nil, which is the ISO-8859-1 behaviour.
func newTableReader(input io.Reader, high *[32]rune) io.Reader {
	b, err := io.ReadAll(input)
	if err != nil {
		return &errReader{err}
	}
	var out bytes.Buffer
	out.Grow(len(b))
	for _, c := range b {
		switch {
		case c < utf8.RuneSelf:
			out.WriteByte(c)
		case high != nil && c < 0xA0:
			out.WriteRune(high[c-0x80])
		default:
			out.WriteRune(rune(c))
		}
	}
	return &out
}

type errReader struct{ err error }

func (r *errReader) Read([]byte) (int, error) { return 0, r.err }

// ParseMessageIDList splits a header value holding a list of message
// identifiers — In-Reply-To or References — into the individual identifiers,
// each including its angle brackets. RFC 5322 section 3.6.4.
//
// Text outside angle brackets, such as the obsolete phrase form, is ignored.
// An unterminated bracket yields no identifier. Parsing is total.
func ParseMessageIDList(s string) []string {
	var out []string
	for {
		i := strings.IndexByte(s, '<')
		if i < 0 {
			return out
		}
		j := strings.IndexByte(s[i:], '>')
		if j < 0 {
			return out
		}
		out = append(out, s[i:i+j+1])
		s = s[i+j+1:]
	}
}
