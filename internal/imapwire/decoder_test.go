package imapwire

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func dec(s string) *Decoder { return NewDecoderString(s, nil) }

func decOpts(s string, opts *Options) *Decoder { return NewDecoderString(s, opts) }

// rest returns whatever the decoder has not consumed, which is how the tests
// check that a production stopped in the right place.
func rest(t *testing.T, d *Decoder) string {
	t.Helper()
	var sb strings.Builder
	if _, err := io.Copy(&sb, d.r); err != nil {
		t.Fatalf("draining decoder: %v", err)
	}
	return sb.String()
}

func TestAtom(t *testing.T) {
	tests := []struct {
		name, in, want, rest string
		ok                   bool
	}{
		{name: "simple", in: "FETCH", want: "FETCH", ok: true},
		{name: "stops at SP", in: "FETCH 1", want: "FETCH", rest: " 1", ok: true},
		{name: "stops at paren", in: "A(", want: "A", rest: "(", ok: true},
		{name: "stops at resp-special", in: "OK]", want: "OK", rest: "]", ok: true},
		{name: "dotted", in: "RFC822.SIZE", want: "RFC822.SIZE", ok: true},
		{name: "brackets are atom chars", in: "BODY[1]", want: "BODY[1", rest: "]", ok: true},
		{name: "8-bit accepted", in: "Caf\xc3\xa9", want: "Caf\xc3\xa9", ok: true},
		{name: "empty", in: "", ok: false},
		{name: "wildcard is not an atom char", in: "*", rest: "*", ok: false},
		{name: "quote is not an atom char", in: `"x"`, rest: `"x"`, ok: false},
		{name: "control is not an atom char", in: "\x01", rest: "\x01", ok: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := dec(tc.in)
			var got string
			if ok := d.Atom(&got); ok != tc.ok {
				t.Fatalf("Atom() = %v, want %v (err %v)", ok, tc.ok, d.Err())
			}
			if got != tc.want {
				t.Errorf("Atom() gave %q, want %q", got, tc.want)
			}
			if !tc.ok && d.Err() != nil {
				t.Errorf("a non-match must not record an error, got %v", d.Err())
			}
			if r := rest(t, d); r != tc.rest {
				t.Errorf("unconsumed input %q, want %q", r, tc.rest)
			}
		})
	}
}

func TestQuoted(t *testing.T) {
	tests := []struct {
		name, in, want string
		ok             bool
		errIs          error
	}{
		{name: "simple", in: `"hello"`, want: "hello", ok: true},
		{name: "empty", in: `""`, want: "", ok: true},
		{name: "escaped quote", in: `"a\"b"`, want: `a"b`, ok: true},
		{name: "escaped backslash", in: `"a\\b"`, want: `a\b`, ok: true},
		{name: "8-bit tolerated", in: "\"caf\xc3\xa9\"", want: "caf\xc3\xa9", ok: true},
		{name: "not quoted", in: "atom", ok: false},
		{name: "unterminated", in: `"abc`, errIs: ErrUnexpectedEOF},
		{name: "bad escape", in: `"a\qb"`, errIs: ErrSyntax},
		{name: "LF inside", in: "\"a\nb\"", errIs: ErrSyntax},
		{name: "CR inside", in: "\"a\rb\"", errIs: ErrSyntax},
		{name: "NUL inside", in: "\"a\x00b\"", errIs: ErrSyntax},
		{name: "trailing backslash", in: `"a\`, errIs: ErrUnexpectedEOF},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := dec(tc.in)
			var got string
			ok := d.Quoted(&got)
			if tc.errIs != nil {
				if ok {
					t.Fatalf("Quoted() unexpectedly succeeded with %q", got)
				}
				if !errors.Is(d.Err(), tc.errIs) {
					t.Fatalf("error = %v, want %v", d.Err(), tc.errIs)
				}
				return
			}
			if ok != tc.ok {
				t.Fatalf("Quoted() = %v, want %v (err %v)", ok, tc.ok, d.Err())
			}
			if got != tc.want {
				t.Errorf("Quoted() gave %q, want %q", got, tc.want)
			}
		})
	}
}

func TestString(t *testing.T) {
	tests := []struct {
		name, in, want string
		ok             bool
		errIs          error
	}{
		{name: "quoted", in: `"hi"`, want: "hi", ok: true},
		{name: "literal", in: "{5}\r\nhello", want: "hello", ok: true},
		{name: "empty literal", in: "{0}\r\n", want: "", ok: true},
		{name: "literal with NUL via literal8", in: "~{3}\r\na\x00b", want: "a\x00b", ok: true},
		{name: "NUL in ordinary literal", in: "{3}\r\na\x00b", errIs: ErrSyntax},
		{name: "literal with LF", in: "{4}\r\na\r\nb", want: "a\r\nb", ok: true},
		{name: "non-sync literal accepted", in: "{5+}\r\nhello", want: "hello", ok: true},
		{name: "not a string", in: "atom", ok: false},
		{name: "truncated payload", in: "{5}\r\nhi", errIs: ErrUnexpectedEOF},
		{name: "no CRLF after count", in: "{5} hello", errIs: ErrSyntax},
		{name: "no closing brace", in: "{5 }\r\nhello", errIs: ErrSyntax},
		{name: "empty count", in: "{}\r\n", errIs: ErrSyntax},
		{name: "huge count", in: "{4294967295}\r\n", errIs: ErrLimitExceeded},
		{name: "overflowing count", in: "{99999999999999999999999}\r\n", errIs: ErrSyntax},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := dec(tc.in)
			var got string
			ok := d.String(&got)
			if tc.errIs != nil {
				if ok {
					t.Fatalf("String() unexpectedly succeeded with %q", got)
				}
				if !errors.Is(d.Err(), tc.errIs) {
					t.Fatalf("error = %v, want %v", d.Err(), tc.errIs)
				}
				if !d.Fatal() {
					t.Errorf("a broken literal must be fatal, got %v", d.Err())
				}
				return
			}
			if ok != tc.ok {
				t.Fatalf("String() = %v, want %v (err %v)", ok, tc.ok, d.Err())
			}
			if got != tc.want {
				t.Errorf("String() gave %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAstring(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{name: "atom", in: "INBOX", want: "INBOX"},
		{name: "tilde atom", in: "~user", want: "~user"},
		{name: "resp-special allowed", in: "a]b", want: "a]b"},
		{name: "quoted", in: `"a b"`, want: "a b"},
		{name: "literal", in: "{3}\r\nabc", want: "abc"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := dec(tc.in)
			var got string
			if !d.ExpectAstring(&got) {
				t.Fatalf("ExpectAstring() failed: %v", d.Err())
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestListMailbox(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{in: "*", want: "*"},
		{in: "%/Archive", want: "%/Archive"},
		{in: "~user/*", want: "~user/*"},
		{in: `"two words/*"`, want: "two words/*"},
	} {
		d := dec(tc.in)
		var got string
		if !d.ListMailbox(&got) {
			t.Fatalf("ListMailbox(%q): %v", tc.in, d.Err())
		}
		if got != tc.want {
			t.Fatalf("ListMailbox(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNString(t *testing.T) {
	tests := []struct {
		name, in, want string
		isNil          bool
		fails          bool
	}{
		{name: "NIL", in: "NIL", want: "", isNil: true},
		{name: "nil lowercase", in: "nil", want: "", isNil: true},
		{name: "empty string is not NIL", in: `""`, want: ""},
		{name: "atom starting with NIL is not NIL", in: "NILE", fails: true},
		{name: "quoted", in: `"x"`, want: "x"},
		{name: "literal", in: "{1}\r\nx", want: "x"},
		{name: "bare atom is not an nstring", in: "abc", fails: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := dec(tc.in)
			var got string
			var isNil bool
			ok := d.ExpectNString(&got, &isNil)
			if tc.fails {
				if ok {
					t.Fatalf("ExpectNString() unexpectedly succeeded with %q", got)
				}
				if d.Err() == nil {
					t.Fatal("expected an error to be recorded")
				}
				return
			}
			if !ok {
				t.Fatalf("ExpectNString() failed: %v", d.Err())
			}
			if got != tc.want || isNil != tc.isNil {
				t.Errorf("got (%q, nil=%v), want (%q, nil=%v)", got, isNil, tc.want, tc.isNil)
			}
		})
	}
}

func TestNumber(t *testing.T) {
	tests := []struct {
		name, in string
		want     uint32
		ok       bool
	}{
		{name: "zero", in: "0", want: 0, ok: true},
		{name: "max", in: "4294967295", want: 4294967295, ok: true},
		{name: "leading zeros", in: "007", want: 7, ok: true},
		{name: "stops at non-digit", in: "12a", want: 12, ok: true},
		{name: "overflow", in: "4294967296", ok: false},
		{name: "far overflow", in: "99999999999999999999999999", ok: false},
		{name: "empty", in: "", ok: false},
		{name: "negative", in: "-1", ok: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := dec(tc.in)
			var got uint32
			if ok := d.Number(&got); ok != tc.ok {
				t.Fatalf("Number() = %v, want %v (err %v)", ok, tc.ok, d.Err())
			}
			if tc.ok && got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
			if !tc.ok && d.Err() != nil && d.Fatal() {
				t.Errorf("a bad number must stay recoverable, got %v", d.Err())
			}
		})
	}
}

func TestNZNumber(t *testing.T) {
	d := dec("0")
	var n uint32
	if d.ExpectNZNumber(&n) {
		t.Fatal("nz-number must reject 0")
	}
	if !errors.Is(d.Err(), ErrSyntax) {
		t.Errorf("error = %v, want a syntax error", d.Err())
	}
	d = dec("1")
	if !d.ExpectNZNumber(&n) || n != 1 {
		t.Errorf("ExpectNZNumber(1) = %d, %v", n, d.Err())
	}
}

func TestNumber64(t *testing.T) {
	d := dec("9223372036854775807")
	var n int64
	if !d.ExpectNumber64(&n) || n != 1<<63-1 {
		t.Errorf("got %d, err %v", n, d.Err())
	}
	d = dec("9223372036854775808")
	if d.ExpectNumber64(&n) {
		t.Error("number64 must reject values above 2^63-1")
	}
}

func TestList(t *testing.T) {
	tests := []struct {
		name, in string
		want     []string
		fails    bool
	}{
		{name: "empty", in: "()", want: nil},
		{name: "one", in: "(A)", want: []string{"A"}},
		{name: "several", in: "(A B C)", want: []string{"A", "B", "C"}},
		{name: "unterminated", in: "(A B", fails: true},
		{name: "double space", in: "(A  B)", fails: true},
		{name: "trailing space", in: "(A )", fails: true},
		{name: "not a list", in: "A", fails: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := dec(tc.in)
			var got []string
			err := d.ExpectList(func() error {
				var s string
				if !d.ExpectAtom(&s) {
					return d.Err()
				}
				got = append(got, s)
				return nil
			})
			if tc.fails {
				if err == nil {
					t.Fatalf("expected failure, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ExpectList: %v", err)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestListDepthLimit(t *testing.T) {
	const depth = 8
	opts := &Options{MaxListDepth: depth}
	var nest func(d *Decoder) error
	nest = func(d *Decoder) error {
		return d.ExpectList(func() error { return nest(d) })
	}

	// One level under the limit is accepted.
	ok := strings.Repeat("(", depth) + strings.Repeat(")", depth)
	d := decOpts(ok, opts)
	if err := nest(d); err != nil {
		t.Fatalf("%d levels rejected: %v", depth, err)
	}

	// One level over is refused, and refused fatally: the parser cannot know
	// how much of the rest of the line belongs to the list it abandoned.
	tooDeep := strings.Repeat("(", depth+1) + strings.Repeat(")", depth+1)
	d = decOpts(tooDeep, opts)
	err := nest(d)
	if !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("error = %v, want ErrLimitExceeded", err)
	}
	if !IsFatal(err) {
		t.Error("exceeding the nesting limit must be fatal")
	}

	// A pathological input must not run away either.
	d = decOpts(strings.Repeat("(", 5000), opts)
	if err := nest(d); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("error = %v, want ErrLimitExceeded", err)
	}
}

func TestLineLengthLimit(t *testing.T) {
	opts := &Options{MaxLineLength: 16}
	d := decOpts(strings.Repeat("A", 64), opts)
	var s string
	if d.Atom(&s) {
		t.Fatal("an over-long atom must be rejected")
	}
	if !errors.Is(d.Err(), ErrLimitExceeded) {
		t.Fatalf("error = %v, want ErrLimitExceeded", d.Err())
	}
	if !d.Fatal() {
		t.Error("exceeding the line limit must be fatal")
	}

	// The budget is per line: a second line starts fresh.
	d = decOpts("AAAA\r\nBBBB\r\n", opts)
	for i := 0; i < 2; i++ {
		if !d.Atom(&s) || !d.ExpectCRLF() {
			t.Fatalf("line %d: %v", i, d.Err())
		}
	}

	// Literal payloads are not charged to the line budget.
	d = decOpts("{32}\r\n"+strings.Repeat("x", 32)+"\r\n", opts)
	if !d.String(&s) || len(s) != 32 {
		t.Fatalf("literal payload must not count towards the line limit: %v", d.Err())
	}
}

func TestLiteralStreaming(t *testing.T) {
	const payload = "0123456789"
	d := dec("{10}\r\n" + payload + "\r\n* OK done\r\n")
	lr, ok := d.Literal()
	if !ok {
		t.Fatalf("Literal(): %v", d.Err())
	}
	if lr.Size() != 10 || lr.Binary() {
		t.Fatalf("Size=%d Binary=%v", lr.Size(), lr.Binary())
	}

	// Parsing anything before the payload has been consumed must fail, and fail
	// fatally: it would attribute payload octets to the next response.
	var s string
	if d.Atom(&s) {
		t.Fatal("decoding while a literal is pending must fail")
	}
	if !errors.Is(d.Err(), ErrLiteralPending) || !d.Fatal() {
		t.Fatalf("error = %v, want a fatal ErrLiteralPending", d.Err())
	}
}

func TestLiteralDrainThenContinue(t *testing.T) {
	d := dec("{10}\r\n0123456789\r\n* OK done\r\n")
	lr, ok := d.Literal()
	if !ok {
		t.Fatalf("Literal(): %v", d.Err())
	}
	var buf bytes.Buffer
	if n, err := io.Copy(&buf, lr); err != nil || n != 10 {
		t.Fatalf("copied %d octets: %v", n, err)
	}
	if buf.String() != "0123456789" {
		t.Fatalf("payload = %q", buf.String())
	}
	if !d.ExpectCRLF() {
		t.Fatalf("CRLF after payload: %v", d.Err())
	}
	kind, _, err := d.BeginResponse()
	if err != nil || kind != ResponseUntagged {
		t.Fatalf("BeginResponse() = %v, %v", kind, err)
	}
}

func TestLiteralDiscard(t *testing.T) {
	d := dec("{10}\r\n0123456789\r\n")
	lr, ok := d.Literal()
	if !ok {
		t.Fatalf("Literal(): %v", d.Err())
	}
	if err := lr.Discard(); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	if err := lr.Discard(); err != nil {
		t.Fatalf("Discard must be idempotent: %v", err)
	}
	if !d.ExpectCRLF() {
		t.Fatalf("CRLF after discarded payload: %v", d.Err())
	}
}

// finalEOFReader models a legal io.Reader that returns the final literal bytes
// and io.EOF in the same call.
type finalEOFReader struct {
	announcement bool
	payload      string
}

func (r *finalEOFReader) Read(p []byte) (int, error) {
	if !r.announcement {
		r.announcement = true
		return copy(p, "{5000}\r\n"), nil
	}
	if r.payload == "" {
		r.payload = strings.Repeat("x", 5000)
	}
	n := copy(p, r.payload)
	r.payload = r.payload[n:]
	return n, io.EOF
}

func TestLiteralAcceptsFinalBytesWithEOF(t *testing.T) {
	d := NewDecoder(&finalEOFReader{}, &Options{MaxLiteralSize: 5000})
	lr, ok := d.Literal()
	if !ok {
		t.Fatal(d.Err())
	}
	n, err := io.Copy(io.Discard, lr)
	if err != nil || n != 5000 {
		t.Fatalf("copied %d bytes: %v", n, err)
	}
	if d.Err() != nil {
		t.Fatalf("complete literal poisoned decoder: %v", d.Err())
	}
}

func TestLiteralSizeLimit(t *testing.T) {
	opts := &Options{MaxLiteralSize: 16}
	d := decOpts("{17}\r\n", opts)
	if _, ok := d.Literal(); ok {
		t.Fatal("a literal over the limit must be rejected")
	}
	if !errors.Is(d.Err(), ErrLimitExceeded) || !d.Fatal() {
		t.Fatalf("error = %v, want a fatal ErrLimitExceeded", d.Err())
	}

	// The rejection must happen without reading the payload; there is none here
	// at all, which a decoder that tried to allocate or skip would notice.
	d = decOpts("{16}\r\n"+strings.Repeat("x", 16), opts)
	if _, ok := d.Literal(); !ok {
		t.Fatalf("a literal at the limit must be accepted: %v", d.Err())
	}
}

func TestBufferedLiteralLimit(t *testing.T) {
	opts := &Options{MaxBufferedLiteralSize: 8}
	d := decOpts("{9}\r\n123456789", opts)
	var s string
	if d.String(&s) {
		t.Fatal("a literal over the in-memory limit must be rejected")
	}
	if !errors.Is(d.Err(), ErrLimitExceeded) {
		t.Fatalf("error = %v, want ErrLimitExceeded", d.Err())
	}
	// The streaming path is bounded by the larger limit and still works.
	d = decOpts("{9}\r\n123456789", opts)
	lr, ok := d.Literal()
	if !ok {
		t.Fatalf("Literal(): %v", d.Err())
	}
	if err := lr.Discard(); err != nil {
		t.Fatalf("Discard: %v", err)
	}
}

func TestCRLF(t *testing.T) {
	tests := []struct {
		name, in string
		ok       bool
		errIs    error
	}{
		{name: "CRLF", in: "\r\n", ok: true},
		{name: "bare LF accepted", in: "\n", ok: true},
		{name: "lone CR", in: "\rx", errIs: ErrSyntax},
		{name: "not a terminator", in: "x", ok: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := dec(tc.in)
			ok := d.CRLF()
			if tc.errIs != nil {
				if ok || !errors.Is(d.Err(), tc.errIs) {
					t.Fatalf("got ok=%v err=%v", ok, d.Err())
				}
				return
			}
			if ok != tc.ok {
				t.Fatalf("CRLF() = %v, want %v", ok, tc.ok)
			}
		})
	}
}

func TestFlags(t *testing.T) {
	tests := []struct {
		name, in string
		want     []string
		fails    bool
	}{
		{name: "empty", in: "()", want: []string{}},
		{name: "system", in: `(\Seen \Answered)`, want: []string{`\Seen`, `\Answered`}},
		{name: "keyword", in: `(\Seen $Forwarded junk)`, want: []string{`\Seen`, "$Forwarded", "junk"}},
		{name: "wildcard", in: `(\Seen \*)`, want: []string{`\Seen`, `\*`}},
		{name: "case preserved", in: `(\sEEN)`, want: []string{`\sEEN`}},
		{name: "mbx-list flags", in: `(\HasNoChildren \Subscribed)`, want: []string{`\HasNoChildren`, `\Subscribed`}},
		{name: "bare backslash", in: `(\)`, fails: true},
		{name: "unterminated", in: `(\Seen`, fails: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := dec(tc.in)
			var got []string
			err := d.ExpectFlagList(&got)
			if tc.fails {
				if err == nil {
					t.Fatalf("expected failure, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ExpectFlagList: %v", err)
			}
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMailbox(t *testing.T) {
	tests := []struct {
		name, in, want string
		utf8Accept     bool
	}{
		{name: "inbox is case-insensitive", in: "iNbOx", want: "INBOX"},
		{name: "plain", in: "Sent", want: "Sent"},
		{name: "quoted with space", in: `"My Mail"`, want: "My Mail"},
		{name: "modified UTF-7", in: `"~peter/mail/&U,BTFw-/&ZeVnLIqe-"`, want: "~peter/mail/台北/日本語"},
		{name: "ampersand escape", in: "R&-D", want: "R&D"},
		{name: "undecodable name passes through", in: "R&D", want: "R&D"},
		{name: "raw UTF-8 under UTF8=ACCEPT", in: "\"caf\xc3\xa9\"", want: "café", utf8Accept: true},
		{name: "UTF-7 not decoded under UTF8=ACCEPT", in: "R&-D", want: "R&-D", utf8Accept: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := dec(tc.in)
			d.SetUTF8Accept(tc.utf8Accept)
			var got string
			if !d.ExpectMailbox(&got) {
				t.Fatalf("ExpectMailbox: %v", d.Err())
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
	d := dec("\"\xff\"")
	d.SetUTF8Accept(true)
	var mailbox string
	if d.ExpectMailbox(&mailbox) {
		t.Fatal("invalid UTF-8 mailbox was accepted under UTF8=ACCEPT")
	}
}

func TestRespText(t *testing.T) {
	tests := []struct {
		name, in         string
		code, args, text string
		fails            bool
	}{
		{name: "no code", in: "Logged in", text: "Logged in"},
		{name: "bare code", in: "[ALERT] disk full", code: "ALERT", text: "disk full"},
		{name: "code with args", in: "[UNSEEN 12] first unseen", code: "UNSEEN", args: "12", text: "first unseen"},
		{name: "parenthesised args", in: `[PERMANENTFLAGS (\Seen \*)] limited`, code: "PERMANENTFLAGS", args: `(\Seen \*)`, text: "limited"},
		{name: "bracket in quoted args", in: `[BADCHARSET ("a]b")] nope`, code: "BADCHARSET", args: `("a]b")`, text: "nope"},
		{name: "code lowercased on the wire", in: "[read-write] ok", code: "READ-WRITE", text: "ok"},
		{name: "empty text after code", in: "[READ-ONLY]", code: "READ-ONLY"},
		{name: "unterminated code", in: "[ALERT disk full", fails: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := dec(tc.in)
			var got RespText
			ok := d.ExpectRespText(&got)
			if tc.fails {
				if ok {
					t.Fatalf("expected failure, got %+v", got)
				}
				return
			}
			if !ok {
				t.Fatalf("ExpectRespText: %v", d.Err())
			}
			if got.Code != tc.code || got.Args != tc.args || got.Text != tc.text {
				t.Errorf("got %+v, want code=%q args=%q text=%q", got, tc.code, tc.args, tc.text)
			}
		})
	}
}

func TestRespCond(t *testing.T) {
	tests := []struct {
		name, in, status, text string
		fails                  bool
	}{
		{name: "ok", in: "OK SELECT completed", status: "OK", text: "SELECT completed"},
		{name: "no", in: "NO no such mailbox", status: "NO", text: "no such mailbox"},
		{name: "bad", in: "BAD command unknown", status: "BAD", text: "command unknown"},
		{name: "preauth", in: "PREAUTH ready", status: "PREAUTH", text: "ready"},
		{name: "bye", in: "BYE logging out", status: "BYE", text: "logging out"},
		{name: "no text", in: "OK", status: "OK"},
		{name: "unknown condition", in: "MAYBE something", fails: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := dec(tc.in)
			var got RespCond
			ok := d.ExpectRespCond(&got)
			if tc.fails {
				if ok {
					t.Fatalf("expected failure, got %+v", got)
				}
				return
			}
			if !ok {
				t.Fatalf("ExpectRespCond: %v", d.Err())
			}
			if got.Status != tc.status || got.Text.Text != tc.text {
				t.Errorf("got %+v, want %q / %q", got, tc.status, tc.text)
			}
		})
	}
}

func TestBeginResponse(t *testing.T) {
	tests := []struct {
		name, in  string
		kind      ResponseKind
		tag       string
		wantErrIs error
	}{
		{name: "untagged", in: "* OK", kind: ResponseUntagged},
		{name: "tagged", in: "A001 OK", kind: ResponseTagged, tag: "A001"},
		{name: "continuation", in: "+ go ahead", kind: ResponseContinuation},
		{name: "clean EOF", in: "", wantErrIs: io.EOF},
		{name: "garbage", in: "\x00", wantErrIs: ErrSyntax},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := dec(tc.in)
			kind, tag, err := d.BeginResponse()
			if tc.wantErrIs != nil {
				if !errors.Is(err, tc.wantErrIs) {
					t.Fatalf("error = %v, want %v", err, tc.wantErrIs)
				}
				return
			}
			if err != nil {
				t.Fatalf("BeginResponse: %v", err)
			}
			if kind != tc.kind || tag != tc.tag {
				t.Errorf("got %v/%q, want %v/%q", kind, tag, tc.kind, tc.tag)
			}
		})
	}
}

func TestRealServerGreetings(t *testing.T) {
	for _, path := range []string{
		"testdata/greetings/dovecot.imap",
		"testdata/greetings/stalwart.imap",
	} {
		t.Run(path, func(t *testing.T) {
			wire, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			d := NewDecoder(bytes.NewReader(wire), nil)
			kind, tag, err := d.BeginResponse()
			if err != nil || kind != ResponseUntagged || tag != "" {
				t.Fatalf("BeginResponse() = %v, %q, %v", kind, tag, err)
			}
			if !d.ExpectSP() {
				t.Fatal(d.Err())
			}
			var cond RespCond
			if !d.ExpectRespCond(&cond) || !d.ExpectCRLF() {
				t.Fatal(d.Err())
			}
			if cond.Status != "OK" || cond.Text.Code != "CAPABILITY" {
				t.Fatalf("greeting = %+v", cond)
			}
			if !d.AtEOF() {
				t.Fatal("decoder did not stop at the response boundary")
			}
		})
	}
}

func TestContinuationText(t *testing.T) {
	for _, in := range []string{"+ go ahead\r\n", "+\r\n", "+ VGhlIGNoYWxsZW5nZQ==\r\n"} {
		d := dec(in)
		kind, _, err := d.BeginResponse()
		if err != nil || kind != ResponseContinuation {
			t.Fatalf("BeginResponse(%q) = %v, %v", in, kind, err)
		}
		var text string
		if !d.ExpectContinuationText(&text) {
			t.Fatalf("ExpectContinuationText(%q): %v", in, d.Err())
		}
	}
}

func TestDateTime(t *testing.T) {
	tests := []struct {
		name, in string
		want     time.Time
		fails    bool
	}{
		{
			name: "padded day",
			in:   `" 7-Jul-1996 02:44:25 -0700"`,
			want: time.Date(1996, time.July, 7, 2, 44, 25, 0, time.FixedZone("", -7*3600)),
		},
		{
			name: "two-digit day",
			in:   `"17-Jul-1996 02:44:25 -0700"`,
			want: time.Date(1996, time.July, 17, 2, 44, 25, 0, time.FixedZone("", -7*3600)),
		},
		{
			name: "UTC",
			in:   `"31-Dec-2024 23:59:59 +0000"`,
			want: time.Date(2024, time.December, 31, 23, 59, 59, 0, time.UTC),
		},
		{name: "unquoted", in: "17-Jul-1996 02:44:25 -0700", fails: true},
		{name: "no zone", in: `"17-Jul-1996 02:44:25"`, fails: true},
		{name: "bad month", in: `"17-Foo-1996 02:44:25 -0700"`, fails: true},
		{name: "impossible day", in: `"31-Feb-1996 02:44:25 -0700"`, fails: true},
		{name: "trailing junk", in: `"17-Jul-1996 02:44:25 -0700 x"`, fails: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := dec(tc.in)
			var got time.Time
			ok := d.ExpectDateTime(&got)
			if tc.fails {
				if ok {
					t.Fatalf("expected failure, got %v", got)
				}
				return
			}
			if !ok {
				t.Fatalf("ExpectDateTime: %v", d.Err())
			}
			if !got.Equal(tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDate(t *testing.T) {
	want := time.Date(1996, time.July, 17, 0, 0, 0, 0, time.UTC)
	for _, in := range []string{"17-Jul-1996", `"17-Jul-1996"`, "7-Jul-1996"} {
		d := dec(in)
		var got time.Time
		if !d.ExpectDate(&got) {
			t.Fatalf("ExpectDate(%q): %v", in, d.Err())
		}
		if in != "7-Jul-1996" && !got.Equal(want) {
			t.Errorf("ExpectDate(%q) = %v, want %v", in, got, want)
		}
	}
	d := dec("17-Jul-96")
	var got time.Time
	if d.ExpectDate(&got) {
		t.Error("a two-digit year must be rejected")
	}
}

func TestBodySection(t *testing.T) {
	tests := []struct {
		name, in string
		want     BodySection
		fails    bool
	}{
		{name: "whole message", in: "[]", want: BodySection{}},
		{name: "header", in: "[HEADER]", want: BodySection{Specifier: SpecifierHeader}},
		{name: "text", in: "[TEXT]", want: BodySection{Specifier: SpecifierText}},
		{name: "part", in: "[1.2]", want: BodySection{Part: []uint32{1, 2}}},
		{name: "part mime", in: "[1.2.MIME]", want: BodySection{Part: []uint32{1, 2}, Specifier: SpecifierMIME}},
		{name: "part text", in: "[4.TEXT]", want: BodySection{Part: []uint32{4}, Specifier: SpecifierText}},
		{
			name: "header fields",
			in:   "[HEADER.FIELDS (From To)]",
			want: BodySection{Specifier: SpecifierHeaderFields, Fields: []string{"From", "To"}},
		},
		{
			name: "header fields not",
			in:   `[HEADER.FIELDS.NOT ("Content-Type")]`,
			want: BodySection{Specifier: SpecifierHeaderFieldsNot, Fields: []string{"Content-Type"}},
		},
		{
			name: "partial with count",
			in:   "[]<0.1024>",
			want: BodySection{Partial: &SectionPartial{Offset: 0, Count: 1024}},
		},
		{
			name: "partial without count",
			in:   "[TEXT]<512>",
			want: BodySection{Specifier: SpecifierText, Partial: &SectionPartial{Offset: 512}},
		},
		{name: "unknown specifier kept", in: "[FUTURE]", want: BodySection{Specifier: "FUTURE"}},
		{name: "MIME without a part", in: "[MIME]", fails: true},
		{name: "zero part number", in: "[0]", fails: true},
		{name: "empty header list", in: "[HEADER.FIELDS ()]", fails: true},
		{name: "missing header list", in: "[HEADER.FIELDS]", fails: true},
		{name: "unterminated", in: "[TEXT", fails: true},
		{name: "zero partial count", in: "[]<0.0>", fails: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := dec(tc.in)
			var got BodySection
			ok := d.ExpectBodySection(&got)
			if tc.fails {
				if ok {
					t.Fatalf("expected failure, got %+v", got)
				}
				return
			}
			if !ok {
				t.Fatalf("ExpectBodySection: %v", d.Err())
			}
			if got.Specifier != tc.want.Specifier || len(got.Part) != len(tc.want.Part) ||
				strings.Join(got.Fields, "\x00") != strings.Join(tc.want.Fields, "\x00") {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestSectionPartDepthLimit(t *testing.T) {
	parts := make([]string, maxSectionPartDepth+1)
	for i := range parts {
		parts[i] = "1"
	}
	d := dec("[" + strings.Join(parts, ".") + "]")
	var got BodySection
	if d.ExpectBodySection(&got) {
		t.Fatal("an over-deep section-part must be rejected")
	}
}

func TestDiscardLine(t *testing.T) {
	tests := []struct {
		name, in, rest string
	}{
		{name: "plain", in: "* OK junk\r\n* NEXT\r\n", rest: "* NEXT\r\n"},
		{name: "with literal", in: "* X {5}\r\nab\r\nc)\r\n* NEXT\r\n", rest: "* NEXT\r\n"},
		{name: "brace in text", in: "* OK [ALERT] {not a literal}\r\n* NEXT\r\n", rest: "* NEXT\r\n"},
		{name: "CRLF in quoted is impossible", in: "* OK \"a b\"\r\n* NEXT\r\n", rest: "* NEXT\r\n"},
		{name: "bare LF", in: "* OK junk\n* NEXT\r\n", rest: "* NEXT\r\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := dec(tc.in)
			if err := d.DiscardLine(); err != nil {
				t.Fatalf("DiscardLine: %v", err)
			}
			if r := rest(t, d); r != tc.rest {
				t.Errorf("left %q, want %q", r, tc.rest)
			}
		})
	}
}

type exactChunkReader struct {
	chunk []byte
	read  bool
}

var errReadPastChunk = errors.New("read past supplied chunk")

func (r *exactChunkReader) Read(p []byte) (int, error) {
	if r.read {
		return 0, errReadPastChunk
	}
	r.read = true
	return copy(p, r.chunk), nil
}

func TestLiteralLookaheadDoesNotReadPastAnnouncement(t *testing.T) {
	d := NewDecoder(&exactChunkReader{chunk: []byte("{0}\r\n")}, nil)
	if !d.looksLikeLiteral() {
		t.Fatalf("literal announcement not recognised: %v", d.Err())
	}
	if d.Err() != nil {
		t.Fatalf("lookahead read beyond the announcement: %v", d.Err())
	}
}

func TestDiscardLineRecovers(t *testing.T) {
	d := dec("* 1 FETCH (X\x00Y)\r\n* 2 EXISTS\r\n")
	kind, _, err := d.BeginResponse()
	if err != nil || kind != ResponseUntagged {
		t.Fatalf("BeginResponse: %v", err)
	}
	var n uint32
	var name string
	d.ExpectSP()
	d.ExpectNumber(&n)
	d.ExpectSP()
	d.ExpectAtom(&name)
	d.ExpectSP()
	// The list contains a NUL, which no production accepts.
	err = d.ExpectList(func() error {
		var s string
		if !d.ExpectAtom(&s) {
			return d.Err()
		}
		return nil
	})
	if err == nil {
		t.Fatal("expected the NUL to be rejected")
	}
	if IsFatal(err) {
		t.Fatalf("a self-delimiting syntax error must stay recoverable: %v", err)
	}
	if err := d.DiscardLine(); err != nil {
		t.Fatalf("DiscardLine: %v", err)
	}
	kind, _, err = d.BeginResponse()
	if err != nil || kind != ResponseUntagged {
		t.Fatalf("after recovery: %v, %v", kind, err)
	}
}

func TestDiscardLineRecoversFromMalformedQuoted(t *testing.T) {
	for _, tc := range []struct{ bad, input string }{
		{bad: `"bad\q"`, input: "* X \"bad\\q\"\r\n* NEXT\r\n"},
		{bad: "NUL", input: "* X \"bad\x00value\"\r\n* NEXT\r\n"},
		{bad: "line break", input: "* X \"bad\r\n* NEXT\r\n"},
	} {
		d := dec(tc.input)
		kind, _, err := d.BeginResponse()
		if err != nil || kind != ResponseUntagged || !d.ExpectSP() {
			t.Fatalf("BeginResponse(%q): %v", tc.bad, d.Err())
		}
		var atom, quoted string
		if !d.ExpectAtom(&atom) || !d.ExpectSP() {
			t.Fatalf("prefix(%q): %v", tc.bad, d.Err())
		}
		if d.ExpectQuoted(&quoted) {
			t.Fatalf("malformed quoted string %q was accepted", tc.bad)
		}
		if IsFatal(d.Err()) {
			t.Fatalf("malformed quoted string became fatal: %v", d.Err())
		}
		if err := d.DiscardLine(); err != nil {
			t.Fatalf("DiscardLine(%q): %v", tc.bad, err)
		}
		kind, _, err = d.BeginResponse()
		if err != nil || kind != ResponseUntagged {
			t.Fatalf("after recovery from %q: %v, %v", tc.bad, kind, err)
		}
	}
}

func TestDiscardLineRefusesAfterFatal(t *testing.T) {
	d := dec("{4294967295}\r\n* NEXT\r\n")
	if _, ok := d.Literal(); ok {
		t.Fatal("expected the literal to be rejected")
	}
	if err := d.DiscardLine(); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("DiscardLine after a fatal error = %v, want it to stay fatal", err)
	}
}

func TestDiscardValue(t *testing.T) {
	tests := []struct{ name, in, rest string }{
		{name: "atom", in: "ATOM rest", rest: " rest"},
		{name: "quoted", in: `"a b" rest`, rest: " rest"},
		{name: "literal", in: "{3}\r\nabc rest", rest: " rest"},
		{name: "list", in: "(A (B {1}\r\nx) C) rest", rest: " rest"},
		{name: "nil", in: "NIL rest", rest: " rest"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := dec(tc.in)
			if err := d.DiscardValue(); err != nil {
				t.Fatalf("DiscardValue: %v", err)
			}
			if r := rest(t, d); r != tc.rest {
				t.Errorf("left %q, want %q", r, tc.rest)
			}
		})
	}
}

// stallingConn is a reader that blocks forever and can have a deadline set on
// it, like a network connection whose peer has gone quiet.
type stallingConn struct {
	deadline chan struct{}
}

func (c *stallingConn) Read(p []byte) (int, error) {
	<-c.deadline
	return 0, errStalled
}

func (c *stallingConn) SetReadDeadline(t time.Time) error {
	go func() {
		time.Sleep(time.Until(t))
		select {
		case <-c.deadline:
		default:
			close(c.deadline)
		}
	}()
	return nil
}

var errStalled = errors.New("i/o timeout")

func TestReadDeadline(t *testing.T) {
	c := &stallingConn{deadline: make(chan struct{})}
	d := NewDecoder(c, &Options{ReadTimeout: 10 * time.Millisecond})
	done := make(chan struct{})
	go func() {
		defer close(done)
		var s string
		d.Atom(&s)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a stalled read did not observe the deadline")
	}
	if !errors.Is(d.Err(), errStalled) {
		t.Fatalf("error = %v, want the timeout", d.Err())
	}
}

func TestNoPanicOnTruncatedInput(t *testing.T) {
	// Every prefix of a realistic response must fail cleanly rather than panic.
	const full = "* 12 FETCH (FLAGS (\\Seen) INTERNALDATE \"17-Jul-1996 02:44:25 -0700\" " +
		"BODY[HEADER.FIELDS (From)] {5}\r\nhello)\r\n"
	for i := 0; i <= len(full); i++ {
		d := dec(full[:i])
		consumeResponse(d)
	}
}

// consumeResponse exercises the response framing and recovery path used by a
// client without assigning semantics to the response payload. It deliberately
// ignores errors: callers use it to assert that arbitrary/truncated wire input
// terminates without panicking.
func consumeResponse(d *Decoder) {
	if _, _, err := d.BeginResponse(); err != nil {
		return
	}
	_ = d.DiscardLine()
}
