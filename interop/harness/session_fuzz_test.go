package harness

import (
	"bytes"
	"strings"
	"testing"
	"unicode"
)

func FuzzBoundedResponseLine(f *testing.F) {
	for _, seed := range [][]byte{
		[]byte("* OK ready\r\n"),
		[]byte("* CAPABILITY IMAP4rev1 IDLE\r\n"),
		[]byte("* 1 FETCH (BODY[] {5}\r\nhello)\r\n"),
		[]byte("broken\r"),
		[]byte{'*', ' ', 'O', 'K', ' ', 0, '\r', '\n'},
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		const limit = 64 << 10
		line, _ := readBoundedLine(bytes.NewReader(input), limit)
		if len(line) > limit {
			t.Fatalf("read %d bytes past limit %d", len(line), limit)
		}
	})
}

func FuzzTraceRedaction(f *testing.F) {
	f.Add("A001", "user@example.test", "password")
	f.Add("Z999", "a b", "quote\"slash\\")
	f.Fuzz(func(t *testing.T, tag, user, password string) {
		if tag == "" || strings.IndexFunc(tag, unicode.IsSpace) >= 0 || strings.ContainsAny(user+password, "\r\n") {
			t.Skip()
		}
		line := tag + " LOGIN " + quote(user) + " " + quote(password) + "\r\n"
		redacted := redactClientLine(line)
		want := tag + " LOGIN <redacted> <redacted>\r\n"
		if redacted != want {
			t.Fatalf("redacted line = %q, want %q", redacted, want)
		}
	})
}
