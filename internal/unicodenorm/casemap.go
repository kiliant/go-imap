package unicodenorm

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Casemap prepares a known UTF-8 string for i;unicode-casemap (RFC 5051).
// Titlecasing precedes recursive compatibility decomposition of each code
// point; the resulting sequence is not recomposed or reordered. Invalid UTF-8
// retains its original octets, as the comparator's binary fallback requires.
func Casemap(s string) string {
	if !utf8.ValidString(s) {
		return s
	}
	var out strings.Builder
	for _, r := range s {
		r = unicode.ToTitle(r)
		if expanded := compatDecompose(r); expanded != nil {
			for _, c := range expanded {
				out.WriteRune(c)
			}
		} else {
			out.WriteRune(r)
		}
	}
	return out.String()
}
