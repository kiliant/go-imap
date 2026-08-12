// Package imapmessage analyses stored RFC 5322 messages for the server side.
// It generates IMAP ENVELOPE and BODYSTRUCTURE values, exposes byte-exact body
// sections, and evaluates SEARCH criteria without imposing storage policy on a
// backend.
package imapmessage

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"math"
	"mime"
	"net/mail"
	"strings"
	"time"

	"github.com/kiliant/go-imap"
)

var errStopScan = fmt.Errorf("imapmessage: stop scan")

const (
	maxMIMEDepth      = 64
	maxMIMEParts      = 10_000
	maxHeaderFields   = 10_000
	maxHeaderBytes    = 8 << 20
	maxHeaderLineKeep = 1 << 20
)

// Message is a parsed view over immutable RFC 5322 bytes. Analysis retains the
// ReaderAt rather than the message itself, so opening BODY[] remains streaming
// even for very large messages.
type Message struct {
	r     io.ReaderAt
	size  int64
	root  *part
	parts int

	Envelope      *imap.Envelope
	BodyStructure imap.BodyStructure
}

// Analyze parses an immutable message of exactly size octets. Malformed MIME
// and header syntax is represented best-effort; only an underlying I/O failure
// prevents analysis.
func Analyze(r io.ReaderAt, size int64) (*Message, error) {
	if r == nil || size < 0 {
		return nil, fmt.Errorf("imapmessage: invalid message source")
	}
	m := &Message{r: r, size: size}
	root, err := m.parsePart(0, size, 0)
	if err != nil {
		return nil, err
	}
	m.root = root
	m.Envelope = envelopeFromHeaders(root.headers)
	m.BodyStructure = m.bodyStructure(root)
	return m, nil
}

// Size returns the message size in octets.
func (m *Message) Size() int64 {
	if m == nil {
		return 0
	}
	return m.size
}

type headerField struct {
	Name       string
	Value      string
	Start, End int64
}

type headerBlock struct {
	fields                []headerField
	start, end, bodyStart int64
	blankStart            int64
}

func (h headerBlock) value(name string) string {
	for _, field := range h.fields {
		if strings.EqualFold(field.Name, name) {
			return field.Value
		}
	}
	return ""
}

func (h headerBlock) values(name string) []string {
	var values []string
	for _, field := range h.fields {
		if strings.EqualFold(field.Name, name) {
			values = append(values, field.Value)
		}
	}
	return values
}

type part struct {
	start, end int64
	headers    headerBlock

	mediaType string
	typeName  string
	subtype   string
	params    map[string]string
	encoding  string

	children []*part
	message  *part
}

func (m *Message) parsePart(start, end int64, depth int) (*part, error) {
	if depth > maxMIMEDepth || m.parts >= maxMIMEParts {
		return &part{
			start: start, end: end,
			headers:   headerBlock{start: start, end: start, bodyStart: start, blankStart: -1},
			typeName:  "application",
			subtype:   "octet-stream",
			mediaType: "application/octet-stream",
		}, nil
	}
	m.parts++
	headers, err := parseHeaders(m.r, start, end)
	if err != nil {
		return nil, err
	}
	p := &part{start: start, end: end, headers: headers}
	p.mediaType, p.params = parseContentType(headers.value("Content-Type"))
	p.typeName, p.subtype, _ = strings.Cut(p.mediaType, "/")
	p.encoding = strings.TrimSpace(headers.value("Content-Transfer-Encoding"))
	if p.encoding == "" {
		p.encoding = "7BIT"
	}

	switch {
	case strings.EqualFold(p.typeName, "multipart"):
		boundary := p.params["boundary"]
		if boundary != "" {
			children, err := m.multipartChildren(p, boundary, depth+1)
			if err != nil {
				return nil, err
			}
			p.children = children
		}
	case strings.EqualFold(p.typeName, "message") && strings.EqualFold(p.subtype, "rfc822"):
		// Even an empty or header-only message/rfc822 part needs the three
		// mandatory body-type-msg fields. Parsing its zero-length body produces
		// a valid best-effort empty text/plain message instead of making the
		// stored outer message unfetchable.
		p.message, err = m.parsePart(p.headers.bodyStart, p.end, depth+1)
		if err != nil {
			return nil, err
		}
	}
	return p, nil
}

func parseContentType(raw string) (string, map[string]string) {
	if strings.TrimSpace(raw) == "" {
		return "text/plain", nil
	}
	mediaType, params, err := mime.ParseMediaType(raw)
	if err == nil && strings.Contains(mediaType, "/") {
		out := make(map[string]string, len(params))
		for key, value := range params {
			out[cleanHeaderValue(strings.ToLower(key))] = cleanHeaderValue(value)
		}
		return strings.ToLower(mediaType), out
	}
	base, _, _ := strings.Cut(raw, ";")
	base = strings.ToLower(strings.TrimSpace(base))
	if !strings.Contains(base, "/") {
		return "application/octet-stream", nil
	}
	return base, nil
}

func (m *Message) multipartChildren(parent *part, boundary string, depth int) ([]*part, error) {
	delim := "--" + boundary
	var (
		children   []*part
		childStart int64 = -1
		closed           = false
	)
	err := scanLines(m.r, parent.headers.bodyStart, parent.end, len(delim)+8, func(start, end int64, prefix []byte, endedLF bool) error {
		if closed {
			return nil
		}
		if m.parts >= maxMIMEParts {
			closed = true
			childStart = -1
			return nil
		}
		line := strings.TrimRight(string(prefix), "\r\n")
		trimmed := strings.TrimRight(line, " \t")
		if trimmed != delim && trimmed != delim+"--" {
			return nil
		}
		if childStart >= 0 {
			childEnd := stripBoundaryCRLF(m.r, start, childStart)
			child, err := m.parsePart(childStart, childEnd, depth)
			if err != nil {
				return err
			}
			children = append(children, child)
		}
		if trimmed == delim+"--" {
			closed = true
			childStart = -1
		} else {
			childStart = end
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if childStart >= 0 {
		child, err := m.parsePart(childStart, parent.end, depth)
		if err != nil {
			return nil, err
		}
		children = append(children, child)
	}
	return children, nil
}

func stripBoundaryCRLF(r io.ReaderAt, boundaryStart, floor int64) int64 {
	end := boundaryStart
	var b [2]byte
	if end-floor >= 2 {
		if n, _ := r.ReadAt(b[:], end-2); n == 2 && b == [2]byte{'\r', '\n'} {
			return end - 2
		}
	}
	if end-floor >= 1 {
		if n, _ := r.ReadAt(b[:1], end-1); n == 1 && b[0] == '\n' {
			return end - 1
		}
	}
	return end
}

func parseHeaders(r io.ReaderAt, start, end int64) (headerBlock, error) {
	h := headerBlock{start: start, end: start, bodyStart: start, blankStart: -1}
	var kept int
	err := scanLines(r, start, end, maxHeaderLineKeep, func(lineStart, lineEnd int64, prefix []byte, endedLF bool) error {
		if h.blankStart >= 0 {
			return nil
		}
		line := strings.TrimSuffix(string(prefix), "\n")
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			h.blankStart = lineStart
			h.end = lineEnd
			h.bodyStart = lineEnd
			return errStopScan
		}
		if (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) && len(h.fields) > 0 {
			last := &h.fields[len(h.fields)-1]
			addition := " " + cleanHeaderValue(strings.TrimSpace(line))
			if remaining := maxHeaderBytes - kept; len(addition) > remaining {
				addition = addition[:max(0, remaining)]
			}
			last.Value += addition
			kept += len(addition)
			last.End = lineEnd
			return nil
		}
		colon := strings.IndexByte(line, ':')
		if colon <= 0 || len(h.fields) >= maxHeaderFields || kept >= maxHeaderBytes {
			return nil
		}
		name := strings.TrimSpace(line[:colon])
		if name == "" {
			return nil
		}
		value := cleanHeaderValue(strings.TrimSpace(line[colon+1:]))
		if len(name)+len(value) > maxHeaderBytes-kept {
			value = value[:max(0, maxHeaderBytes-kept-len(name))]
		}
		kept += len(name) + len(value)
		h.fields = append(h.fields, headerField{Name: name, Value: value, Start: lineStart, End: lineEnd})
		return nil
	})
	if err == errStopScan {
		err = nil
	}
	if err != nil {
		return h, err
	}
	if h.blankStart < 0 {
		h.end = end
		h.bodyStart = end
	}
	return h, nil
}

func cleanHeaderValue(value string) string {
	// NUL is forbidden in a normal IMAP string and therefore cannot appear in
	// ENVELOPE or BODYSTRUCTURE. Preserve every other octet, including 8-bit
	// malformed header text, and substitute only the unencodable byte.
	return strings.ReplaceAll(value, "\x00", "\uFFFD")
}

// scanLines visits byte ranges one physical line at a time while retaining at
// most prefixLimit bytes of any line. A message body containing a 200 MiB line
// therefore stays a bounded-memory scan.
func scanLines(r io.ReaderAt, start, end int64, prefixLimit int, fn func(start, end int64, prefix []byte, endedLF bool) error) error {
	if start >= end {
		return nil
	}
	br := bufio.NewReaderSize(io.NewSectionReader(r, start, end-start), 32<<10)
	lineStart, at := start, start
	prefix := make([]byte, 0, min(prefixLimit, 4<<10))
	for {
		frag, err := br.ReadSlice('\n')
		if len(prefix) < prefixLimit {
			n := min(len(frag), prefixLimit-len(prefix))
			prefix = append(prefix, frag[:n]...)
		}
		at += int64(len(frag))
		switch err {
		case nil:
			if err := fn(lineStart, at, prefix, true); err != nil {
				return err
			}
			lineStart = at
			prefix = prefix[:0]
		case bufio.ErrBufferFull:
			continue
		case io.EOF:
			if at > lineStart {
				return fn(lineStart, at, prefix, false)
			}
			return nil
		default:
			return err
		}
	}
}

func (m *Message) bodyStructure(p *part) imap.BodyStructure {
	if strings.EqualFold(p.typeName, "multipart") && len(p.children) > 0 {
		children := make([]imap.BodyStructure, len(p.children))
		for i, child := range p.children {
			children[i] = m.bodyStructure(child)
		}
		return &imap.BodyStructureMultiPart{
			Children: children,
			Subtype:  p.subtype,
			Extended: &imap.BodyStructureMultiPartExt{
				Params:   cloneMap(p.params),
				Disp:     dispositionFromHeaders(p.headers),
				Lang:     languageFromHeaders(p.headers),
				Location: strings.TrimSpace(p.headers.value("Content-Location")),
			},
		}
	}
	bodySize := max(int64(0), p.end-p.headers.bodyStart)
	sp := &imap.BodyStructureSinglePart{
		Type:        p.typeName,
		Subtype:     p.subtype,
		Params:      cloneMap(p.params),
		ID:          strings.TrimSpace(p.headers.value("Content-ID")),
		Description: cleanHeaderValue(imap.DecodeHeader(p.headers.value("Content-Description"))),
		Encoding:    p.encoding,
		Size:        uint32(min(bodySize, int64(math.MaxUint32))),
		Extended: &imap.BodyStructureSinglePartExt{
			MD5:      strings.TrimSpace(p.headers.value("Content-MD5")),
			Disp:     dispositionFromHeaders(p.headers),
			Lang:     languageFromHeaders(p.headers),
			Location: strings.TrimSpace(p.headers.value("Content-Location")),
		},
	}
	if strings.EqualFold(p.typeName, "text") {
		sp.Text = &imap.BodyStructureText{NumLines: m.countLines(p.headers.bodyStart, p.end)}
	}
	if strings.EqualFold(p.typeName, "message") && strings.EqualFold(p.subtype, "rfc822") && p.message != nil {
		sp.Message = &imap.BodyStructureMessageRFC822{
			Envelope:      envelopeFromHeaders(p.message.headers),
			BodyStructure: m.bodyStructure(p.message),
			NumLines:      m.countLines(p.headers.bodyStart, p.end),
		}
	}
	return sp
}

func (m *Message) countLines(start, end int64) int64 {
	var count int64
	buf := make([]byte, 32<<10)
	for at := start; at < end; {
		nwant := int(min(int64(len(buf)), end-at))
		n, err := m.r.ReadAt(buf[:nwant], at)
		count += int64(bytes.Count(buf[:n], []byte{'\n'}))
		at += int64(n)
		if err != nil && err != io.EOF {
			break
		}
		if n == 0 {
			break
		}
	}
	return count
}

func cloneMap(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]string, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}

func dispositionFromHeaders(headers headerBlock) *imap.BodyStructureDisposition {
	raw := headers.value("Content-Disposition")
	if raw == "" {
		return nil
	}
	value, params, err := mime.ParseMediaType(raw)
	if err != nil {
		value, _, _ = strings.Cut(raw, ";")
		value = strings.TrimSpace(value)
		params = nil
	}
	cleanParams := make(map[string]string, len(params))
	for key, param := range params {
		cleanParams[cleanHeaderValue(key)] = cleanHeaderValue(param)
	}
	return &imap.BodyStructureDisposition{Value: cleanHeaderValue(value), Params: cloneMap(cleanParams)}
}

func languageFromHeaders(headers headerBlock) []string {
	raw := headers.value("Content-Language")
	if raw == "" {
		return nil
	}
	var langs []string
	for _, item := range strings.Split(raw, ",") {
		if item = strings.TrimSpace(item); item != "" {
			langs = append(langs, item)
		}
	}
	return langs
}

func envelopeFromHeaders(headers headerBlock) *imap.Envelope {
	rawDate := headers.value("Date")
	env := &imap.Envelope{
		Date:      parseDate(rawDate),
		RawDate:   rawDate,
		Subject:   cleanHeaderValue(imap.DecodeHeader(headers.value("Subject"))),
		From:      parseAddressList(headers.value("From")),
		Sender:    parseAddressList(headers.value("Sender")),
		ReplyTo:   parseAddressList(headers.value("Reply-To")),
		To:        parseAddressList(headers.value("To")),
		Cc:        parseAddressList(headers.value("Cc")),
		Bcc:       parseAddressList(headers.value("Bcc")),
		InReplyTo: imap.ParseMessageIDList(headers.value("In-Reply-To")),
		MessageID: strings.TrimSpace(headers.value("Message-ID")),
	}
	if len(env.Sender) == 0 {
		env.Sender = append([]imap.Address(nil), env.From...)
	}
	if len(env.ReplyTo) == 0 {
		env.ReplyTo = append([]imap.Address(nil), env.From...)
	}
	return env
}

func parseDate(raw string) time.Time {
	if raw == "" {
		return time.Time{}
	}
	t, err := mail.ParseDate(raw)
	if err != nil {
		return time.Time{}
	}
	return t
}

func parseAddressList(raw string) []imap.Address {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if colon, semi := groupBounds(raw); colon >= 0 && semi > colon {
		var out []imap.Address
		if prefix := strings.Trim(strings.TrimSpace(raw[:colon]), ","); prefix != "" {
			// The text before a group colon is its name unless a top-level comma
			// separates earlier mailboxes from it.
			if comma := lastTopLevelComma(prefix); comma >= 0 {
				out = append(out, parseAddressList(prefix[:comma])...)
				prefix = strings.TrimSpace(prefix[comma+1:])
			}
			out = append(out, imap.Address{Mailbox: cleanHeaderValue(imap.DecodeHeader(prefix))})
		}
		out = append(out, parseMailboxAddresses(raw[colon+1:semi])...)
		out = append(out, imap.Address{})
		if suffix := strings.Trim(strings.TrimSpace(raw[semi+1:]), ","); suffix != "" {
			out = append(out, parseAddressList(suffix)...)
		}
		return out
	}
	return parseMailboxAddresses(raw)
}

func parseMailboxAddresses(raw string) []imap.Address {
	addrs, err := mail.ParseAddressList(raw)
	if err == nil {
		out := make([]imap.Address, 0, len(addrs))
		for _, addr := range addrs {
			out = append(out, toIMAPAddress(addr))
		}
		return out
	}
	var out []imap.Address
	for _, token := range splitTopLevel(raw, ',') {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		if addr, err := mail.ParseAddress(token); err == nil {
			out = append(out, toIMAPAddress(addr))
			continue
		}
		address := strings.Trim(token, "<>")
		if at := strings.LastIndexByte(address, '@'); at > 0 && at+1 < len(address) {
			out = append(out, imap.Address{Mailbox: address[:at], Host: address[at+1:]})
		}
	}
	return out
}

func toIMAPAddress(addr *mail.Address) imap.Address {
	mailbox, host := addr.Address, ""
	if at := strings.LastIndexByte(mailbox, '@'); at >= 0 {
		mailbox, host = mailbox[:at], mailbox[at+1:]
	}
	return imap.Address{
		Name:    cleanHeaderValue(imap.DecodeHeader(addr.Name)),
		Mailbox: cleanHeaderValue(mailbox),
		Host:    cleanHeaderValue(host),
	}
}

func groupBounds(s string) (colon, semi int) {
	colon, semi = -1, -1
	quoted, escaped, angle, comment := false, false, 0, 0
	for i, r := range s {
		switch {
		case escaped:
			escaped = false
		case quoted && r == '\\':
			escaped = true
		case r == '"' && comment == 0:
			quoted = !quoted
		case !quoted && r == '<':
			angle++
		case !quoted && r == '>' && angle > 0:
			angle--
		case !quoted && angle == 0 && r == '(':
			comment++
		case !quoted && angle == 0 && r == ')' && comment > 0:
			comment--
		case !quoted && angle == 0 && comment == 0 && r == ':' && colon < 0:
			colon = i
		case !quoted && angle == 0 && comment == 0 && r == ';' && colon >= 0:
			return colon, i
		}
	}
	return colon, semi
}

func lastTopLevelComma(s string) int {
	parts := splitTopLevelWithOffsets(s, ',')
	if len(parts) < 2 {
		return -1
	}
	return parts[len(parts)-2].end
}

func splitTopLevel(s string, sep rune) []string {
	parts := splitTopLevelWithOffsets(s, sep)
	out := make([]string, len(parts))
	for i, part := range parts {
		out[i] = s[part.start:part.end]
	}
	return out
}

type stringRange struct{ start, end int }

func splitTopLevelWithOffsets(s string, sep rune) []stringRange {
	var out []stringRange
	start := 0
	quoted, escaped, angle, comment := false, false, 0, 0
	for i, r := range s {
		switch {
		case escaped:
			escaped = false
		case quoted && r == '\\':
			escaped = true
		case r == '"' && comment == 0:
			quoted = !quoted
		case !quoted && r == '<':
			angle++
		case !quoted && r == '>' && angle > 0:
			angle--
		case !quoted && angle == 0 && r == '(':
			comment++
		case !quoted && angle == 0 && r == ')' && comment > 0:
			comment--
		case !quoted && angle == 0 && comment == 0 && r == sep:
			out = append(out, stringRange{start, i})
			start = i + 1
		}
	}
	out = append(out, stringRange{start, len(s)})
	return out
}
