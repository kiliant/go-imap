package saslprep

import (
	"strings"
	"testing"

	"github.com/kiliant/go-imap/internal/unicodenorm"
)

// TestRFC4013Examples runs the worked examples from RFC 4013 Section 3
// verbatim. This is the load-bearing conformance test for this package: if
// any of these fail, the implementation does not conform to RFC 4013,
// regardless of what any other test says.
//
// Every non-ASCII or otherwise hard-to-eyeball code point is written as an
// explicit \uXXXX escape rather than a literal character, so the intended
// code point is unambiguous from the source text itself.
func TestRFC4013Examples(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{
			name: "SOFT HYPHEN (U+00AD) mapped to nothing",
			in:   "I\u00adX",
			want: "IX",
		},
		{
			name: "no transformation",
			in:   "user",
			want: "user",
		},
		{
			name: "case preserved, not case-folded",
			in:   "USER",
			want: "USER",
		},
		{
			name: "FEMININE ORDINAL INDICATOR (U+00AA) maps via NFKC to 'a'",
			in:   "ª",
			want: "a",
		},
		{
			name: "ROMAN NUMERAL NINE (U+2168) maps via NFKC to 'IX'",
			in:   "Ⅸ",
			want: "IX",
		},
		{
			name:    "prohibited character: BELL (U+0007), ASCII control",
			in:      "\a",
			wantErr: true,
		},
		{
			name:    "bidi check violation: RandALCat (U+0627 ARABIC LETTER ALEF) followed by a non-RandALCat last character",
			in:      "ا1",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Prepare(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Prepare(%q) = %q, <nil>; want an error", tc.in, got)
				}
				t.Logf("Prepare(%q) correctly failed: %v", tc.in, err)
				return
			}
			if err != nil {
				t.Fatalf("Prepare(%q) unexpected error: %v", tc.in, err)
			}
			t.Logf("Prepare(%q) = %q (% x), want %q (% x)", tc.in, got, []rune(got), tc.want, []rune(tc.want))
			if got != tc.want {
				t.Fatalf("Prepare(%q) = %q (% x), want %q (% x)", tc.in, got, []rune(got), tc.want, []rune(tc.want))
			}
		})
	}
}

// TestNonASCIISpaceMapping checks the Map step's Table C.1.2 handling:
// non-ASCII space characters become U+0020 SPACE, distinct from Table B.1
// (mapped to nothing).
func TestNonASCIISpaceMapping(t *testing.T) {
	in := "a b" // U+00A0 NO-BREAK SPACE
	got, err := Prepare(in)
	if err != nil {
		t.Fatalf("Prepare(%q): unexpected error: %v", in, err)
	}
	if want := "a b"; got != want {
		t.Fatalf("Prepare(%q) = %q, want %q", in, got, want)
	}
}

// TestMicroSign checks the classic SASLprep/NFKC compatibility-normalization
// example: MICRO SIGN (U+00B5) normalizes to GREEK SMALL LETTER MU
// (U+03BC), not the reverse and not a no-op.
func TestMicroSign(t *testing.T) {
	got, err := Prepare("µ")
	if err != nil {
		t.Fatalf("Prepare: unexpected error: %v", err)
	}
	if want := "μ"; got != want {
		t.Fatalf("Prepare(micro sign) = %q (U+%04X), want %q (U+%04X)", got, []rune(got)[0], want, []rune(want)[0])
	}
}

// TestPrepareVsPrepareStored checks the one behavioral difference RFC 4013
// draws between the two entry points: an unassigned (in Unicode 3.2) code
// point is permitted by Prepare (queries) but rejected by PrepareStored.
func TestPrepareVsPrepareStored(t *testing.T) {
	// U+0221 is listed in RFC 3454 Table A.1 (unassigned in Unicode 3.2)
	// and remains unassigned in every later Unicode version used here, so
	// NFKC leaves it untouched and only the unassigned-code-point check
	// distinguishes the two functions.
	const unassigned = "ȡ"

	got, err := Prepare(unassigned)
	if err != nil {
		t.Fatalf("Prepare(unassigned code point) unexpected error: %v", err)
	}
	if got != unassigned {
		t.Fatalf("Prepare(unassigned code point) = %q, want unchanged %q", got, unassigned)
	}

	if _, err := PrepareStored(unassigned); err == nil {
		t.Fatalf("PrepareStored(unassigned code point) = <nil> error, want an error")
	} else {
		t.Logf("PrepareStored correctly rejected unassigned code point: %v", err)
	}
}

// TestErrorsDoNotLeakInput guards the hard requirement that error messages
// from this package never embed the credential being prepared, since s is,
// on this code path, a SASL user name or password. Each case is
// constructed to fail for a different reason (prohibited character, bidi
// violation, invalid UTF-8 replaced with a prohibited U+FFFD) and tagged
// with a distinctive marker string, so an accidental "helpful"
// fmt.Errorf("...: %q", s) added later is very likely to be caught.
func TestErrorsDoNotLeakInput(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		marker string
	}{
		{"prohibited control character", "hunter2MARKERONE\a", "MARKERONE"},
		{"bidi violation", "اMARKERTWO1", "MARKERTWO"},
		{"invalid UTF-8 becomes prohibited U+FFFD", "correcthorsebatterystapleMARKERTHREE" + string(rune(0xD800)), "MARKERTHREE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Prepare(tc.in)
			if err == nil {
				t.Fatalf("Prepare(%q) unexpectedly succeeded; test case no longer exercises an error path", tc.name)
			}
			if strings.Contains(err.Error(), tc.marker) {
				t.Fatalf("error message leaks input: %v", err)
			}
		})
	}

	if _, err := PrepareStored("ȡSTOREDMARKER"); err == nil {
		t.Fatal("expected PrepareStored to reject unassigned code point")
	} else if strings.Contains(err.Error(), "STOREDMARKER") {
		t.Fatalf("error message leaks input: %v", err)
	}
}

// TestIdempotent checks that Prepare and PrepareStored are idempotent over
// their own successful output, which is required for it to be safe to
// apply SASLprep more than once along a call path (e.g. once client-side
// and, redundantly, again in a shared helper).
func TestIdempotent(t *testing.T) {
	inputs := []string{
		"",
		"user",
		"USER",
		"I\u00adX",
		"ª",
		"Ⅸ",
		"a b",
		"µ",
		"café",
		"اب", // two RandALCat characters: bidi check passes
	}
	for _, in := range inputs {
		once, err := Prepare(in)
		if err != nil {
			t.Fatalf("Prepare(%q) unexpected error: %v", in, err)
		}
		twice, err := Prepare(once)
		if err != nil {
			t.Fatalf("Prepare(Prepare(%q)) unexpected error: %v", in, err)
		}
		if once != twice {
			t.Errorf("Prepare not idempotent for %q: once=%q (% x), twice=%q (% x)", in, once, []rune(once), twice, []rune(twice))
		}

		onceStored, err := PrepareStored(in)
		if err != nil {
			t.Fatalf("PrepareStored(%q) unexpected error: %v", in, err)
		}
		twiceStored, err := PrepareStored(onceStored)
		if err != nil {
			t.Fatalf("PrepareStored(PrepareStored(%q)) unexpected error: %v", in, err)
		}
		if onceStored != twiceStored {
			t.Errorf("PrepareStored not idempotent for %q: once=%q (% x), twice=%q (% x)", in, onceStored, []rune(onceStored), twiceStored, []rune(twiceStored))
		}
	}
}

// TestBidiLCatOnlyUnaffected checks the RandALCat-absent branch of the
// bidi check: a string with only LCat characters (Table D.2), and no
// RandALCat characters at all, is unaffected by RFC 3454 Section 6.
func TestBidiLCatOnlyUnaffected(t *testing.T) {
	got, err := Prepare("abc123")
	if err != nil {
		t.Fatalf("Prepare: unexpected error: %v", err)
	}
	if got != "abc123" {
		t.Fatalf("Prepare(%q) = %q, want unchanged", "abc123", got)
	}
}

// TestBidiRandALCatOnlyBothEnds checks that a string composed entirely of
// RandALCat characters (satisfying "first and last are RandALCat" and
// containing no LCat character) passes the bidi check unchanged.
func TestBidiRandALCatOnlyBothEnds(t *testing.T) {
	// Two Arabic letters (ALEF U+0627, BEH U+0628), both in Table D.1,
	// neither in Table D.2.
	in := "اب"
	got, err := Prepare(in)
	if err != nil {
		t.Fatalf("Prepare(%q) unexpected error: %v", in, err)
	}
	if got != in {
		t.Fatalf("Prepare(%q) = %q, want unchanged", in, got)
	}
}

// TestUnicodeVersionAssumption documents and guards the deliberate version
// skew described in the package doc comment: table A.1 (and the rest of
// RFC 3454's tables) are frozen to Unicode 3.2 by design and must never be
// regenerated against a newer version, while NFKC tracks whatever version
// internal/unicodenorm was generated from. This test does not assert
// unicode.Version directly (internal/unicodenorm already guards that); it
// exists so a reader auditing this package's tests finds a pointer back to
// the package doc comment's explanation.
func TestUnicodeVersionAssumption(t *testing.T) {
	// U+00AA is assigned in Unicode 3.2 (so it's absent from Table A.1)
	// and has an NFKC mapping to "a" under Unicode 15.0.0 (see
	// TestRFC4013Examples): both facts must hold simultaneously for
	// SASLprep to behave as RFC 4013 documents, which is only possible
	// because of the deliberate version split documented in this
	// package's doc comment.
	if got := unicodenorm.NFKC("ª"); got != "a" {
		t.Fatalf("unicodenorm.NFKC(U+00AA) = %q, want %q; internal/unicodenorm may have been regenerated against an incompatible Unicode version", got, "a")
	}
	if inTable(tableA1[:], 'ª') {
		t.Fatalf("U+00AA unexpectedly present in Table A.1 (unassigned in Unicode 3.2); internal/saslprep/tables.go may be corrupted or regenerated incorrectly")
	}
}

// TestTableInvariants asserts the structural invariant inTable's binary
// search depends on for every generated table: entries sorted ascending
// by lo, disjoint (not even touching -- a touching pair should have been
// merged by the generator's normalize step), lo <= hi within each entry,
// and every code point within the valid Unicode range. A single
// mis-sorted or overlapping table would make inTable silently return
// wrong answers for some inputs while every other test in this package
// (the RFC examples, the leak test, and the fuzzer's idempotence/UTF-8
// checks) could still pass, since none of them cross-check the table data
// itself against this invariant.
func TestTableInvariants(t *testing.T) {
	const maxCodePoint = 0x10FFFF

	tables := []struct {
		name string
		t    []interval
	}{
		{"A.1", tableA1[:]},
		{"B.1", tableB1[:]},
		{"C.1.2", tableC12[:]},
		{"C.2.1", tableC21[:]},
		{"C.2.2", tableC22[:]},
		{"C.3", tableC3[:]},
		{"C.4", tableC4[:]},
		{"C.5", tableC5[:]},
		{"C.6", tableC6[:]},
		{"C.7", tableC7[:]},
		{"C.8", tableC8[:]},
		{"C.9", tableC9[:]},
		{"D.1", tableD1[:]},
		{"D.2", tableD2[:]},
	}

	for _, tc := range tables {
		t.Run(tc.name, func(t *testing.T) {
			if len(tc.t) == 0 {
				t.Fatalf("table %s is empty", tc.name)
			}
			for i, iv := range tc.t {
				if iv.lo > iv.hi {
					t.Fatalf("table %s entry %d: lo (0x%X) > hi (0x%X)", tc.name, i, iv.lo, iv.hi)
				}
				if iv.lo < 0 || iv.hi > maxCodePoint {
					t.Fatalf("table %s entry %d: [0x%X, 0x%X] outside valid code point range", tc.name, i, iv.lo, iv.hi)
				}
				if i > 0 && tc.t[i-1].hi >= iv.lo {
					t.Fatalf("table %s entries %d and %d are not disjoint/ascending: [0x%X, 0x%X] then [0x%X, 0x%X]",
						tc.name, i-1, i, tc.t[i-1].lo, tc.t[i-1].hi, iv.lo, iv.hi)
				}
				// The generator's normalize step merges touching ranges
				// (hi+1 == next.lo), so a committed table should never
				// contain adjacent-but-unmerged entries either.
				if i > 0 && tc.t[i-1].hi+1 == iv.lo {
					t.Fatalf("table %s entries %d and %d touch but were not merged: [0x%X, 0x%X] then [0x%X, 0x%X]",
						tc.name, i-1, i, tc.t[i-1].lo, tc.t[i-1].hi, iv.lo, iv.hi)
				}
			}
		})
	}
}

// TestTableAnchorMembership spot-checks specific, independently-known code
// points against the large tables (A.1: 396 ranges, D.2: 360 ranges after
// generation) that TestRFC4013Examples and the other tests never exercise
// directly, plus a couple of smaller tables. This catches a wholesale
// table mix-up, truncation, or off-by-one in the generator that the
// structural invariant in TestTableInvariants cannot: that test only
// checks the tables are well-formed, not that they contain the right code
// points.
func TestTableAnchorMembership(t *testing.T) {
	cases := []struct {
		table    []interval
		name     string
		r        rune
		inTable  bool
		codeName string
	}{
		// Table D.2 (LCat): ordinary Latin letters and the two special
		// cases RFC 3454 calls out explicitly in its own D.2 commentary.
		{tableD2[:], "D.2", 'A', true, "LATIN CAPITAL LETTER A"},
		{tableD2[:], "D.2", 'Z', true, "LATIN CAPITAL LETTER Z"},
		{tableD2[:], "D.2", 'a', true, "LATIN SMALL LETTER A"},
		{tableD2[:], "D.2", 'z', true, "LATIN SMALL LETTER Z"},
		{tableD2[:], "D.2", 0x00B5, true, "MICRO SIGN"},
		{tableD2[:], "D.2", 0x0627, false, "ARABIC LETTER ALEF (must be D.1, not D.2)"},

		// Table D.1 (RandALCat): Hebrew, Arabic, and Hebrew presentation
		// forms, spread across the table's three source ranges.
		{tableD1[:], "D.1", 0x05D0, true, "HEBREW LETTER ALEF"},
		{tableD1[:], "D.1", 0x0627, true, "ARABIC LETTER ALEF"},
		{tableD1[:], "D.1", 0xFB1D, true, "HEBREW LETTER YOD WITH HIRIQ"},
		{tableD1[:], "D.1", 'A', false, "LATIN CAPITAL LETTER A (must be D.2, not D.1)"},

		// Table A.1 (unassigned in Unicode 3.2): the RFC's own first
		// listed entry, plus one from deep in the table (near the end of
		// the BMP) and a sanity negative.
		{tableA1[:], "A.1", 0x0221, true, "first A.1 entry per RFC 3454 text"},
		{tableA1[:], "A.1", 0x0FFF, true, "within the FD0-FFF A.1 range"},
		{tableA1[:], "A.1", 'A', false, "LATIN CAPITAL LETTER A (assigned since long before Unicode 3.2)"},

		// Table C.3 (private use): all three private-use planes.
		{tableC3[:], "C.3", 0xE000, true, "start of Plane 0 private use"},
		{tableC3[:], "C.3", 0xF8FF, true, "end of Plane 0 private use"},
		{tableC3[:], "C.3", 0x10FFFD, true, "end of Plane 16 private use"},
		{tableC3[:], "C.3", 0xF900, false, "CJK COMPATIBILITY IDEOGRAPH (adjacent to, not within, private use)"},
	}

	for _, tc := range cases {
		t.Run(tc.codeName, func(t *testing.T) {
			got := inTable(tc.table, tc.r)
			if got != tc.inTable {
				t.Fatalf("inTable(table %s, U+%04X %s) = %v, want %v", tc.name, tc.r, tc.codeName, got, tc.inTable)
			}
		})
	}
}

// TestZeroWidthSpaceMapping pins the deliberate, documented resolution
// (see mapChars in saslprep.go) of the one code point that appears in
// both RFC 3454 Table B.1 (mapped to nothing) and Table C.1.2 (mapped to
// SPACE): U+200B ZERO WIDTH SPACE. B.1 wins: the character is deleted,
// not turned into a space. This is a deliberate spec-ambiguity resolution,
// not an accident, so it is pinned here to prevent it silently drifting if
// mapChars is refactored.
func TestZeroWidthSpaceMapping(t *testing.T) {
	got, err := Prepare("a\u200bb") // U+200B between 'a' and 'b'
	if err != nil {
		t.Fatalf("Prepare: unexpected error: %v", err)
	}
	if want := "ab"; got != want {
		t.Fatalf("Prepare(%q) = %q, want %q (U+200B should be deleted per Table B.1, not turned into a space per Table C.1.2)", "a\u200bb", got, want)
	}
}

// TestBidiErrorNamesCodePoint checks that bidi-check failures identify the
// specific offending code point, matching the same requirement already
// enforced for prohibited-character errors by checkProhibited: naming a
// single code point does not leak the credential being prepared, so this
// does not conflict with the no-leak rule in TestErrorsDoNotLeakInput.
func TestBidiErrorNamesCodePoint(t *testing.T) {
	t.Run("last character not RandALCat", func(t *testing.T) {
		_, err := Prepare("ا1") // ARABIC LETTER ALEF then ASCII '1'
		if err == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(err.Error(), "U+0031") {
			t.Fatalf("error does not name the offending code point U+0031: %v", err)
		}
	})
	t.Run("mixed RandALCat and LCat", func(t *testing.T) {
		_, err := Prepare("اa") // ARABIC LETTER ALEF then LATIN SMALL LETTER A
		if err == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(err.Error(), "U+0061") {
			t.Fatalf("error does not name the offending LCat code point U+0061: %v", err)
		}
	})
	t.Run("first character not RandALCat, no LCat present", func(t *testing.T) {
		_, err := Prepare("1ا") // ASCII digit '1' (neither RandALCat nor LCat), then ARABIC LETTER ALEF
		if err == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(err.Error(), "U+0031") {
			t.Fatalf("error does not name the offending first code point U+0031: %v", err)
		}
	})
}
