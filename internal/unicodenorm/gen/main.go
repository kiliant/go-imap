//go:build ignore

// Command gen generates internal/unicodenorm/tables.go from the Unicode
// Character Database (UCD), version 15.0.0. It is not part of any normal
// build (see the "ignore" build tag above) and must be run manually, by
// file path (the "ignore" tag excludes it from `go run ./...`-style
// package paths), from the repository root, since outPath below is
// repo-root-relative:
//
//	go run internal/unicodenorm/gen/main.go
//
// It fetches, over the network, exactly two UCD files:
//
//	https://www.unicode.org/Public/15.0.0/ucd/UnicodeData.txt
//	https://www.unicode.org/Public/15.0.0/ucd/CompositionExclusions.txt
//
// and derives from them the canonical combining class table, the canonical
// decomposition table, the fully expanded compatibility decomposition table
// (used by NFKC/NFKD), and the canonical composition (pair) table used by
// package unicodenorm to implement Unicode Normalization Forms C and KC
// (NFC, NFKC). The generated file, tables.go, is committed to the
// repository so that no network access is required at build or test time.
package main

import (
	"bytes"
	"fmt"
	"go/format"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	unicodeDataURL   = "https://www.unicode.org/Public/15.0.0/ucd/UnicodeData.txt"
	compExclusionURL = "https://www.unicode.org/Public/15.0.0/ucd/CompositionExclusions.txt"

	outPath = "internal/unicodenorm/tables.go"
)

// unicodeChar holds the fields of interest from one UnicodeData.txt record.
type unicodeChar struct {
	cp      rune
	ccc     uint8
	decomp  []rune // canonical decomposition, one level (field 5, no <tag>)
	hasDeco bool
}

func fetch(url string) ([]byte, error) {
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: status %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// parseUnicodeData parses UnicodeData.txt, returning the canonical
// combining class, the one-level canonical decomposition mapping (no
// <tag> prefix -- this is what NFC uses), and the one-level compatibility
// decomposition mapping (mappings that DO carry a <tag> prefix, which is
// stripped). Both decomposition mappings returned here are one level only;
// fullyDecompose and fullyDecomposeCompat, below, expand them to a fixed
// point. "First>"/"Last>" range markers (used for huge algorithmic blocks
// such as CJK and Hangul) never carry a decomposition or non-zero ccc in
// the UCD, so no special-casing is required for them.
func parseUnicodeData(data []byte) (ccc map[rune]uint8, decomp map[rune][]rune, compatDecomp map[rune][]rune) {
	ccc = make(map[rune]uint8)
	decomp = make(map[rune][]rune)
	compatDecomp = make(map[rune][]rune)

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			continue
		}
		fields := strings.Split(line, ";")
		if len(fields) < 6 {
			continue
		}
		cp64, err := strconv.ParseInt(fields[0], 16, 32)
		if err != nil {
			continue
		}
		cp := rune(cp64)

		if cccField := fields[3]; cccField != "" && cccField != "0" {
			n, err := strconv.Atoi(cccField)
			if err != nil {
				log.Fatalf("bad ccc field for %04X: %q", cp, cccField)
			}
			if n < 0 || n > 255 {
				log.Fatalf("ccc out of uint8 range for %04X: %d", cp, n)
			}
			ccc[cp] = uint8(n)
		}

		decoField := strings.TrimSpace(fields[5])
		if decoField == "" {
			continue
		}
		isCompat := false
		if strings.HasPrefix(decoField, "<") {
			// Compatibility decomposition (e.g. <compat>, <font>, <super>,
			// <fraction>, <noBreak>, ...): NFC excludes these, but they are
			// still captured (tag stripped) into compatDecomp, which
			// fullyDecomposeCompat (below) expands for NFKC/NFKD (see
			// package doc).
			isCompat = true
			if idx := strings.IndexByte(decoField, '>'); idx >= 0 {
				decoField = strings.TrimSpace(decoField[idx+1:])
			}
		}
		parts := strings.Fields(decoField)
		runes := make([]rune, 0, len(parts))
		for _, p := range parts {
			v, err := strconv.ParseInt(p, 16, 32)
			if err != nil {
				log.Fatalf("bad decomposition field for %04X: %q", cp, decoField)
			}
			runes = append(runes, rune(v))
		}
		if isCompat {
			compatDecomp[cp] = runes
		} else {
			decomp[cp] = runes
		}
	}
	return ccc, decomp, compatDecomp
}

// parseCompositionExclusions parses CompositionExclusions.txt, which lists
// one code point per (non-comment, non-blank) line, optionally followed by
// a "#" comment.
func parseCompositionExclusions(data []byte) map[rune]bool {
	out := make(map[rune]bool)
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if idx := strings.IndexByte(line, '#'); idx >= 0 {
			line = strings.TrimSpace(line[:idx])
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		v, err := strconv.ParseInt(fields[0], 16, 32)
		if err != nil {
			log.Fatalf("bad composition exclusion line: %q", line)
		}
		out[rune(v)] = true
	}
	return out
}

// fullyDecompose expands cp's canonical decomposition recursively to a
// fixed point (i.e. every returned code point either has no canonical
// decomposition of its own, or is Hangul, which is handled algorithmically
// and never appears in the decomp map). Memoized via cache.
func fullyDecompose(cp rune, decomp map[rune][]rune, cache map[rune][]rune, visiting map[rune]bool) []rune {
	if r, ok := cache[cp]; ok {
		return r
	}
	d, ok := decomp[cp]
	if !ok {
		return nil
	}
	if visiting[cp] {
		log.Fatalf("cycle detected while decomposing %04X", cp)
	}
	visiting[cp] = true
	var out []rune
	for _, c := range d {
		if sub := fullyDecompose(c, decomp, cache, visiting); sub != nil {
			out = append(out, sub...)
		} else {
			out = append(out, c)
		}
	}
	visiting[cp] = false
	cache[cp] = out
	return out
}

// fullyDecomposeCompat expands cp's compatibility decomposition recursively
// to a fixed point per UAX #15: at each step, a code point is replaced by
// its compatibility mapping (compatDecomp) if it has one, otherwise by its
// canonical mapping (decomp), until a code point has neither. This
// interleaves the two mapping kinds -- a character reached via one kind of
// mapping may itself only carry the other -- which is why this cannot
// simply call fullyDecompose and patch in compatDecomp at the top level.
// Memoized via cache. Returns nil if cp has neither kind of decomposition.
func fullyDecomposeCompat(cp rune, decomp, compatDecomp map[rune][]rune, cache map[rune][]rune, visiting map[rune]bool) []rune {
	if r, ok := cache[cp]; ok {
		return r
	}
	d, ok := compatDecomp[cp]
	if !ok {
		d, ok = decomp[cp]
	}
	if !ok {
		return nil
	}
	if visiting[cp] {
		log.Fatalf("cycle detected while compat-decomposing %04X", cp)
	}
	visiting[cp] = true
	var out []rune
	for _, c := range d {
		if sub := fullyDecomposeCompat(c, decomp, compatDecomp, cache, visiting); sub != nil {
			out = append(out, sub...)
		} else {
			out = append(out, c)
		}
	}
	visiting[cp] = false
	cache[cp] = out
	return out
}

func main() {
	log.Printf("fetching %s", unicodeDataURL)
	udData, err := fetch(unicodeDataURL)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("fetching %s", compExclusionURL)
	ceData, err := fetch(compExclusionURL)
	if err != nil {
		log.Fatal(err)
	}

	ccc, decomp, compatDecomp := parseUnicodeData(udData)
	scriptExclusions := parseCompositionExclusions(ceData)
	log.Printf("parsed %d ccc entries, %d canonical decompositions, %d compatibility decompositions, %d script exclusions",
		len(ccc), len(decomp), len(compatDecomp), len(scriptExclusions))

	ccOf := func(r rune) uint8 { return ccc[r] } // 0 default for unlisted code points

	// Full composition exclusion set per UAX #15: script-specific
	// exclusions, plus singleton decompositions, plus decompositions whose
	// first character is not a starter (ccc != 0).
	fullExclusion := make(map[rune]bool, len(scriptExclusions))
	for cp := range scriptExclusions {
		fullExclusion[cp] = true
	}
	for cp, d := range decomp {
		if len(d) == 1 {
			fullExclusion[cp] = true
		} else if len(d) >= 2 && ccOf(d[0]) != 0 {
			fullExclusion[cp] = true
		}
	}

	// Composition pairs: derived from the ONE-LEVEL (not recursively
	// expanded) canonical decomposition of exactly two code points, whose
	// composite is not excluded.
	type pair struct {
		key uint64
		val rune
	}
	var pairs []pair
	for cp, d := range decomp {
		if len(d) != 2 {
			continue
		}
		if fullExclusion[cp] {
			continue
		}
		key := uint64(uint32(d[0]))<<32 | uint64(uint32(d[1]))
		pairs = append(pairs, pair{key, cp})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].key < pairs[j].key })
	log.Printf("built %d composition pairs", len(pairs))

	// Recursive (fully expanded) decomposition table.
	cache := make(map[rune][]rune)
	visiting := make(map[rune]bool)
	type decoEntry struct {
		cp   rune
		data []rune
	}
	var decoEntries []decoEntry
	for cp := range decomp {
		full := fullyDecompose(cp, decomp, cache, visiting)
		decoEntries = append(decoEntries, decoEntry{cp, full})
	}
	sort.Slice(decoEntries, func(i, j int) bool { return decoEntries[i].cp < decoEntries[j].cp })

	// Compatibility decomposition table: fully (recursively) expanded to a
	// fixed point per UAX #15, using the compatibility mapping when a code
	// point has one and falling back to the canonical mapping otherwise
	// (see fullyDecomposeCompat). Consumed by NFKC/NFKD. Built over the
	// union of decomp and compatDecomp keys, not just compatDecomp keys:
	// a code point with only a canonical mapping can still need its
	// canonical decomposition members expanded further via a compatibility
	// mapping reached transitively (and vice versa), so every code point
	// that decomposes at all -- by either mapping -- needs an entry here.
	compatCache := make(map[rune][]rune)
	compatVisiting := make(map[rune]bool)
	compatCps := make(map[rune]bool, len(decomp)+len(compatDecomp))
	for cp := range decomp {
		compatCps[cp] = true
	}
	for cp := range compatDecomp {
		compatCps[cp] = true
	}
	var compatEntries []decoEntry
	for cp := range compatCps {
		full := fullyDecomposeCompat(cp, decomp, compatDecomp, compatCache, compatVisiting)
		compatEntries = append(compatEntries, decoEntry{cp, full})
	}
	sort.Slice(compatEntries, func(i, j int) bool { return compatEntries[i].cp < compatEntries[j].cp })

	// Combining class table (non-zero entries only).
	type cccEntry struct {
		cp  rune
		ccc uint8
	}
	var cccEntries []cccEntry
	for cp, c := range ccc {
		cccEntries = append(cccEntries, cccEntry{cp, c})
	}
	sort.Slice(cccEntries, func(i, j int) bool { return cccEntries[i].cp < cccEntries[j].cp })

	var buf bytes.Buffer
	buf.WriteString("// Code generated by gen/main.go. DO NOT EDIT.\n")
	buf.WriteString("// Source: Unicode Character Database version 15.0.0.\n")
	buf.WriteString("//   https://www.unicode.org/Public/15.0.0/ucd/UnicodeData.txt\n")
	buf.WriteString("//   https://www.unicode.org/Public/15.0.0/ucd/CompositionExclusions.txt\n\n")
	buf.WriteString("package unicodenorm\n\n")

	// --- ccc table ---
	fmt.Fprintf(&buf, "// cccKey and cccVal together form a sorted parallel-array map from a\n")
	fmt.Fprintf(&buf, "// code point to its canonical combining class (Unicode property ccc).\n")
	fmt.Fprintf(&buf, "// Code points absent from cccKey have ccc == 0.\n")
	fmt.Fprintf(&buf, "var cccKey = [...]int32{")
	for i, e := range cccEntries {
		if i%16 == 0 {
			buf.WriteString("\n\t")
		}
		fmt.Fprintf(&buf, "0x%X, ", e.cp)
	}
	buf.WriteString("\n}\n\n")

	fmt.Fprintf(&buf, "var cccVal = [...]uint8{")
	for i, e := range cccEntries {
		if i%16 == 0 {
			buf.WriteString("\n\t")
		}
		fmt.Fprintf(&buf, "%d, ", e.ccc)
	}
	buf.WriteString("\n}\n\n")

	// --- decomposition table ---
	fmt.Fprintf(&buf, "// decompKey, decompOffset and decompData together form a sorted map\n")
	fmt.Fprintf(&buf, "// from a code point to its fully (recursively) expanded canonical\n")
	fmt.Fprintf(&buf, "// decomposition: decompData[decompOffset[i]:decompOffset[i+1]] is the\n")
	fmt.Fprintf(&buf, "// decomposition of decompKey[i]. Hangul syllables are excluded: they\n")
	fmt.Fprintf(&buf, "// decompose algorithmically, see hangulDecompose.\n")
	fmt.Fprintf(&buf, "var decompKey = [...]int32{")
	for i, e := range decoEntries {
		if i%16 == 0 {
			buf.WriteString("\n\t")
		}
		fmt.Fprintf(&buf, "0x%X, ", e.cp)
	}
	buf.WriteString("\n}\n\n")

	fmt.Fprintf(&buf, "var decompOffset = [...]int32{")
	offset := 0
	for i, e := range decoEntries {
		if i%16 == 0 {
			buf.WriteString("\n\t")
		}
		fmt.Fprintf(&buf, "%d, ", offset)
		offset += len(e.data)
	}
	fmt.Fprintf(&buf, "%d, ", offset) // trailing sentinel
	buf.WriteString("\n}\n\n")

	fmt.Fprintf(&buf, "var decompData = [...]int32{")
	n := 0
	for _, e := range decoEntries {
		for _, r := range e.data {
			if n%16 == 0 {
				buf.WriteString("\n\t")
			}
			fmt.Fprintf(&buf, "0x%X, ", r)
			n++
		}
	}
	buf.WriteString("\n}\n\n")

	// --- compatibility decomposition table (used by NFKC/NFKD) ---
	fmt.Fprintf(&buf, "// compatDecompKey, compatDecompOffset and compatDecompData mirror\n")
	fmt.Fprintf(&buf, "// decompKey/decompOffset/decompData above, but hold the fully\n")
	fmt.Fprintf(&buf, "// (recursively) expanded COMPATIBILITY decomposition: at each step a\n")
	fmt.Fprintf(&buf, "// code point is replaced by its compatibility mapping (UnicodeData.txt\n")
	fmt.Fprintf(&buf, "// field 5 entries carrying a <tag>, tag stripped) if it has one,\n")
	fmt.Fprintf(&buf, "// otherwise by its canonical mapping, until a code point has neither\n")
	fmt.Fprintf(&buf, "// (see fullyDecomposeCompat in gen/main.go). This covers every code\n")
	fmt.Fprintf(&buf, "// point that decomposes at all, by either mapping -- a superset of the\n")
	fmt.Fprintf(&buf, "// keys in decompKey.\n")
	fmt.Fprintf(&buf, "//\n")
	fmt.Fprintf(&buf, "// This table is NOT used by NFC: NFC (see norm.go) follows canonical\n")
	fmt.Fprintf(&buf, "// decompositions only, per UAX #15. It is used by NFKC (see norm.go),\n")
	fmt.Fprintf(&buf, "// needed for SASLprep (RFC 4013 / RFC 5802).\n")
	fmt.Fprintf(&buf, "var compatDecompKey = [...]int32{")
	for i, e := range compatEntries {
		if i%16 == 0 {
			buf.WriteString("\n\t")
		}
		fmt.Fprintf(&buf, "0x%X, ", e.cp)
	}
	buf.WriteString("\n}\n\n")

	fmt.Fprintf(&buf, "var compatDecompOffset = [...]int32{")
	coffset := 0
	for i, e := range compatEntries {
		if i%16 == 0 {
			buf.WriteString("\n\t")
		}
		fmt.Fprintf(&buf, "%d, ", coffset)
		coffset += len(e.data)
	}
	fmt.Fprintf(&buf, "%d, ", coffset) // trailing sentinel
	buf.WriteString("\n}\n\n")

	fmt.Fprintf(&buf, "var compatDecompData = [...]int32{")
	cn := 0
	for _, e := range compatEntries {
		for _, r := range e.data {
			if cn%16 == 0 {
				buf.WriteString("\n\t")
			}
			fmt.Fprintf(&buf, "0x%X, ", r)
			cn++
		}
	}
	buf.WriteString("\n}\n\n")

	// --- composition table ---
	fmt.Fprintf(&buf, "// composeKey and composeVal together form a sorted parallel-array map\n")
	fmt.Fprintf(&buf, "// from a starter/combiner pair, packed as uint64(first)<<32|uint64(second),\n")
	fmt.Fprintf(&buf, "// to their canonical composite code point.\n")
	fmt.Fprintf(&buf, "var composeKey = [...]uint64{")
	for i, p := range pairs {
		if i%8 == 0 {
			buf.WriteString("\n\t")
		}
		fmt.Fprintf(&buf, "0x%X, ", p.key)
	}
	buf.WriteString("\n}\n\n")

	fmt.Fprintf(&buf, "var composeVal = [...]int32{")
	for i, p := range pairs {
		if i%16 == 0 {
			buf.WriteString("\n\t")
		}
		fmt.Fprintf(&buf, "0x%X, ", p.val)
	}
	buf.WriteString("\n}\n")

	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		log.Fatalf("gofmt: %v\n--- raw source ---\n%s", err, buf.String())
	}

	if err := os.WriteFile(outPath, formatted, 0o644); err != nil {
		log.Fatal(err)
	}
	log.Printf("wrote %s (%d bytes)", outPath, len(formatted))
}
