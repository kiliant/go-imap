package imapmessage

import (
	"bytes"
	"fmt"
	"io"
	"strings"

	"github.com/kiliant/go-imap"
)

type segment struct {
	start, size int64
	data        []byte
}

// OpenBodySection returns a streaming reader and exact octet count for one
// BODY section request. The returned reader owns no copy of the message.
func (m *Message) OpenBodySection(item *imap.FetchItemBodySection) (io.Reader, int64, error) {
	if m == nil || m.root == nil || item == nil {
		return nil, 0, fmt.Errorf("imapmessage: invalid body section")
	}
	if len(item.HeaderFields) > 0 && len(item.HeaderFieldsNot) > 0 {
		return nil, 0, fmt.Errorf("body section cannot include both HEADER.FIELDS and HEADER.FIELDS.NOT")
	}
	if (len(item.HeaderFields) > 0 || len(item.HeaderFieldsNot) > 0) && item.Specifier != imap.PartSpecifierHeader {
		return nil, 0, fmt.Errorf("header field selection requires the HEADER specifier")
	}
	p, err := m.findPart(item.Part)
	if err != nil {
		return nil, 0, err
	}
	var spans []segment
	switch {
	case len(item.HeaderFields) > 0 || len(item.HeaderFieldsNot) > 0:
		headers := sectionHeaders(p, item)
		spans = filterHeaderSegments(headers, item.HeaderFields, item.HeaderFieldsNot)
	case item.Specifier == imap.PartSpecifierHeader:
		headers := sectionHeaders(p, item)
		spans = []segment{{start: headers.start, size: headers.end - headers.start}}
	case item.Specifier == imap.PartSpecifierMIME:
		if len(item.Part) == 0 {
			return nil, 0, fmt.Errorf("MIME section requires a part number")
		}
		spans = []segment{{start: p.headers.start, size: p.headers.end - p.headers.start}}
	case item.Specifier == imap.PartSpecifierText:
		target := p
		if p.message != nil {
			target = p.message
		}
		spans = []segment{{start: target.headers.bodyStart, size: target.end - target.headers.bodyStart}}
	case item.Specifier == imap.PartSpecifierNone:
		if len(item.Part) == 0 {
			spans = []segment{{start: m.root.start, size: m.root.end - m.root.start}}
		} else {
			spans = []segment{{start: p.headers.bodyStart, size: p.end - p.headers.bodyStart}}
		}
	default:
		return nil, 0, fmt.Errorf("unsupported body section specifier %q", item.Specifier)
	}
	if item.Partial != nil {
		if item.Partial.Offset < 0 || item.Partial.Size <= 0 {
			return nil, 0, fmt.Errorf("invalid partial range")
		}
		spans = sliceSegments(spans, item.Partial.Offset, item.Partial.Size)
	}
	return m.segmentReader(spans), segmentSize(spans), nil
}

func sectionHeaders(p *part, item *imap.FetchItemBodySection) headerBlock {
	if p.message != nil && item.Specifier == imap.PartSpecifierHeader {
		return p.message.headers
	}
	return p.headers
}

func filterHeaderSegments(headers headerBlock, fields, fieldsNot []string) []segment {
	wanted := make(map[string]struct{}, len(fields)+len(fieldsNot))
	for _, field := range append(append([]string(nil), fields...), fieldsNot...) {
		wanted[strings.ToLower(field)] = struct{}{}
	}
	includeNot := len(fieldsNot) > 0
	var spans []segment
	for _, field := range headers.fields {
		_, matched := wanted[strings.ToLower(field.Name)]
		if matched != includeNot {
			spans = append(spans, segment{start: field.Start, size: field.End - field.Start})
		}
	}
	if headers.blankStart >= 0 {
		spans = append(spans, segment{start: headers.blankStart, size: headers.end - headers.blankStart})
	} else {
		spans = append(spans, segment{data: []byte("\r\n"), size: 2})
	}
	return spans
}

func (m *Message) findPart(path []int) (*part, error) {
	p := m.root
	for i, number := range path {
		if number <= 0 {
			return nil, fmt.Errorf("body part numbers must be positive")
		}
		implicitSingle := i == 0
		if p.message != nil {
			p = p.message
			implicitSingle = true
		}
		if len(p.children) > 0 {
			if number > len(p.children) {
				return nil, fmt.Errorf("body part %v does not exist", path)
			}
			p = p.children[number-1]
		} else {
			// A non-multipart message body has the implicit part number 1.
			// This applies both at the top level and after descending through a
			// message/rfc822 part (for example BODY[2.1]).
			if number == 1 && implicitSingle {
				continue
			}
			return nil, fmt.Errorf("body part %v does not exist", path)
		}
	}
	return p, nil
}

func sliceSegments(src []segment, offset, count int64) []segment {
	if count <= 0 {
		return nil
	}
	var out []segment
	for _, span := range src {
		if offset >= span.size {
			offset -= span.size
			continue
		}
		span.start += offset
		if span.data != nil {
			span.data = span.data[offset:]
		}
		span.size -= offset
		offset = 0
		if span.size > count {
			span.size = count
			if span.data != nil {
				span.data = span.data[:count]
			}
		}
		out = append(out, span)
		count -= span.size
		if count == 0 {
			break
		}
	}
	return out
}

func segmentSize(spans []segment) int64 {
	var size int64
	for _, span := range spans {
		size += span.size
	}
	return size
}

func (m *Message) segmentReader(spans []segment) io.Reader {
	readers := make([]io.Reader, 0, len(spans))
	for _, span := range spans {
		if span.size == 0 {
			continue
		}
		if span.data != nil {
			readers = append(readers, bytes.NewReader(span.data[:span.size]))
		} else {
			readers = append(readers, io.NewSectionReader(m.r, span.start, span.size))
		}
	}
	return io.MultiReader(readers...)
}
