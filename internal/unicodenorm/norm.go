// Package unicodenorm implements Unicode Normalization Forms C and KC (NFC,
// NFKC) using generated tables derived from the Unicode Character Database
// (UCD), version 15.0.0 (matching the standard library's unicode.Version on
// the toolchain this package was generated with). It uses only the Go
// standard library: no external dependencies, no golang.org/x/text.
//
// Both forms follow UAX #15 (Unicode Normalization Forms) and share the
// same last two steps; they differ only in the decomposition step:
//
//  1. Decomposition of every code point to a fixed point: canonical
//     decomposition only for NFC, or compatibility decomposition
//     (falling back to canonical where a code point has no compatibility
//     mapping of its own) for NFKC.
//  2. Canonical ordering: a stable sort, by combining class, of each
//     maximal run of non-starter code points (ccc > 0).
//  3. Canonical composition: a starter is greedily composed with each
//     following character that (a) forms a known canonical composite pair
//     with it and (b) is not "blocked" from it by an intervening
//     character of combining class zero, or of combining class greater
//     than or equal to the candidate's.
//
// NFKC is required by SASLprep (RFC 4013 / RFC 5802), which mandates NFKC
// rather than NFC.
//
// Hangul syllables (U+AC00-U+D7A3) are handled algorithmically per UAX #15
// rather than via table lookups.
package unicodenorm

import "sort"

// Hangul constants, per UAX #15.
const (
	sBase  = 0xAC00
	lBase  = 0x1100
	vBase  = 0x1161
	tBase  = 0x11A7
	lCount = 19
	vCount = 21
	tCount = 28
	nCount = vCount * tCount // 588
	sCount = lCount * nCount // 11172
)

// NFC returns the Unicode Normalization Form C of s.
//
// s is decoded as UTF-8 by ranging over it (as encoding/range does): any
// invalid UTF-8 byte sequence is replaced by U+FFFD REPLACEMENT CHARACTER,
// the same behavior as the standard library's range-over-string and
// unicode/utf8 decoders. NFC never panics on malformed input.
func NFC(s string) string {
	if s == "" {
		return ""
	}

	runes := decomposeString(s)
	canonicalOrder(runes)
	runes = compose(runes)

	return string(runes)
}

// NFKC returns the Unicode Normalization Form KC of s.
//
// Like NFC, NFKC decodes s as UTF-8 by ranging over it (as encoding/range
// does): any invalid UTF-8 byte sequence is replaced by U+FFFD REPLACEMENT
// CHARACTER. NFKC never panics on malformed input.
//
// NFKC differs from NFC only in its decomposition step: it decomposes using
// the fully expanded COMPATIBILITY decomposition (see compatDecompose)
// instead of the canonical-only one, then applies the same canonical
// ordering and canonical composition steps NFC uses (see canonicalOrder and
// compose, both reused unchanged). NFKC is a lossy normalization -- e.g. it
// maps single-character compatibility variants (superscripts, ligatures,
// fullwidth forms, ...) to their canonical equivalents -- and is required
// by SASLprep (RFC 4013 / RFC 5802), which mandates NFKC rather than NFC.
func NFKC(s string) string {
	if s == "" {
		return ""
	}

	runes := decomposeCompatString(s)
	canonicalOrder(runes)
	runes = compose(runes)

	return string(runes)
}

// combiningClass returns the canonical combining class of r.
func combiningClass(r rune) uint8 {
	// Hangul (syllables, and their L/V/T jamo constituents) are always
	// starters (ccc == 0), so no table lookup is needed for them.
	i := sort.Search(len(cccKey), func(i int) bool { return cccKey[i] >= int32(r) })
	if i < len(cccKey) && cccKey[i] == int32(r) {
		return cccVal[i]
	}
	return 0
}

// decompose returns r's fully expanded canonical decomposition, or nil if r
// has none (including if r is not assigned a canonical decomposition at
// all). Hangul syllables are decomposed algorithmically.
func decompose(r rune) []rune {
	if r >= sBase && r < sBase+sCount {
		return hangulDecompose(r)
	}
	i := sort.Search(len(decompKey), func(i int) bool { return decompKey[i] >= int32(r) })
	if i < len(decompKey) && decompKey[i] == int32(r) {
		start, end := decompOffset[i], decompOffset[i+1]
		out := make([]rune, end-start)
		for j := range out {
			out[j] = rune(decompData[int(start)+j])
		}
		return out
	}
	return nil
}

// compatDecompose returns r's fully expanded compatibility decomposition,
// or nil if r has neither a compatibility nor a canonical decomposition.
// Per UAX #15, r is repeatedly replaced by its compatibility mapping
// (UnicodeData.txt field 5 entries carrying a <tag>, tag stripped) if it
// has one, otherwise by its canonical mapping, until a code point has
// neither -- so this interleaves the two mapping kinds, since a character
// reached through one kind of mapping may itself only carry the other.
// Hangul syllables are decomposed algorithmically, as with decompose.
//
// NFC (see NFC, above) never calls this: NFC follows canonical
// decompositions only, per UAX #15. It is the decomposition step of NFKC
// (see NFKC, below), needed for SASLprep (RFC 4013 / RFC 5802).
func compatDecompose(r rune) []rune {
	if r >= sBase && r < sBase+sCount {
		return hangulDecompose(r)
	}
	i := sort.Search(len(compatDecompKey), func(i int) bool { return compatDecompKey[i] >= int32(r) })
	if i < len(compatDecompKey) && compatDecompKey[i] == int32(r) {
		start, end := compatDecompOffset[i], compatDecompOffset[i+1]
		out := make([]rune, end-start)
		for j := range out {
			out[j] = rune(compatDecompData[int(start)+j])
		}
		return out
	}
	return nil
}

// hangulDecompose algorithmically decomposes a precomposed Hangul syllable
// into its Leading, Vowel, and (if present) Trailing jamo, per UAX #15.
func hangulDecompose(r rune) []rune {
	sIndex := r - sBase
	l := lBase + sIndex/nCount
	v := vBase + (sIndex%nCount)/tCount
	t := tBase + sIndex%tCount
	if sIndex%tCount == 0 {
		// No trailing consonant.
		return []rune{l, v}
	}
	return []rune{l, v, t}
}

// decomposeString applies canonical decomposition to every rune of s,
// recursively to a fixed point, and concatenates the results in order.
func decomposeString(s string) []rune {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if d := decompose(r); d != nil {
			out = append(out, d...)
		} else {
			out = append(out, r)
		}
	}
	return out
}

// decomposeCompatString applies compatibility decomposition (see
// compatDecompose) to every rune of s, recursively to a fixed point, and
// concatenates the results in order. This is the decomposition step of
// NFKC, analogous to decomposeString for NFC.
func decomposeCompatString(s string) []rune {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if d := compatDecompose(r); d != nil {
			out = append(out, d...)
		} else {
			out = append(out, r)
		}
	}
	return out
}

// canonicalOrder reorders runs of non-starter (ccc > 0) code points into
// ascending order of combining class, using a stable sort so that code
// points already sharing a combining class keep their relative order, as
// required by UAX #15.
func canonicalOrder(runes []rune) {
	n := len(runes)
	for i := 0; i < n; {
		if combiningClass(runes[i]) == 0 {
			i++
			continue
		}
		j := i + 1
		for j < n && combiningClass(runes[j]) != 0 {
			j++
		}
		if j-i > 1 {
			run := runes[i:j]
			sort.SliceStable(run, func(a, b int) bool {
				return combiningClass(run[a]) < combiningClass(run[b])
			})
		}
		i = j
	}
}

// composePair looks up the canonical composite of the ordered pair
// (first, second), if one exists.
func composePair(first, second rune) (rune, bool) {
	if isHangulL(first) && isHangulV(second) {
		lIndex := first - lBase
		vIndex := second - vBase
		return sBase + (lIndex*vCount+vIndex)*tCount, true
	}
	if isHangulLV(first) && isHangulT(second) {
		tIndex := second - tBase
		return first + tIndex, true
	}
	key := uint64(uint32(first))<<32 | uint64(uint32(second))
	i := sort.Search(len(composeKey), func(i int) bool { return composeKey[i] >= key })
	if i < len(composeKey) && composeKey[i] == key {
		return rune(composeVal[i]), true
	}
	return 0, false
}

func isHangulL(r rune) bool  { return r >= lBase && r < lBase+lCount }
func isHangulV(r rune) bool  { return r >= vBase && r < vBase+vCount }
func isHangulT(r rune) bool  { return r > tBase && r < tBase+tCount }
func isHangulLV(r rune) bool { return r >= sBase && r < sBase+sCount && (r-sBase)%tCount == 0 }

// compose performs canonical composition over a canonically-ordered
// sequence of runes, per UAX #15: for each starter L, later characters are
// greedily composed into it in turn, unless blocked by an intervening
// character with combining class zero or with a combining class greater
// than or equal to the candidate's own.
func compose(runes []rune) []rune {
	if len(runes) == 0 {
		return runes
	}

	out := make([]rune, 0, len(runes))
	out = append(out, runes[0])

	// starterIdx indexes into out: the position of the last starter that
	// composition attempts are made against.
	starterIdx := 0
	if combiningClass(runes[0]) != 0 {
		// A leading non-starter can never compose (there is no preceding
		// starter); mark starterIdx as "none" by using -1.
		starterIdx = -1
	}
	// blockingClass tracks the highest combining class seen since
	// starterIdx, to implement the "blocked" rule.
	blockingClass := int(-1)

	for _, r := range runes[1:] {
		ccc := combiningClass(r)

		var composed rune
		var ok bool
		if starterIdx >= 0 {
			// Blocked (per UAX #15) if some character B intervenes between
			// the starter and r with ccc(B) == 0 or ccc(B) >= ccc(r).
			// blockingClass == -1 means nothing has intervened since the
			// starter (i.e. r immediately follows it), so composition may
			// be attempted even when ccc(r) == 0 itself (e.g. adjacent
			// Hangul jamo); otherwise any intervening character blocks a
			// zero-ccc candidate, since ccc(B) >= 0 is trivially true.
			blocked := blockingClass >= int(ccc)
			if !blocked {
				composed, ok = composePair(out[starterIdx], r)
			}
		}

		if ok {
			out[starterIdx] = composed
			// The composite replaces the starter; blockingClass is
			// unaffected by this position since it was consumed, not
			// appended.
			continue
		}

		out = append(out, r)
		if ccc == 0 {
			starterIdx = len(out) - 1
			blockingClass = -1
		} else if starterIdx >= 0 {
			if int(ccc) > blockingClass {
				blockingClass = int(ccc)
			}
		}
	}

	return out
}
