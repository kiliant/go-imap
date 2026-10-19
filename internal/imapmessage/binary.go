package imapmessage

import (
	"bufio"
	"encoding/base64"
	"io"
	"mime/quotedprintable"
	"strings"

	"github.com/kiliant/go-imap"
)

// OpenBinarySection decodes one leaf body's CTE with bounded buffers. Measuring
// and reopening avoids retaining a decoded body just to know its literal size.
func (m *Message) OpenBinarySection(item *imap.FetchItemBinarySection) (io.Reader, int64, error) {
	if m == nil || m.root == nil || item == nil {
		return nil, 0, binaryError(imap.CodeCannot, "invalid BINARY section")
	}
	if item.Partial != nil && (item.Partial.Offset < 0 || item.Partial.Size <= 0) {
		return nil, 0, binaryError(imap.CodeCannot, "invalid BINARY partial range")
	}
	p, err := m.findPart(item.Part)
	if err != nil {
		return nil, 0, binaryError(imap.CodeCannot, "BINARY body part does not exist")
	}
	if strings.EqualFold(p.typeName, "multipart") || strings.EqualFold(p.typeName, "message") &&
		(strings.EqualFold(p.subtype, "rfc822") || strings.EqualFold(p.subtype, "global")) {
		return nil, 0, binaryError(imap.CodeCannot, "BINARY requires a leaf body part")
	}
	open := func() (io.Reader, error) {
		var r io.Reader = io.NewSectionReader(m.r, p.headers.bodyStart, p.end-p.headers.bodyStart)
		switch strings.ToLower(strings.TrimSpace(p.encoding)) {
		case "", "7bit", "8bit", "binary":
		case "base64":
			r = base64.NewDecoder(base64.StdEncoding, r)
		case "quoted-printable":
			r = quotedprintable.NewReader(r)
		default:
			return nil, binaryError(imap.CodeUnknownCTE, "cannot decode this content-transfer-encoding")
		}
		if strings.EqualFold(p.typeName, "text") {
			r = &binaryTextReader{reader: bufio.NewReader(r)}
		}
		if len(item.Part) == 0 {
			// An empty section includes the stored headers. CTE decoding
			// applies only to the body; encoded header words stay untouched.
			r = io.MultiReader(io.NewSectionReader(m.r, p.start, p.headers.bodyStart-p.start), r)
		}
		return r, nil
	}
	r, err := open()
	if err != nil {
		return nil, 0, err
	}
	size, err := io.Copy(io.Discard, r)
	if err != nil {
		return nil, 0, binaryError(imap.CodeUnknownCTE, "cannot decode body content")
	}
	r, err = open()
	if err != nil {
		return nil, 0, err
	}
	if item.Partial != nil {
		offset := min(item.Partial.Offset, size)
		if _, err := io.CopyN(io.Discard, r, offset); err != nil {
			return nil, 0, err
		}
		size = min(size-offset, item.Partial.Size)
		r = io.LimitReader(r, size)
	}
	return r, size, nil
}

func binaryError(code imap.ResponseCode, text string) error {
	return &imap.Error{Type: imap.ErrorTypeNo, Code: code, Text: text}
}

// binaryTextReader canonicalises text line endings after CTE decoding. Pending
// state also works when the caller reads one byte at a time or ends on a CR.
type binaryTextReader struct {
	reader    *bufio.Reader
	pendingLF bool
	skipLF    bool
}

func (r *binaryTextReader) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		if r.pendingLF {
			p[n], r.pendingLF = '\n', false
			n++
			continue
		}
		b, err := r.reader.ReadByte()
		if err != nil {
			return n, err
		}
		if r.skipLF {
			r.skipLF = false
			if b == '\n' {
				continue
			}
		}
		switch b {
		case '\r', '\n':
			p[n], r.pendingLF = '\r', true
			r.skipLF = b == '\r'
		default:
			p[n] = b
		}
		n++
	}
	return n, nil
}
