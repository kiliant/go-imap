package imapmessage

import (
	"bufio"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/kiliant/go-imap/internal/unicodenorm"
)

// asciiCasemap is how the default comparator matches text it cannot convert:
// invalid UTF-8, an unknown stored charset or a malformed transfer encoding's
// octets. RFC 3501 section 6.4.4 makes SEARCH case-insensitive, and a server
// without I18NLEVEL has no RFC 5255 licence to compare such text as octets, so
// it still folds ASCII. A named i;unicode-casemap applies RFC 5255 section
// 4.6 instead, comparing it with i;octet. The name is internal: MatchOptions
// refuses it.
const asciiCasemap = "i;ascii-casemap"

// fallbackComparator is the comparator for text that comparator cannot
// convert.
func fallbackComparator(comparator string) string {
	if comparator == "" || comparator == asciiCasemap {
		return asciiCasemap
	}
	return "i;octet"
}

func containsComparatorString(haystack, needle, comparator string) bool {
	switch {
	case comparator == "i;octet":
		return strings.Contains(haystack, needle)
	case comparator == asciiCasemap:
		return strings.Contains(asciiUpper(haystack), asciiUpper(needle))
	case utf8.ValidString(haystack) && utf8.ValidString(needle):
		return strings.Contains(unicodenorm.Casemap(haystack), unicodenorm.Casemap(needle))
	default:
		return containsComparatorString(haystack, needle, fallbackComparator(comparator))
	}
}

func asciiUpper(s string) string {
	b := []byte(s)
	for i, c := range b {
		b[i] = asciiUpperByte(c)
	}
	return string(b)
}

func asciiUpperByte(b byte) byte {
	if b >= 'a' && b <= 'z' {
		return b - ('a' - 'A')
	}
	return b
}

// Keep both prepared and original substring matches until UTF-8 validity is
// known. An invalid byte late in the input requires the whole string's binary
// fallback, including the already-read prefix. Neither matcher retains input.
func containsComparatorReader(reader io.Reader, needle, comparator string) (bool, error) {
	if needle == "" {
		return true, nil
	}
	// raw is the fallback matcher, used alone when the comparator or the
	// needle rules preparation out.
	fallback := fallbackComparator(comparator)
	if comparator == "i;octet" || comparator == asciiCasemap || !utf8.ValidString(needle) {
		comparator = fallback
	}
	raw := newSubstringMatcher(needle, fallback == asciiCasemap)
	if comparator == fallback {
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
	prepared := newSubstringMatcher(unicodenorm.Casemap(needle), false)
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
				_, _ = prepared.Write([]byte{asciiUpperByte(byte(r))})
			} else {
				_, _ = prepared.Write([]byte(unicodenorm.Casemap(string(r))))
			}
		}
		if raw.found && (!valid || prepared.found) {
			return true, nil
		}
	}
}

// substringMatcher is a streaming KMP search. With fold, ASCII letters match
// case-insensitively.
type substringMatcher struct {
	want    string
	fold    bool
	prefix  []int
	matched int
	found   bool
}

func newSubstringMatcher(want string, fold bool) *substringMatcher {
	if fold {
		want = asciiUpper(want)
	}
	m := &substringMatcher{want: want, fold: fold, prefix: make([]int, len(want))}
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
			if m.fold {
				b = asciiUpperByte(b)
			}
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
