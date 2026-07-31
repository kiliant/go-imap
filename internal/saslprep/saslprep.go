// Package saslprep implements SASLprep (RFC 4013), the profile of
// stringprep (RFC 3454) used to prepare user names and passwords for
// comparison in SASL mechanisms such as SCRAM (RFC 5802) and PLAIN
// (RFC 4616). It has no dependency on, and is not wired into, any SASL
// mechanism implementation in this module: it is a pure string transform,
// exposed so that callers elsewhere in the tree (or callers of this module,
// via a future public wrapper) can apply it explicitly.
//
// # Algorithm
//
// Per RFC 4013 Section 2, referencing RFC 3454 Section 3, four steps are
// applied in order:
//
//  1. Map: each character in RFC 3454 Table C.1.2 (non-ASCII space) maps to
//     U+0020 SPACE; each character in Table B.1 (commonly mapped to
//     nothing, e.g. soft hyphen, zero width characters) is deleted.
//  2. Normalize: the mapped string is normalized to Unicode Normalization
//     Form KC (NFKC), using [github.com/kiliant/go-imap/internal/unicodenorm.NFKC].
//     SASLprep does not fold case; "USER" and "user" remain distinct.
//  3. Prohibit: the normalized string must not contain any character from
//     RFC 3454 Tables C.1.2, C.2.1, C.2.2, C.3, C.4, C.5, C.6, C.7, C.8 or
//     C.9. [PrepareStored] additionally prohibits Table A.1 (code points
//     unassigned in Unicode 3.2), per RFC 3454 Section 7's rule that stored
//     strings must not contain unassigned code points while queries may.
//  4. Check bidi: per RFC 3454 Section 6, if the string contains any
//     character with bidirectional property R or AL (Table D.1,
//     "RandALCat"), then it must not contain any character with
//     bidirectional property L (Table D.2, "LCat"), and its first and last
//     characters must both be RandALCat.
//
// # Unicode version mismatch (deliberate)
//
// RFC 3454's tables (A.1 through D.2) are normatively fixed to Unicode
// 3.2 and will never be updated; RFC 3454 Section 7 exists specifically to
// make that safe by splitting code points into "assigned" and
// "unassigned" categories rather than requiring profiles to track new
// Unicode versions. This package's table data (tables.go) is therefore,
// correctly, frozen to Unicode 3.2 (see internal/saslprep/gen/main.go).
//
// The NFKC normalization step, by contrast, uses
// [github.com/kiliant/go-imap/internal/unicodenorm], whose tables are
// generated from Unicode 15.0.0 (matching the Go toolchain's
// unicode.Version at the time it was generated -- see
// internal/unicodenorm/gen/main.go).
//
// These two Unicode versions are deliberately different, and this is not a
// bug to "fix": every practical SASLprep implementation (including the
// reference behavior relied on by interoperating SCRAM/PLAIN clients and
// servers) normalizes with a current Unicode version while still
// classifying unassigned/prohibited/bidi code points against the frozen
// RFC 3454 tables, precisely because RFC 3454 Section 7 designed the
// unassigned-code-point category to tolerate exactly this kind of skew.
// Pinning normalization itself to Unicode 3.2 would require carrying a
// second, independent, long-unmaintained Unicode Character Database
// purely for this one profile, for no interoperability benefit.
package saslprep

import (
	"fmt"
	"sort"
	"strings"

	"github.com/kiliant/go-imap/internal/unicodenorm"
)

// Prepare applies SASLprep (RFC 4013) to s, treating s as a query (RFC 3454
// Section 7): code points unassigned in Unicode 3.2 (Table A.1) are
// permitted. Use [PrepareStored] instead when s is being prepared once for
// long-term storage (e.g. persisted as a credential to compare future
// queries against), per RFC 4013's distinction between the two.
//
// Prepare returns an error if s contains a prohibited code point (RFC 3454
// Tables C.1.2, C.2.1, C.2.2, C.3, C.4, C.5, C.6, C.7, C.8 or C.9, checked
// after mapping and NFKC normalization) or violates the bidirectional
// character checks of RFC 3454 Section 6.
func Prepare(s string) (string, error) {
	return prepare(s, false)
}

// PrepareStored is [Prepare] with code points unassigned in Unicode 3.2
// (RFC 3454 Table A.1) additionally prohibited, as RFC 4013 requires when
// preparing a string for storage rather than for a one-off query (see RFC
// 3454 Section 7).
func PrepareStored(s string) (string, error) {
	return prepare(s, true)
}

// prepare implements the four SASLprep steps documented in the package
// comment: map, normalize, prohibit, check bidi.
func prepare(s string, prohibitUnassigned bool) (string, error) {
	mapped := mapChars(s)
	normalized := unicodenorm.NFKC(mapped)

	if err := checkProhibited(normalized, prohibitUnassigned); err != nil {
		return "", err
	}
	if err := checkBidi(normalized); err != nil {
		return "", err
	}
	return normalized, nil
}

// mapChars applies the SASLprep "Map" step: RFC 3454 Table B.1 code points
// are deleted, and Table C.1.2 code points become U+0020 SPACE. All other
// code points pass through unchanged.
//
// Table B.1 is checked before Table C.1.2 deliberately: RFC 3454 defines
// them as two independent mapping tables and does not itself state a
// precedence, but exactly one code point, U+200B ZERO WIDTH SPACE, appears
// in both (B.1 "commonly mapped to nothing" and C.1.2 "non-ASCII space
// characters"), and neither the RFC nor its errata resolve the conflict.
// Checking B.1 first (delete wins over map-to-space for U+200B) matches
// the order used by xdg-go/stringprep (github.com/xdg-go/stringprep,
// used by the MongoDB Go driver's SCRAM implementation), which is treated
// here as the de facto interoperability precedent. See TestZeroWidthSpaceMapping.
func mapChars(s string) string {
	var sb strings.Builder
	sb.Grow(len(s))
	for _, r := range s {
		switch {
		case inTable(tableB1[:], r):
			// Map to nothing: character is dropped.
		case inTable(tableC12[:], r):
			sb.WriteRune(' ')
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// prohibitedTable names one RFC 3454 table checked by the SASLprep
// "Prohibit" step, paired with a description used in error messages.
type prohibitedTable struct {
	rfcTable string
	table    []interval
}

// prohibitedTables lists the RFC 3454 tables SASLprep prohibits
// unconditionally in every profile (RFC 4013 Section 2.3). Table A.1
// (unassigned code points) is deliberately not included here: whether it
// is prohibited depends on the prohibitUnassigned argument to prepare, per
// RFC 3454 Section 7, and is checked separately in checkProhibited.
var prohibitedTables = []prohibitedTable{
	{"C.1.2 (non-ASCII space character)", tableC12[:]},
	{"C.2.1 (ASCII control character)", tableC21[:]},
	{"C.2.2 (non-ASCII control character)", tableC22[:]},
	{"C.3 (private use code point)", tableC3[:]},
	{"C.4 (non-character code point)", tableC4[:]},
	{"C.5 (surrogate code point)", tableC5[:]},
	{"C.6 (inappropriate for plain text)", tableC6[:]},
	{"C.7 (inappropriate for canonical representation)", tableC7[:]},
	{"C.8 (changes display properties, or deprecated)", tableC8[:]},
	{"C.9 (tagging character)", tableC9[:]},
}

// checkProhibited implements the SASLprep "Prohibit" step over s (which
// must already be mapped and NFKC-normalized): it is an error if s
// contains any code point from prohibitedTables, or -- when
// prohibitUnassigned is set, as required for stored strings by RFC 3454
// Section 7 -- from Table A.1 (code points unassigned in Unicode 3.2).
//
// The Table A.1 check, like every other check here, runs against s AFTER
// NFKC normalization, per the step order in the package doc comment (Map,
// Normalize, Prohibit, Check bidi) and in RFC 3454 Section 3. One
// consequence, not a bug: a code point unassigned in Unicode 3.2 whose
// Unicode 15.0.0 NFKC compatibility decomposition folds entirely to
// assigned characters (plausible for some post-3.2 compatibility
// characters, e.g. certain superscript/subscript letters) will pass
// PrepareStored, because by the time this check runs, that code point is
// no longer present in the string -- only its decomposition is. This
// mirrors the RFC's own algorithm order exactly and is not something to
// "fix" by moving the check earlier.
//
// The returned error identifies the violated rule and the offending code
// point only. It must never embed s itself: s is, on this code path, a
// SASL credential (a user name or password), and credentials must never
// appear in an error that might be logged or otherwise surfaced. Do not
// "helpfully" add s (or any substring of it) to these messages later.
func checkProhibited(s string, prohibitUnassigned bool) error {
	for _, r := range s {
		if prohibitUnassigned && inTable(tableA1[:], r) {
			return fmt.Errorf("saslprep: prohibited by RFC 3454 Table A.1 (unassigned code point in Unicode 3.2): U+%04X", r)
		}
		for _, pt := range prohibitedTables {
			if inTable(pt.table, r) {
				return fmt.Errorf("saslprep: prohibited by RFC 3454 Table %s: U+%04X", pt.rfcTable, r)
			}
		}
	}
	return nil
}

// checkBidi implements the SASLprep "Check bidi" step (RFC 3454 Section 6)
// over s (which must already be mapped and NFKC-normalized): if s contains
// any RandALCat character (Table D.1: bidirectional property R or AL),
// then s must not contain any LCat character (Table D.2: bidirectional
// property L), and the first and last characters of s must both be
// RandALCat.
//
// As with checkProhibited, any error returned identifies the violated rule
// only, never s itself: s is a SASL credential and must not reach an error
// that might be logged.
func checkBidi(s string) error {
	runes := []rune(s)
	if len(runes) == 0 {
		return nil
	}

	hasRandALCat := false
	hasLCat := false
	for _, r := range runes {
		if inTable(tableD1[:], r) {
			hasRandALCat = true
		}
		if inTable(tableD2[:], r) {
			hasLCat = true
		}
	}
	if !hasRandALCat {
		// RFC 3454 Section 6's rule only applies to strings containing a
		// RandALCat character; a string with none (including one with
		// LCat characters, or with neither) is unaffected.
		return nil
	}
	if hasLCat {
		for _, r := range runes {
			if inTable(tableD2[:], r) {
				return fmt.Errorf("saslprep: bidi check failed (RFC 3454 Section 6): string contains a RandALCat (Table D.1) character and also an LCat (Table D.2) character: U+%04X", r)
			}
		}
	}
	if !inTable(tableD1[:], runes[0]) {
		return fmt.Errorf("saslprep: bidi check failed (RFC 3454 Section 6): a RandALCat (Table D.1) string must start with a RandALCat character, but the first character is U+%04X", runes[0])
	}
	if !inTable(tableD1[:], runes[len(runes)-1]) {
		return fmt.Errorf("saslprep: bidi check failed (RFC 3454 Section 6): a RandALCat (Table D.1) string must end with a RandALCat character, but the last character is U+%04X", runes[len(runes)-1])
	}
	return nil
}

// inTable reports whether r falls within any of the disjoint, ascending
// intervals in t, via binary search. t must be sorted by lo and disjoint,
// as produced by internal/saslprep/gen/main.go.
func inTable(t []interval, r rune) bool {
	i := sort.Search(len(t), func(i int) bool { return t[i].hi >= int32(r) })
	return i < len(t) && t[i].lo <= int32(r)
}
