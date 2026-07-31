package saslprep

import (
	"testing"
	"unicode/utf8"
)

// FuzzPrepare asserts the properties that must hold for every input, valid
// UTF-8 or not, since SASL credentials reach this code directly from the
// caller and a password is exactly the kind of value that carries
// unpaired surrogates, truncated sequences, lone combining marks and
// control characters.
//
//  1. Prepare never panics.
//  2. When Prepare(s) succeeds, its output is well-formed UTF-8.
//  3. When Prepare(s) succeeds, it is idempotent:
//     Prepare(Prepare(s)) == Prepare(s). This is the property that makes
//     it safe to apply SASLprep more than once along a call path, and the
//     one a mapping, normalization or prohibition-table bug tends to
//     break.
//
// PrepareStored is exercised identically: it shares its entire
// implementation with Prepare except for one extra table check (see
// prepare in saslprep.go), so any input-dependent panic or idempotence
// break reachable through one is reachable through the other.
func FuzzPrepare(f *testing.F) {
	f.Add("")
	f.Add("user")
	f.Add("USER")
	f.Add("I­X")                // SOFT HYPHEN, mapped to nothing
	f.Add("ª")                  // FEMININE ORDINAL INDICATOR -> NFKC "a"
	f.Add("Ⅸ")                  // ROMAN NUMERAL NINE -> NFKC "IX"
	f.Add("")                  // BELL: prohibited ASCII control character
	f.Add("ا1")                 // bidi violation: RandALCat then non-RandALCat
	f.Add("اب")                 // bidi OK: RandALCat only, both ends RandALCat
	f.Add("a b")                // NO-BREAK SPACE, mapped to SPACE
	f.Add("µ")                  // MICRO SIGN -> NFKC GREEK SMALL LETTER MU
	f.Add("ȡ")                  // unassigned in Unicode 3.2 (Table A.1)
	f.Add("café")               // precomposed e-acute
	f.Add("café")              // decomposed e + combining acute
	f.Add("\xff\xfe")           // invalid UTF-8
	f.Add("\xed\xa0\x80")       // unpaired surrogate (encoded)
	f.Add(string(rune(0xD800))) // unpaired surrogate (rune conversion)
	f.Add("�")                  // REPLACEMENT CHARACTER: prohibited (Table C.6)
	f.Add("​")                  // ZERO WIDTH SPACE: mapped to nothing (Table B.1)
	f.Add("")                  // private use (Table C.3): prohibited
	f.Add("\U0001D160")         // outside the BMP
	f.Add("　")                  // IDEOGRAPHIC SPACE: mapped to SPACE (Table C.1.2)

	f.Fuzz(func(t *testing.T, s string) {
		got, err := Prepare(s)
		if err != nil {
			return
		}
		if !utf8.ValidString(got) {
			t.Fatalf("Prepare succeeded but returned invalid UTF-8: %q", got)
		}
		twice, err := Prepare(got)
		if err != nil {
			t.Fatalf("Prepare(s) succeeded but Prepare(Prepare(s)) failed: %v", err)
		}
		if twice != got {
			t.Fatalf("Prepare is not idempotent:\n  once  = %q (% x)\n  twice = %q (% x)", got, []rune(got), twice, []rune(twice))
		}

		// PrepareStored shares its implementation with Prepare (see
		// prepare in saslprep.go); when it also succeeds on this input,
		// it must satisfy the same two properties.
		gotStored, err := PrepareStored(s)
		if err != nil {
			return
		}
		if !utf8.ValidString(gotStored) {
			t.Fatalf("PrepareStored succeeded but returned invalid UTF-8: %q", gotStored)
		}
		twiceStored, err := PrepareStored(gotStored)
		if err != nil {
			t.Fatalf("PrepareStored(s) succeeded but PrepareStored(PrepareStored(s)) failed: %v", err)
		}
		if twiceStored != gotStored {
			t.Fatalf("PrepareStored is not idempotent:\n  once  = %q (% x)\n  twice = %q (% x)", gotStored, []rune(gotStored), twiceStored, []rune(twiceStored))
		}
	})
}
