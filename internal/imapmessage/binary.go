package imapmessage

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"io"
	"mime/quotedprintable"
	"strings"

	"github.com/kiliant/go-imap"
)

// OpenBinarySection decodes one section's CTE with bounded buffers. Measuring
// and reopening avoids retaining a decoded body just to know its literal size.
//
// RFC 9051 section 6.4.5 lets an IMAP4rev2 client request BINARY only for leaf
// parts, but RFC 3516, which the BINARY capability advertises, does not
// restrict it. A multipart or message part is decoded by its own CTE, which
// RFC 2046 limits to 7bit, 8bit and binary, so its octets are returned as
// stored, nested encodings included.
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
	open := func() (io.Reader, error) {
		var r io.Reader = io.NewSectionReader(m.r, p.headers.bodyStart, p.end-p.headers.bodyStart)
		switch strings.ToLower(strings.TrimSpace(p.encoding)) {
		case "", "7bit", "8bit", "binary":
		case "base64":
			r = newBase64Reader(r)
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

// EncodeNULParts writes the message to w with every leaf part whose stored
// body holds a NUL octet under an identity transfer encoding (none, 7bit, 8bit
// or binary) re-encoded as base64. No IMAP string can carry NUL, so BODY could
// not transmit such a part at all. RFC 3516 section 4.4 lets a server change
// the CTE of appended binary data when nothing is lost; BODY and BODYSTRUCTURE
// then describe a form IMAP can transmit (section 6), and BINARY still returns
// the original octets. Every other byte, including header lines other than
// Content-Transfer-Encoding, is copied unchanged. It reports false and writes
// nothing when no part needs it.
func (m *Message) EncodeNULParts(w io.Writer) (bool, error) {
	targets, err := m.nulParts()
	if err != nil || len(targets) == 0 {
		return false, err
	}
	copyRange := func(start, end int64) error {
		_, err := io.Copy(w, io.NewSectionReader(m.r, start, end-start))
		return err
	}
	pos := int64(0)
	for _, p := range targets {
		h := p.headers
		if err := copyRange(pos, h.start); err != nil {
			return true, err
		}
		cursor := h.start
		headerEnd := h.end
		if h.blankStart >= 0 {
			headerEnd = h.blankStart
		}
		// Scan physical fields rather than the bounded semantic inventory:
		// every original CTE, including duplicates beyond its limits, must go,
		// and no continuation of another field may be removed with it.
		skipping := false
		copiedEnd := h.start
		copyHeaders := func(start, end int64) error {
			if end > start {
				copiedEnd = end
			}
			return copyRange(start, end)
		}
		err := scanLines(m.r, h.start, headerEnd, maxHeaderLineKeep, func(start, end int64, prefix []byte, _ bool) error {
			line := string(prefix)
			if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
				if skipping {
					cursor = end
				}
				return nil
			}
			name, _, found := strings.Cut(line, ":")
			skipping = found && strings.EqualFold(strings.TrimSpace(name), "Content-Transfer-Encoding")
			if skipping {
				if err := copyHeaders(cursor, start); err != nil {
					return err
				}
				cursor = end
			}
			return nil
		})
		if err != nil {
			return true, err
		}
		if err := copyHeaders(cursor, headerEnd); err != nil {
			return true, err
		}
		field := "Content-Transfer-Encoding: base64\r\n"
		if copiedEnd > h.start {
			var last [1]byte
			if _, err := m.r.ReadAt(last[:], copiedEnd-1); err != nil {
				return true, err
			}
			if last[0] != '\n' {
				field = "\r\n" + field
			}
		}
		blank := "\r\n"
		if h.blankStart >= 0 {
			blank = ""
		}
		if _, err := io.WriteString(w, field+blank); err != nil {
			return true, err
		}
		if h.blankStart >= 0 {
			if err := copyRange(h.blankStart, h.bodyStart); err != nil {
				return true, err
			}
		}
		encoder := base64.NewEncoder(base64.StdEncoding, &base64LineWriter{w: w})
		if _, err := io.Copy(encoder, io.NewSectionReader(m.r, h.bodyStart, p.end-h.bodyStart)); err != nil {
			return true, err
		}
		if err := encoder.Close(); err != nil {
			return true, err
		}
		// A part inside a multipart ends where the delimiter's own CRLF begins;
		// only the last part of the message needs a final line break.
		if p.end == m.size {
			if _, err := io.WriteString(w, "\r\n"); err != nil {
				return true, err
			}
		}
		pos = p.end
	}
	return true, copyRange(pos, m.size)
}

// nulParts lists, in message order, the leaf parts EncodeNULParts rewrites.
func (m *Message) nulParts() ([]*part, error) {
	var targets []*part
	var walk func(*part) error
	walk = func(p *part) error {
		switch {
		case p == nil:
		case strings.EqualFold(p.typeName, "multipart"):
			for _, child := range p.children {
				if err := walk(child); err != nil {
					return err
				}
			}
		case p.message != nil:
			return walk(p.message)
		default:
			switch strings.ToLower(strings.TrimSpace(p.encoding)) {
			case "", "7bit", "8bit", "binary":
				found, err := containsNUL(io.NewSectionReader(m.r, p.headers.bodyStart, p.end-p.headers.bodyStart))
				if err != nil {
					return err
				}
				if found {
					targets = append(targets, p)
				}
			}
		}
		return nil
	}
	return targets, walk(m.root)
}

func containsNUL(r io.Reader) (bool, error) {
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		if bytes.IndexByte(buf[:n], 0) >= 0 {
			return true, nil
		}
		if err == io.EOF {
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
}

// base64LineWriter breaks base64 output into the 76-character lines RFC 2045
// section 6.8 requires, without a line break after the last one.
type base64LineWriter struct {
	w      io.Writer
	column int
}

func (l *base64LineWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		if l.column == 76 {
			if _, err := io.WriteString(l.w, "\r\n"); err != nil {
				return written, err
			}
			l.column = 0
		}
		chunk := min(len(p), 76-l.column)
		n, err := l.w.Write(p[:chunk])
		written += n
		l.column += n
		if err != nil {
			return written, err
		}
		p = p[chunk:]
	}
	return written, nil
}
