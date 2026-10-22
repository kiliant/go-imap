package imapmessage

import (
	"bytes"
	"encoding/base64"
	"io"
	"strings"
	"testing"

	"github.com/kiliant/go-imap"
)

func TestBase64ReaderLenient(t *testing.T) {
	for _, tc := range []struct{ encoded, want string }{
		{"", ""},
		{"aGVs\r\nbG8gd29y\r\nbGQ=", "hello world"},
		// RFC 2045 section 6.8: characters outside the alphabet are ignored.
		{"  aGVs    \r\n bG8g  \r\n   d29y  \t \r\n    bGQ= \r\n", "hello world"},
		{"aGVsbG8gdG8gZXZlcnlvbmUh", "hello to everyone!"},
		{"aGk", "hi"},
		{"aGk=", "hi"},
		{"aGk=aGk=", "hi"},
		{"aGVsbA", "hell"},
		{"Q", ""},
		{"!!!", ""},
	} {
		got, err := io.ReadAll(newBase64Reader(strings.NewReader(tc.encoded)))
		if err != nil || string(got) != tc.want {
			t.Errorf("decode %q = %q, %v; want %q", tc.encoded, got, err, tc.want)
		}
	}
}

// FuzzBase64Reader decodes arbitrary input without failing, and matches
// encoding/base64 on canonical input wrapped at any width.
func FuzzBase64Reader(f *testing.F) {
	f.Add([]byte("hello world"), uint8(4))
	f.Add([]byte{0, 0xff, '\r', '\n'}, uint8(76))
	f.Add([]byte(""), uint8(1))
	f.Fuzz(func(t *testing.T, data []byte, width uint8) {
		if _, err := io.ReadAll(newBase64Reader(bytes.NewReader(data))); err != nil {
			t.Fatalf("arbitrary input: %v", err)
		}
		encoded := base64.StdEncoding.EncodeToString(data)
		if width > 0 {
			var wrapped strings.Builder
			for i := 0; i < len(encoded); i += int(width) {
				wrapped.WriteString(encoded[i:min(i+int(width), len(encoded))])
				wrapped.WriteString(" \r\n\t")
			}
			encoded = wrapped.String()
		}
		got, err := io.ReadAll(newBase64Reader(strings.NewReader(encoded)))
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("decode(encode(%x)) = %x, %v", data, got, err)
		}
	})
}

const binaryMultipartFixture = "From: user@domain.org\r\n" +
	"Content-Type: multipart/mixed; boundary=\"b\"\r\n" +
	"\r\n" +
	"--b\r\n" +
	"Content-Type: text/plain\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"\r\n" +
	"  aGVs \r\n bG8= \r\n" +
	"--b\r\n" +
	"Content-Type: message/rfc822\r\n" +
	"\r\n" +
	"Subject: inner\r\n" +
	"\r\n" +
	"inner body\r\n" +
	"--b--\r\n"

func openBinary(t *testing.T, m *Message, part ...int) (string, int64, error) {
	t.Helper()
	r, size, err := m.OpenBinarySection(&imap.FetchItemBinarySection{Part: part})
	if err != nil {
		return "", 0, err
	}
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(data)) != size {
		t.Fatalf("BINARY.SIZE %d disagrees with the %d octets returned", size, len(data))
	}
	return string(data), size, nil
}

// RFC 3516 does not restrict BINARY to leaf parts. A composite part's own CTE
// is an identity encoding, so its octets come back as stored; imaptest's
// fetch-binary-mime script expects exactly this.
func TestBinaryNonLeafSections(t *testing.T) {
	m, err := Analyze(strings.NewReader(binaryMultipartFixture), int64(len(binaryMultipartFixture)))
	if err != nil {
		t.Fatal(err)
	}
	whole, _, err := openBinary(t, m)
	if err != nil || whole != binaryMultipartFixture {
		t.Errorf("BINARY[] = %q, %v; want the stored message", whole, err)
	}
	inner, _, err := openBinary(t, m, 2)
	// The CRLF before a boundary belongs to the delimiter (RFC 2046 section
	// 5.1.1), so the part ends without it, as BODY[2] does.
	if err != nil || inner != "Subject: inner\r\n\r\ninner body" {
		t.Errorf("BINARY[2] = %q, %v; want the encapsulated message", inner, err)
	}
	leaf, _, err := openBinary(t, m, 1)
	if err != nil || leaf != "hello" {
		t.Errorf("BINARY[1] = %q, %v; want the leniently decoded text", leaf, err)
	}
}

func encodeNUL(raw []byte) ([]byte, bool, error) {
	m, err := Analyze(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, false, err
	}
	var out bytes.Buffer
	changed, err := m.EncodeNULParts(&out)
	if !changed {
		return raw, false, err
	}
	return out.Bytes(), true, err
}

// RFC 3516 section 4.4: a NUL-bearing part is re-encoded without loss. BINARY
// returns the original octets, BODY carries no NUL and BODYSTRUCTURE says
// base64, which is what section 6 requires BODY and BODYSTRUCTURE to agree on.
func TestEncodeNULParts(t *testing.T) {
	payload := "\x00-\xff\n\r\n"
	single := "From: user@domain.org\r\nContent-Type: application/octet-stream\r\nContent-Transfer-Encoding: binary\r\n\r\n" + payload
	nested := "Content-Type: multipart/mixed; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/plain\r\n\r\nplain\r\n" +
		"--b\r\nContent-Type: message/rfc822\r\n\r\n" +
		"Content-Type: application/octet-stream\r\nContent-transfer-encoding: 8bit\r\nX-Kept: yes\r\n\r\n" + payload +
		"\r\n--b--\r\n"
	for _, tc := range []struct {
		name, raw string
		path      []int
	}{
		{"single", single, nil},
		{"nested", nested, []int{2, 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			encoded, ok, err := encodeNUL([]byte(tc.raw))
			if err != nil || !ok {
				t.Fatalf("EncodeNULParts = %v, %v", ok, err)
			}
			if strings.Count(strings.ToLower(string(encoded)), "content-transfer-encoding") != 1 {
				t.Errorf("encoded headers keep a stale Content-Transfer-Encoding:\n%q", encoded)
			}
			m, err := Analyze(bytes.NewReader(encoded), int64(len(encoded)))
			if err != nil {
				t.Fatal(err)
			}
			part := tc.path
			if part == nil {
				part = []int{1}
			}
			if got, _, err := openBinary(t, m, part...); err != nil || got != payload {
				t.Errorf("BINARY%v = %q, %v; want the original octets", part, got, err)
			}
			body, _, err := m.OpenBodySection(&imap.FetchItemBodySection{Part: part})
			if err != nil {
				t.Fatal(err)
			}
			if data, _ := io.ReadAll(body); bytes.IndexByte(data, 0) >= 0 {
				t.Errorf("BODY%v still carries NUL: %q", part, data)
			}
			if again, ok, _ := encodeNUL(encoded); ok || !bytes.Equal(again, encoded) {
				t.Error("a second pass changed the message again")
			}
			if tc.name == "nested" && (!strings.Contains(string(encoded), "X-Kept: yes\r\n") || !strings.Contains(string(encoded), "plain\r\n")) {
				t.Errorf("unrelated header or part changed:\n%q", encoded)
			}
		})
	}
	for _, raw := range []string{
		"Subject: plain\r\n\r\nbody\r\n",
		// NUL inside base64 is decoded data, which BODY never sends.
		"Content-Transfer-Encoding: base64\r\n\r\nAC3/\r\n",
	} {
		if got, ok, err := encodeNUL([]byte(raw)); err != nil || ok || string(got) != raw {
			t.Errorf("EncodeNULParts(%q) = %q, %v, %v; want it untouched", raw, got, ok, err)
		}
	}
}

// FuzzEncodeNULParts never fails on arbitrary input, and one pass leaves no
// identity-encoded part with NUL, so a second pass changes nothing.
func FuzzEncodeNULParts(f *testing.F) {
	f.Add([]byte("Content-Transfer-Encoding: binary\r\n\r\n\x00"))
	f.Add([]byte("Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\n\r\n\x00\r\n--b--\r\n"))
	f.Add([]byte("\x00"))
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 1<<16 {
			t.Skip()
		}
		encoded, _, err := encodeNUL(raw)
		if err != nil {
			return
		}
		if _, ok, err := encodeNUL(encoded); err != nil || ok {
			t.Fatalf("second pass = %v, %v for %q", ok, err, encoded)
		}
	})
}

func TestEncodeNULPartsWrapsLines(t *testing.T) {
	payload := strings.Repeat("\x00\x01\x02 binary payload ", 40)
	raw := "Content-Type: application/octet-stream\r\n\r\n" + payload
	encoded, ok, err := encodeNUL([]byte(raw))
	if err != nil || !ok {
		t.Fatalf("encode = %v, %v", ok, err)
	}
	_, body, _ := strings.Cut(string(encoded), "\r\n\r\n")
	for _, line := range strings.Split(strings.TrimSuffix(body, "\r\n"), "\r\n") {
		if len(line) == 0 || len(line) > 76 {
			t.Fatalf("base64 line of %d characters", len(line))
		}
	}
	m, err := Analyze(bytes.NewReader(encoded), int64(len(encoded)))
	if err != nil {
		t.Fatal(err)
	}
	if got, _, err := openBinary(t, m, 1); err != nil || got != payload {
		t.Errorf("round trip lost data: %v", err)
	}
}
