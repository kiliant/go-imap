package imapwire

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func encodeWith(t *testing.T, opts *EncoderOptions, f func(*Encoder)) (string, error) {
	t.Helper()
	var out bytes.Buffer
	e := NewEncoder(&out, opts)
	f(e)
	err := e.Flush()
	return out.String(), err
}

func TestEncoderAstringChoosesMinimalForm(t *testing.T) {
	tests := []struct {
		name, value, want string
	}{
		{name: "atom", value: "INBOX", want: "INBOX"},
		{name: "empty", value: "", want: `""`},
		{name: "NIL is quoted", value: "NIL", want: `"NIL"`},
		{name: "space is quoted", value: "two words", want: `"two words"`},
		{name: "quoted specials escaped", value: `a"b\c`, want: `"a\"b\\c"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := encodeWith(t, nil, func(e *Encoder) { e.Astring(tc.value) })
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("encoded %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEncoderNeverQuotesEightBitOrNUL(t *testing.T) {
	got, err := encodeWith(t, &EncoderOptions{LiteralPlus: true}, func(e *Encoder) {
		e.Astring("café")
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "{5+}\r\ncafé" {
		t.Fatalf("encoded as %q", got)
	}

	_, err = encodeWith(t, &EncoderOptions{LiteralPlus: true}, func(e *Encoder) {
		e.String("a\x00b")
	})
	if err == nil {
		t.Fatal("a non-binary literal accepted NUL")
	}

	got, err = encodeWith(t, &EncoderOptions{LiteralPlus: true}, func(e *Encoder) {
		e.Literal8("a\x00b")
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "~{3+}\r\na\x00b" {
		t.Fatalf("literal8 encoded as %q", got)
	}
}

func TestEncoderResponseStringKeepsLineBounded(t *testing.T) {
	value := strings.Repeat("a", DefaultMaxLineLength)
	got, err := encodeWith(t, &EncoderOptions{ServerResponse: true}, func(e *Encoder) {
		e.Special('(').String(value).Special(')')
	})
	if err != nil {
		t.Fatal(err)
	}
	wantPrefix := "({8192}\r\n"
	if !strings.HasPrefix(got, wantPrefix) || !strings.HasSuffix(got, ")") {
		t.Fatalf("long response string was not literalised: prefix %q", got[:min(len(got), len(wantPrefix))])
	}

	dec := NewDecoderString(got, nil)
	if !dec.ExpectSpecial('(') {
		t.Fatal(dec.Err())
	}
	var decoded string
	if !dec.ExpectString(&decoded) || decoded != value || !dec.ExpectSpecial(')') || !dec.AtEOF() {
		t.Fatalf("round trip failed: %v", dec.Err())
	}
}

func TestEncoderAtomAndTagValidation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		write func(*Encoder)
	}{
		{name: "empty atom", write: func(e *Encoder) { e.Atom("") }},
		{name: "atom resp special", write: func(e *Encoder) { e.Atom("BAD]") }},
		{name: "atom wildcard", write: func(e *Encoder) { e.Atom("A*") }},
		{name: "8-bit atom", write: func(e *Encoder) { e.Atom("café") }},
		{name: "plus in tag", write: func(e *Encoder) { e.Tag("A+1") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := encodeWith(t, nil, tc.write); err == nil {
				t.Fatal("invalid value was accepted")
			}
		})
	}
}

func TestEncoderSynchronisingLiteralWaits(t *testing.T) {
	var out bytes.Buffer
	waits := 0
	e := NewEncoder(&out, &EncoderOptions{WaitContinuation: func() error {
		waits++
		if got := out.String(); got != "{5}\r\n" {
			t.Fatalf("continuation requested after writing %q", got)
		}
		return nil
	}})
	lw, err := e.Literal(5, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(lw, "hello"); err != nil {
		t.Fatal(err)
	}
	if err := lw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := e.Flush(); err != nil {
		t.Fatal(err)
	}
	if waits != 1 || out.String() != "{5}\r\nhello" {
		t.Fatalf("waits=%d output=%q", waits, out.String())
	}
}

func TestEncoderLiteralCapabilityForms(t *testing.T) {
	tests := []struct {
		name   string
		size   int64
		binary bool
		opts   EncoderOptions
		want   string
	}{
		{name: "literal plus", size: 2, opts: EncoderOptions{LiteralPlus: true}, want: "{2+}\r\nxx"},
		{name: "literal plus binary", size: 2, binary: true, opts: EncoderOptions{LiteralPlus: true}, want: "~{2+}\r\nxx"},
		{name: "literal minus boundary", size: 4096, opts: EncoderOptions{LiteralMinus: true}, want: "{4096+}\r\n" + strings.Repeat("x", 4096)},
		{name: "literal minus over boundary synchronises", size: 4097, opts: EncoderOptions{LiteralMinus: true, WaitContinuation: func() error { return nil }}, want: "{4097}\r\n" + strings.Repeat("x", 4097)},
		{name: "literal minus binary synchronises", size: 2, binary: true, opts: EncoderOptions{LiteralMinus: true, WaitContinuation: func() error { return nil }}, want: "~{2}\r\nxx"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := encodeWith(t, &tc.opts, func(e *Encoder) {
				lw, err := e.Literal(tc.size, tc.binary)
				if err != nil {
					return
				}
				_, _ = io.CopyN(lw, repeatedByte('x'), tc.size)
				_ = lw.Close()
			})
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("encoded %d-byte literal incorrectly", tc.size)
			}
		})
	}
}

func TestEncoderLiteralWriterInterlock(t *testing.T) {
	var out bytes.Buffer
	e := NewEncoder(&out, &EncoderOptions{LiteralPlus: true})
	lw, err := e.Literal(3, false)
	if err != nil {
		t.Fatal(err)
	}
	e.Atom("NEXT")
	if e.Err() == nil {
		t.Fatal("encoder advanced past an incomplete literal")
	}
	if _, err := lw.Write([]byte("toolong")); err == nil {
		t.Fatal("literal writer accepted excess data")
	}

	e = NewEncoder(io.Discard, &EncoderOptions{LiteralPlus: true})
	lw, err = e.Literal(3, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lw.Write([]byte("xy")); err != nil {
		t.Fatal(err)
	}
	if err := lw.Close(); err == nil {
		t.Fatal("short literal was accepted")
	}
}

func TestEncoderContinuationFailureIsFatal(t *testing.T) {
	want := errors.New("command rejected")
	e := NewEncoder(io.Discard, &EncoderOptions{WaitContinuation: func() error { return want }})
	if _, err := e.Literal(1, false); !errors.Is(err, want) || !IsFatal(err) {
		t.Fatalf("error = %v, want fatal continuation error", err)
	}
}

func TestEncoderMailboxDateAndSection(t *testing.T) {
	tm := time.Date(1996, time.July, 7, 2, 44, 25, 0, time.FixedZone("", -7*60*60))
	section := &BodySection{
		Part:      []uint32{1, 2},
		Specifier: SpecifierHeaderFieldsNot,
		Fields:    []string{"From", "Content-Type"},
		Partial:   &SectionPartial{Offset: 0, Count: 1024},
	}
	got, err := encodeWith(t, nil, func(e *Encoder) {
		e.Mailbox("台北").SP().DateTime(tm).SP().Date(tm).SP().BodySection(section)
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `&U,BTFw- " 7-Jul-1996 02:44:25 -0700" 7-Jul-1996 [1.2.HEADER.FIELDS.NOT (From Content-Type)]<0.1024>`
	if got != want {
		t.Fatalf("encoded %q, want %q", got, want)
	}
}

func TestEncoderUTF8Mailbox(t *testing.T) {
	got, err := encodeWith(t, &EncoderOptions{UTF8Accept: true, LiteralPlus: true}, func(e *Encoder) {
		e.Mailbox("台北")
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "{6+}\r\n台北" {
		t.Fatalf("encoded %q", got)
	}
	if _, err := encodeWith(t, &EncoderOptions{UTF8Accept: true, LiteralPlus: true}, func(e *Encoder) {
		e.Mailbox("\xff")
	}); err == nil {
		t.Fatal("invalid UTF-8 mailbox was accepted")
	}
}

func TestEncoderRejectsInvalidDateAndSection(t *testing.T) {
	tooManyParts := make([]uint32, maxSectionPartDepth+1)
	for i := range tooManyParts {
		tooManyParts[i] = 1
	}
	for _, tc := range []struct {
		name  string
		write func(*Encoder)
	}{
		{name: "date year", write: func(e *Encoder) { e.Date(time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)) }},
		{name: "date-time year", write: func(e *Encoder) { e.DateTime(time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)) }},
		{name: "nil section", write: func(e *Encoder) { e.BodySection(nil) }},
		{name: "zero part", write: func(e *Encoder) { e.BodySection(&BodySection{Part: []uint32{0}}) }},
		{name: "too many parts", write: func(e *Encoder) { e.BodySection(&BodySection{Part: tooManyParts}) }},
		{name: "MIME without part", write: func(e *Encoder) { e.BodySection(&BodySection{Specifier: SpecifierMIME}) }},
		{name: "empty header fields", write: func(e *Encoder) { e.BodySection(&BodySection{Specifier: SpecifierHeaderFields}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := encodeWith(t, nil, tc.write); err == nil {
				t.Fatal("invalid value was accepted")
			}
		})
	}
}
