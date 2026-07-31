//go:build ignore

// Command gen generates internal/saslprep/tables.go from RFC 3454
// ("Preparation of Internationalized Strings ('stringprep')"), section 3
// (Appendices A, B, C and D), version Unicode 3.2. It is not part of any
// normal build (see the "ignore" build tag above) and must be run manually,
// by file path (the "ignore" tag excludes it from `go run ./...`-style
// package paths), from the repository root, since outPath below is
// repo-root-relative:
//
//	go run internal/saslprep/gen/main.go
//
// It fetches, over the network, exactly one document:
//
//	https://www.rfc-editor.org/rfc/rfc3454.txt
//
// and extracts from its plain-text tables the specific subset needed to
// implement SASLprep (RFC 4013), which references RFC 3454 tables A.1, B.1,
// C.1.2, C.2.1, C.2.2, C.3, C.4, C.5, C.6, C.7, C.8, C.9, D.1 and D.2.
// Tables B.2 and B.3 (case-folding maps used by other stringprep profiles,
// not SASLprep) are intentionally not extracted. The generated file,
// tables.go, is committed to the repository so that no network access is
// required at build or test time.
//
// RFC 3454's tables are frozen to Unicode 3.2 -- they will never be
// reissued for a newer Unicode version -- so, unlike internal/unicodenorm's
// generator (which tracks the Go toolchain's Unicode version), this
// generator has no version parameter: RFC 3454 IS the version.
package main

import (
	"bytes"
	"fmt"
	"go/format"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	rfc3454URL = "https://www.rfc-editor.org/rfc/rfc3454.txt"

	outPath = "internal/saslprep/tables.go"
)

// tableSpec describes one RFC 3454 appendix table to extract, and the name
// of the exported-within-package Go variable ([]interval) generated for it.
type tableSpec struct {
	rfcName string // e.g. "C.1.2", as it appears in "Start Table C.1.2"
	varName string // e.g. "tableC12"
	doc     string // one-line doc comment for the generated variable
}

var tableSpecs = []tableSpec{
	{"A.1", "tableA1", "Table A.1: unassigned code points in Unicode 3.2."},
	{"B.1", "tableB1", "Table B.1: commonly mapped to nothing."},
	{"C.1.2", "tableC12", "Table C.1.2: non-ASCII space characters."},
	{"C.2.1", "tableC21", "Table C.2.1: ASCII control characters."},
	{"C.2.2", "tableC22", "Table C.2.2: non-ASCII control characters."},
	{"C.3", "tableC3", "Table C.3: private use."},
	{"C.4", "tableC4", "Table C.4: non-character code points."},
	{"C.5", "tableC5", "Table C.5: surrogate codes."},
	{"C.6", "tableC6", "Table C.6: inappropriate for plain text."},
	{"C.7", "tableC7", "Table C.7: inappropriate for canonical representation."},
	{"C.8", "tableC8", "Table C.8: change display properties or are deprecated."},
	{"C.9", "tableC9", "Table C.9: tagging characters."},
	{"D.1", "tableD1", "Table D.1: characters with bidirectional property \"R\" or \"AL\"."},
	{"D.2", "tableD2", "Table D.2: characters with bidirectional property \"L\"."},
}

// interval is a closed, inclusive code point range [lo, hi].
type interval struct {
	lo, hi rune
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

// extractTable returns the raw line-by-line body of the named table (e.g.
// "C.1.2"), i.e. everything strictly between its
//
//	----- Start Table <name> -----
//	----- End Table <name> -----
//
// delimiter lines.
func extractTable(text, name string) (string, error) {
	startMarker := "----- Start Table " + name + " -----"
	endMarker := "----- End Table " + name + " -----"

	startIdx := strings.Index(text, startMarker)
	if startIdx < 0 {
		return "", fmt.Errorf("start marker for table %s not found", name)
	}
	bodyStart := startIdx + len(startMarker)

	endIdx := strings.Index(text[bodyStart:], endMarker)
	if endIdx < 0 {
		return "", fmt.Errorf("end marker for table %s not found", name)
	}

	return text[bodyStart : bodyStart+endIdx], nil
}

// rowPattern matches one data row of an RFC 3454 appendix table, once the
// line has been trimmed of surrounding whitespace:
//
//	HHHH                   (a single code point, tables A.1/D.1/D.2)
//	HHHH-HHHH              (an inclusive range, tables A.1/D.1/D.2)
//	HHHH; NAME             (a single code point with a comment, tables C.*)
//	HHHH-HHHH; NAME        (a range with a comment, tables C.*)
//	HHHH; ; Map to nothing (table B.1: two semicolon-delimited fields)
//
// It deliberately does not need to distinguish these cases: only the code
// point(s) before the first ";" (if any) matter for SASLprep, since none of
// the tables it needs (A.1, B.1, C.*, D.1, D.2) carry a second-field
// mapping target -- that is only tables B.2/B.3, which SASLprep does not
// use and this generator does not extract.
//
// Anchored at both ends so that running prose, page headers/footers
// ("Hoffman & Blanchet ... [Page 79]", "RFC 3454 ... December 2002") and
// blank/form-feed lines interleaved by pagination never match: none of them
// begin with a bare hex run of two or more digits followed by end-of-line,
// '-' or ';'.
var rowPattern = regexp.MustCompile(`^([0-9A-Fa-f]{2,6})(?:-([0-9A-Fa-f]{2,6}))?(?:;.*)?$`)

// parseTableBody parses the body text of one RFC 3454 appendix table (as
// returned by extractTable) into a list of intervals, in file order.
func parseTableBody(name, body string) []interval {
	var out []interval
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		m := rowPattern.FindStringSubmatch(line)
		if m == nil {
			continue // page header/footer/blank/form-feed noise
		}
		lo, err := strconv.ParseInt(m[1], 16, 32)
		if err != nil {
			log.Fatalf("table %s: bad code point %q in line %q: %v", name, m[1], line, err)
		}
		hi := lo
		if m[2] != "" {
			hi, err = strconv.ParseInt(m[2], 16, 32)
			if err != nil {
				log.Fatalf("table %s: bad range end %q in line %q: %v", name, m[2], line, err)
			}
		}
		if hi < lo {
			log.Fatalf("table %s: range end before start in line %q", name, line)
		}
		out = append(out, interval{rune(lo), rune(hi)})
	}
	if len(out) == 0 {
		log.Fatalf("table %s: no rows parsed (delimiter or row format changed?)", name)
	}
	return out
}

// normalize sorts intervals and merges any that overlap or touch
// (hi+1 == next.lo), producing the compact, disjoint, ascending form the
// generated binary-search tables require. RFC 3454 tables are already
// sorted and disjoint in the source text; this is defensive, and also
// coalesces adjacent single-code-point rows (common in tables C.6/C.8/C.9)
// into fewer ranges.
func normalize(ivs []interval) []interval {
	sorted := append([]interval(nil), ivs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].lo < sorted[j].lo })

	out := sorted[:0:0]
	for _, iv := range sorted {
		if n := len(out); n > 0 && iv.lo <= out[n-1].hi+1 {
			if iv.hi > out[n-1].hi {
				out[n-1].hi = iv.hi
			}
			continue
		}
		out = append(out, iv)
	}
	return out
}

func main() {
	log.Printf("fetching %s", rfc3454URL)
	raw, err := fetch(rfc3454URL)
	if err != nil {
		log.Fatal(err)
	}
	text := string(raw)

	var buf bytes.Buffer
	buf.WriteString("// Code generated by gen/main.go. DO NOT EDIT.\n")
	buf.WriteString("// Source: RFC 3454 (\"Preparation of Internationalized Strings\n")
	buf.WriteString("// ('stringprep')\"), Unicode 3.2 -- as referenced by RFC 4013 (SASLprep):\n")
	buf.WriteString("//   https://www.rfc-editor.org/rfc/rfc3454.txt\n\n")
	buf.WriteString("package saslprep\n\n")
	buf.WriteString("// interval is a closed, inclusive code point range [lo, hi].\n")
	buf.WriteString("type interval struct {\n\tlo, hi int32\n}\n\n")

	for _, spec := range tableSpecs {
		body, err := extractTable(text, spec.rfcName)
		if err != nil {
			log.Fatal(err)
		}
		ivs := normalize(parseTableBody(spec.rfcName, body))
		log.Printf("table %-6s -> %-8s %4d intervals", spec.rfcName, spec.varName, len(ivs))

		fmt.Fprintf(&buf, "// %s\n", spec.doc)
		fmt.Fprintf(&buf, "var %s = [...]interval{", spec.varName)
		for i, iv := range ivs {
			if i%6 == 0 {
				buf.WriteString("\n\t")
			}
			fmt.Fprintf(&buf, "{0x%X, 0x%X}, ", iv.lo, iv.hi)
		}
		buf.WriteString("\n}\n\n")
	}

	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		log.Fatalf("gofmt: %v\n--- raw source ---\n%s", err, buf.String())
	}

	if err := os.WriteFile(outPath, formatted, 0o644); err != nil {
		log.Fatal(err)
	}
	log.Printf("wrote %s (%d bytes)", outPath, len(formatted))
}
