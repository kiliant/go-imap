package harness

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/kiliant/go-imap/interop/definition"
)

// Fixture is a repeatable message used to seed every server.
type Fixture struct {
	Name  string
	Flags []string
	Size  int64
	Open  func() io.Reader
}

const largeBodySize = 5 << 20

// Fixtures returns the ten-message canonical corpus. Open may be called more
// than once and always returns a reader at offset zero.
func Fixtures() []Fixture {
	fixtures := []Fixture{
		textFixture("plain", nil, "Subject: plain\r\nFrom: sender@example.test\r\nTo: interop@example.test\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nhello from the interoperability suite\r\n"),
		textFixture("alternative", nil, "Subject: alternative\r\nFrom: sender@example.test\r\nTo: interop@example.test\r\nContent-Type: multipart/alternative; boundary=alt\r\n\r\n--alt\r\nContent-Type: text/plain\r\n\r\nplain\r\n--alt\r\nContent-Type: text/html\r\n\r\n<p>html</p>\r\n--alt--\r\n"),
		textFixture("attachment", nil, "Subject: attachment\r\nFrom: sender@example.test\r\nTo: interop@example.test\r\nContent-Type: multipart/mixed; boundary=mixed\r\n\r\n--mixed\r\nContent-Type: text/plain\r\n\r\nbody\r\n--mixed\r\nContent-Type: application/octet-stream; name=data.bin\r\nContent-Disposition: attachment; filename=data.bin\r\nContent-Transfer-Encoding: base64\r\n\r\nAAECAwQ=\r\n--mixed--\r\n"),
		textFixture("encoded-header", nil, "Subject: =?UTF-8?Q?Gr=C3=BC=C3=9Fe?=\r\nFrom: =?ISO-8859-1?Q?J=F6rg?= <sender@example.test>\r\nTo: interop@example.test\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nheader decoding fixture\r\n"),
		largeFixture(),
		textFixture("seen", []string{"\\Seen"}, "Subject: seen\r\nFrom: sender@example.test\r\nTo: interop@example.test\r\n\r\nthis message starts seen\r\n"),
		textFixture("flagged", []string{"\\Flagged"}, "Subject: flagged\r\nFrom: sender@example.test\r\nTo: interop@example.test\r\n\r\nthis message starts flagged\r\n"),
		textFixture("nested", nil, "Subject: nested\r\nFrom: sender@example.test\r\nTo: interop@example.test\r\nContent-Type: message/rfc822\r\n\r\nSubject: inner\r\nFrom: inner@example.test\r\nTo: interop@example.test\r\n\r\ninner body\r\n"),
		textFixture("empty-body", nil, "Subject: empty body\r\nFrom: sender@example.test\r\nTo: interop@example.test\r\n\r\n"),
		textFixture("eight-bit-body", nil, "Subject: UTF-8 body\r\nFrom: sender@example.test\r\nTo: interop@example.test\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\nGrüße aus Berlin\r\n"),
	}
	return fixtures
}

func textFixture(name string, flags []string, message string) Fixture {
	data := []byte(message)
	return Fixture{
		Name:  name,
		Flags: append([]string(nil), flags...),
		Size:  int64(len(data)),
		Open: func() io.Reader {
			return bytes.NewReader(data)
		},
	}
}

func largeFixture() Fixture {
	header := []byte("Subject: five megabyte streaming fixture\r\nFrom: sender@example.test\r\nTo: interop@example.test\r\nContent-Type: application/octet-stream\r\n\r\n")
	return Fixture{
		Name: "large-streaming",
		Size: int64(len(header) + largeBodySize),
		Open: func() io.Reader {
			return io.MultiReader(bytes.NewReader(header), io.LimitReader(repeatingByte('x'), largeBodySize))
		},
	}
}

type byteReader byte

func repeatingByte(value byte) io.Reader { return byteReader(value) }

func (r byteReader) Read(dst []byte) (int, error) {
	for i := range dst {
		dst[i] = byte(r)
	}
	return len(dst), nil
}

// Seed installs the canonical mailbox state over IMAP APPEND.
func Seed(ctx context.Context, session *Session, profile definition.Profile) error {
	for _, mailbox := range []string{"Archive", "Sent", "T&AOs-st"} { // "Tëst" in modified UTF-7.
		wireName := profile.MailboxPrefix + mailbox
		if err := session.Create(ctx, wireName); err != nil && !strings.Contains(strings.ToLower(err.Error()), "already exist") {
			// James's demo image auto-provisions a standard mailbox set
			// (including Archive and Sent) on first login, before Seed ever
			// runs. A mailbox that already exists is exactly the state this
			// loop wants, so that specific failure is not fatal.
			return fmt.Errorf("create fixture mailbox %s: %w", mailbox, err)
		}
	}
	for _, fixture := range Fixtures() {
		if err := session.Append(ctx, "INBOX", fixture.Flags, fixture.Size, fixture.Open()); err != nil {
			return fmt.Errorf("append fixture %s: %w", fixture.Name, err)
		}
	}
	return nil
}

// FixtureSummary is suitable for diagnostics and test output.
func FixtureSummary() string {
	var b strings.Builder
	for _, fixture := range Fixtures() {
		fmt.Fprintf(&b, "%s=%d ", fixture.Name, fixture.Size)
	}
	return strings.TrimSpace(b.String())
}
