package imapmessage

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kiliant/go-imap/internal/unicodenorm"
)

func TestComparatorSubstring(t *testing.T) {
	for _, tc := range []struct {
		haystack, needle, comparator string
		want                         bool
	}{
		{"body Å body", "a\u030a", "", true},
		{"body Å body", "å", "i;unicode-casemap", true},
		{"body Å body", "å", "i;octet", false},
		{"a" + strings.Repeat("x", 40000) + "\xff", "A", "", false},
		{"a" + strings.Repeat("x", 40000) + "\xff", "a", "", true},
		{"b\xffdy", "\xff", "", true},
		{"repeated repeated", "repeated", "i;octet", true},
		{"", "", "", true},
	} {
		got, err := containsComparatorReader(strings.NewReader(tc.haystack), tc.needle, tc.comparator)
		if err != nil || got != tc.want {
			t.Errorf("stream %q in %q (%s) = %v, %v", tc.needle, tc.haystack, tc.comparator, got, err)
		}
		if got := containsComparatorString(tc.haystack, tc.needle, tc.comparator); got != tc.want {
			t.Errorf("string %q in %q (%s) = %v", tc.needle, tc.haystack, tc.comparator, got)
		}
	}
}

func FuzzComparatorSubstring(f *testing.F) {
	f.Add([]byte("body Å body"), []byte("a\u030a"))
	f.Add([]byte("a\xff"), []byte("A"))
	f.Add([]byte("ababababa"), []byte("ababa"))
	f.Fuzz(func(t *testing.T, haystack, needle []byte) {
		if len(haystack) > 1<<16 || len(needle) > 1<<12 {
			t.Skip()
		}
		for _, comparator := range []string{"i;unicode-casemap", "i;octet"} {
			h, n := string(haystack), string(needle)
			if comparator != "i;octet" && utf8.Valid(haystack) && utf8.Valid(needle) {
				h, n = unicodenorm.Casemap(h), unicodenorm.Casemap(n)
			}
			want := strings.Contains(h, n)
			got, err := containsComparatorReader(strings.NewReader(string(haystack)), string(needle), comparator)
			if err != nil || got != want {
				t.Fatalf("%s substring = %v, %v, want %v", comparator, got, err, want)
			}
		}
	})
}
