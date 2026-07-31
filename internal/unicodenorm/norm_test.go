package unicodenorm

import (
	"bufio"
	"compress/gzip"
	"os"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

// TestUnicodeVersionMatchesTables guards against a silent skew between the
// Go toolchain's Unicode data and the UCD version tables.go was generated
// from. If this fails, the Go toolchain has moved to a new Unicode version:
// internal/unicodenorm/tables.go must be regenerated (see
// internal/unicodenorm/gen/main.go) against the matching UCD release, and
// the full conformance suite (TestNFCConformance) re-run and reviewed
// before the new tables are committed.
func TestUnicodeVersionMatchesTables(t *testing.T) {
	const wantVersion = "15.0.0"
	if unicode.Version != wantVersion {
		t.Fatalf(
			"unicode.Version = %q, but internal/unicodenorm/tables.go was generated from UCD %s; "+
				"the Go toolchain's Unicode version has moved. Regenerate tables.go by running "+
				"`go run internal/unicodenorm/gen/main.go` against the UCD release matching "+
				"unicode.Version (%s), then re-run the full conformance suites "+
				"(TestNFCConformance and TestNFKCConformance) before committing the new tables.",
			unicode.Version, wantVersion, unicode.Version)
	}
}

// TestCompatDecompose is a small sanity check on the fully expanded
// compatibility decomposition table that NFKC's decomposition step (see
// compatDecompose, decomposeCompatString and NFKC in norm.go) reads. It is
// not part of the NFC conformance guarantee; NFKC conformance is exercised
// separately by TestNFKCConformance.
func TestCompatDecompose(t *testing.T) {
	cases := []struct {
		name string
		in   rune
		want []rune
	}{
		{"superscript two -> digit two (<super>)", '²', []rune{'2'}},
		{"micro sign -> greek mu (<compat>)", 'µ', []rune{'μ'}},
		{"vulgar fraction one half (<fraction>)", '½', []rune{'1', '⁄', '2'}},
		{"ASCII letter has no compatibility decomposition", 'A', nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := compatDecompose(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("compatDecompose(%q) = %q, want %q", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("compatDecompose(%q) = %q, want %q", tc.in, got, tc.want)
				}
			}
		})
	}
}

func TestNFCBasic(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty string", "", ""},
		{"ASCII passthrough", "Hello, World! 123", "Hello, World! 123"},
		{
			"e + combining acute composes to precomposed e-acute",
			"é",
			"é",
		},
		{
			"already precomposed e-acute is unchanged",
			"é",
			"é",
		},
		{
			"already-normalized string is unchanged",
			"café naïve résumé",
			"café naïve résumé",
		},
		{
			"Hangul decomposed jamo compose to precomposed syllable",
			"가", // L(g) + V(a) -> GA
			"가",
		},
		{
			"Hangul precomposed syllable round-trips",
			"가",
			"가",
		},
		{
			"Hangul LVT jamo sequence composes",
			"각", // g + a + g -> GAG
			"각",
		},
		{
			"multiple combining marks reorder and compose where possible",
			"Á̈", // A + acute + diaeresis: acute composes with A
			"Á̈",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NFC(tc.in)
			if got != tc.want {
				t.Errorf("NFC(%q) = %q (% x), want %q (% x)", tc.in, got, []rune(got), tc.want, []rune(tc.want))
			}
		})
	}
}

func TestNFCIdempotent(t *testing.T) {
	// NFC must be idempotent: normalizing twice gives the same result as
	// normalizing once.
	inputs := []string{
		"é",
		"é",
		"각",
		"Hello, World!",
		"Á̈",
	}
	for _, in := range inputs {
		once := NFC(in)
		twice := NFC(once)
		if once != twice {
			t.Errorf("NFC not idempotent for %q: NFC=%q, NFC(NFC)=%q", in, once, twice)
		}
	}
}

// parseCodePoints parses a space-separated list of hex code points (as used
// in NormalizationTest.txt) into a string.
func parseCodePoints(t *testing.T, field string) string {
	t.Helper()
	var sb strings.Builder
	for _, tok := range strings.Fields(field) {
		v, err := strconv.ParseUint(tok, 16, 32)
		if err != nil {
			t.Fatalf("bad code point %q in field %q: %v", tok, field, err)
		}
		sb.WriteRune(rune(v))
	}
	return sb.String()
}

// TestNFCConformance runs the official Unicode NormalizationTest.txt suite
// (version 15.0.0) and checks the NFC invariants documented in the file's
// own header:
//
//	source; NFC; NFD; NFKC; NFKD
//
//	NFC(source) == NFC(NFC_col) == NFC(NFD_col) == NFC_col
func TestNFCConformance(t *testing.T) {
	f, err := os.Open("testdata/NormalizationTest.txt.gz")
	if err != nil {
		t.Fatalf("open testdata: %v", err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer gz.Close()

	scanner := bufio.NewScanner(gz)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	count := 0
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "@") {
			continue
		}
		// Strip trailing comment, if any.
		if idx := strings.Index(line, "#"); idx >= 0 {
			line = line[:idx]
		}
		fields := strings.Split(line, ";")
		if len(fields) < 5 {
			t.Fatalf("line %d: expected at least 5 fields, got %d: %q", lineNo, len(fields), line)
		}

		source := parseCodePoints(t, fields[0])
		nfc := parseCodePoints(t, fields[1])
		nfd := parseCodePoints(t, fields[2])

		if got := NFC(source); got != nfc {
			t.Errorf("line %d: NFC(source) mismatch:\n  source = %q (% x)\n  got    = %q (% x)\n  want   = %q (% x)",
				lineNo, source, []rune(source), got, []rune(got), nfc, []rune(nfc))
			continue
		}
		if got := NFC(nfc); got != nfc {
			t.Errorf("line %d: NFC(NFC_col) mismatch:\n  got  = %q (% x)\n  want = %q (% x)", lineNo, got, []rune(got), nfc, []rune(nfc))
			continue
		}
		if got := NFC(nfd); got != nfc {
			t.Errorf("line %d: NFC(NFD_col) mismatch:\n  nfd  = %q (% x)\n  got  = %q (% x)\n  want = %q (% x)",
				lineNo, nfd, []rune(nfd), got, []rune(got), nfc, []rune(nfc))
			continue
		}
		count++
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanning testdata: %v", err)
	}
	if count == 0 {
		t.Fatal("no conformance cases were exercised")
	}
	t.Logf("exercised %d NFC conformance cases from NormalizationTest.txt", count)
}

// TestNFKCConformance runs the official Unicode NormalizationTest.txt suite
// (version 15.0.0) and checks the NFKC invariant documented in the file's
// own header:
//
//	source; NFC; NFD; NFKC; NFKD
//
//	NFKC(source) == NFKC(NFC) == NFKC(NFD) == NFKC(NFKC) == NFKC(NFKD) == NFKC
//
// This exercises the same lines as TestNFCConformance (no line is skipped),
// since every non-comment, non-@ line in the file carries all five columns.
func TestNFKCConformance(t *testing.T) {
	f, err := os.Open("testdata/NormalizationTest.txt.gz")
	if err != nil {
		t.Fatalf("open testdata: %v", err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer gz.Close()

	scanner := bufio.NewScanner(gz)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	count := 0
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "@") {
			continue
		}
		// Strip trailing comment, if any.
		if idx := strings.Index(line, "#"); idx >= 0 {
			line = line[:idx]
		}
		fields := strings.Split(line, ";")
		if len(fields) < 5 {
			t.Fatalf("line %d: expected at least 5 fields, got %d: %q", lineNo, len(fields), line)
		}

		source := parseCodePoints(t, fields[0])
		nfc := parseCodePoints(t, fields[1])
		nfd := parseCodePoints(t, fields[2])
		nfkc := parseCodePoints(t, fields[3])
		nfkd := parseCodePoints(t, fields[4])

		checks := []struct {
			name string
			in   string
		}{
			{"source", source},
			{"NFC_col", nfc},
			{"NFD_col", nfd},
			{"NFKC_col", nfkc},
			{"NFKD_col", nfkd},
		}
		ok := true
		for _, c := range checks {
			if got := NFKC(c.in); got != nfkc {
				t.Errorf("line %d: NFKC(%s) mismatch:\n  in   = %q (% x)\n  got  = %q (% x)\n  want = %q (% x)",
					lineNo, c.name, c.in, []rune(c.in), got, []rune(got), nfkc, []rune(nfkc))
				ok = false
			}
		}
		if !ok {
			continue
		}
		count++
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanning testdata: %v", err)
	}
	if count == 0 {
		t.Fatal("no conformance cases were exercised")
	}
	t.Logf("exercised %d NFKC conformance cases from NormalizationTest.txt", count)
}
