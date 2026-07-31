package unicodenorm

import (
	"testing"
	"unicode/utf8"
)

// FuzzNFC asserts the three properties that hold for every input, valid UTF-8
// or not. Credentials reach this code from the caller, and a password is
// exactly the kind of value that carries unpaired surrogates, truncated
// sequences and lone combining marks, so "must not panic" is a real
// requirement rather than a formality.
//
//  1. NFC never panics.
//  2. NFC is idempotent — NFC(NFC(s)) == NFC(s). This is the property that
//     makes normalisation safe to apply more than once along a call path, and
//     it is the one a table or blocking-rule bug tends to break.
//  3. NFC returns well-formed UTF-8, since invalid input is replaced with
//     U+FFFD rather than passed through.
func FuzzNFC(f *testing.F) {
	f.Add("")
	f.Add("hello")
	f.Add("café")
	f.Add("cafe\u0301")
	f.Add("\uAC00")
	f.Add("\u1100\u1161")
	f.Add("A\u0301\u0308")
	f.Add("\u0301")              // leading combining mark, no starter
	f.Add("\xff\xfe")            // invalid UTF-8
	f.Add("\xed\xa0\x80")        // unpaired surrogate
	f.Add("e\u0301\u0301\u0301") // repeated marks
	f.Add("\U0001D160")          // outside the BMP
	f.Add("\u1E14")              // multi-level canonical decomposition
	f.Add("\u0F77")              // singleton with a compatibility mapping

	f.Fuzz(func(t *testing.T, s string) {
		got := NFC(s)

		if !utf8.ValidString(got) {
			t.Fatalf("NFC(%q) returned invalid UTF-8: %q", s, got)
		}
		if twice := NFC(got); twice != got {
			t.Fatalf("NFC is not idempotent for %q:\n  once  = %q (% x)\n  twice = %q (% x)",
				s, got, []rune(got), twice, []rune(twice))
		}
	})
}

// FuzzNFKC mirrors FuzzNFC, asserting the same three properties for NFKC:
// it never panics, it is idempotent, and it returns well-formed UTF-8. Like
// NFC, NFKC sits on the SASLprep (RFC 4013 / RFC 5802) path, so it must be
// safe on arbitrary, possibly hostile or malformed, credential input.
func FuzzNFKC(f *testing.F) {
	f.Add("")
	f.Add("hello")
	f.Add("café")
	f.Add("cafe\u0301")
	f.Add("\uAC00")
	f.Add("\u1100\u1161")
	f.Add("A\u0301\u0308")
	f.Add("\u0301")              // leading combining mark, no starter
	f.Add("\xff\xfe")            // invalid UTF-8
	f.Add("\xed\xa0\x80")        // unpaired surrogate
	f.Add("e\u0301\u0301\u0301") // repeated marks
	f.Add("\U0001D160")          // outside the BMP
	f.Add("\u1E14")              // multi-level canonical decomposition
	f.Add("\u0F77")              // singleton with a compatibility mapping
	f.Add("\u1E9B\u0323")        // compat decomposition then canonical reordering/composition
	f.Add("\uFB01")              // ligature "fi"
	f.Add("\u3131")              // Hangul compatibility jamo
	f.Add("\uFF21")              // fullwidth Latin A
	f.Add("\u2126")              // Ohm sign -> Greek capital omega
	f.Add("½")                   // vulgar fraction one half
	f.Add("²")                   // superscript two

	f.Fuzz(func(t *testing.T, s string) {
		got := NFKC(s)

		if !utf8.ValidString(got) {
			t.Fatalf("NFKC(%q) returned invalid UTF-8: %q", s, got)
		}
		if twice := NFKC(got); twice != got {
			t.Fatalf("NFKC is not idempotent for %q:\n  once  = %q (% x)\n  twice = %q (% x)",
				s, got, []rune(got), twice, []rune(twice))
		}
	})
}
