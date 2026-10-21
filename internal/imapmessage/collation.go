package imapmessage

import (
	"bufio"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/kiliant/go-imap/internal/unicodenorm"
)

func containsComparatorString(haystack, needle, comparator string) bool {
	if comparator == "i;octet" || !utf8.ValidString(haystack) || !utf8.ValidString(needle) {
		return strings.Contains(haystack, needle)
	}
	return strings.Contains(unicodenorm.Casemap(haystack), unicodenorm.Casemap(needle))
}

// Keep both prepared and original substring matches until UTF-8 validity is
// known. An invalid byte late in the input requires the whole string's binary
// fallback, including the already-read prefix. Neither matcher retains input.
func containsComparatorReader(reader io.Reader, needle, comparator string) (bool, error) {
	if needle == "" {
		return true, nil
	}
	raw := newSubstringMatcher(needle)
	if comparator == "i;octet" || !utf8.ValidString(needle) {
		buffer := make([]byte, 32<<10)
		for {
			n, err := reader.Read(buffer)
			_, _ = raw.Write(buffer[:n])
			if raw.found || err == io.EOF {
				return raw.found, nil
			}
			if err != nil {
				return false, err
			}
		}
	}
	prepared := newSubstringMatcher(unicodenorm.Casemap(needle))
	br := bufio.NewReaderSize(io.TeeReader(reader, raw), 32<<10)
	valid := true
	for {
		r, width, err := br.ReadRune()
		if err == io.EOF {
			if valid {
				return prepared.found, nil
			}
			return raw.found, nil
		}
		if err != nil {
			return false, err
		}
		if r == utf8.RuneError && width == 1 {
			valid = false
		}
		if valid {
			if r < utf8.RuneSelf {
				b := byte(r)
				if b >= 'a' && b <= 'z' {
					b -= 'a' - 'A'
				}
				_, _ = prepared.Write([]byte{b})
			} else {
				_, _ = prepared.Write([]byte(unicodenorm.Casemap(string(r))))
			}
		}
		if raw.found && (!valid || prepared.found) {
			return true, nil
		}
	}
}

type substringMatcher struct {
	want    string
	prefix  []int
	matched int
	found   bool
}

func newSubstringMatcher(want string) *substringMatcher {
	m := &substringMatcher{want: want, prefix: make([]int, len(want))}
	for i, j := 1, 0; i < len(want); i++ {
		for j > 0 && want[i] != want[j] {
			j = m.prefix[j-1]
		}
		if want[i] == want[j] {
			j++
		}
		m.prefix[i] = j
	}
	return m
}

func (m *substringMatcher) Write(p []byte) (int, error) {
	if !m.found {
		for _, b := range p {
			for m.matched > 0 && b != m.want[m.matched] {
				m.matched = m.prefix[m.matched-1]
			}
			if b == m.want[m.matched] {
				m.matched++
				if m.matched == len(m.want) {
					m.found = true
					break
				}
			}
		}
	}
	return len(p), nil
}
