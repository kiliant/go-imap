package imapclient

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime/quotedprintable"
	"strings"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// BinaryFetchOptions configures a BINARY fetch with optional UNKNOWN-CTE
// fallback. A nil pointer enables the fallback.
//
// Construct with keyed fields only; fields may be added in a future release.
type BinaryFetchOptions struct {
	// DisableUnknownCTEFallback refuses the BODY[] + client-side decode path
	// when the server answers NO [UNKNOWN-CTE]. The default (false) performs
	// that fallback and documents it on [BinaryFetchData.FellBack].
	DisableUnknownCTEFallback bool

	_ struct{}
}

func (o *BinaryFetchOptions) allowFallback() bool {
	return o == nil || !o.DisableUnknownCTEFallback
}

// BinaryFetchData is the decoded body returned by [Client.FetchBinary].
// BINARY, RFC 3516.
//
// Construct with keyed fields only; fields may be added in a future release.
type BinaryFetchData struct {
	// Content is the decoded section. Ownership passes to the caller.
	Content []byte

	// FellBack reports that the server rejected BINARY with UNKNOWN-CTE and
	// the content was obtained by fetching BODY[] (or the matching part) and
	// decoding Content-Transfer-Encoding on the client. RFC 3516 section 4.1
	// explicitly contemplates that failure mode: a server that cannot decode a
	// CTE answers NO [UNKNOWN-CTE], and the client is expected to fall back.
	FellBack bool

	// CTE is the Content-Transfer-Encoding that was decoded on the fallback
	// path, empty on the native BINARY path.
	CTE string

	_ struct{}
}

// FetchBinary fetches one message's binary section, falling back to BODY[]
// plus client-side CTE decoding when the server answers UNKNOWN-CTE.
// BINARY, RFC 3516 section 4.
//
// The request uses BINARY.PEEK so the \Seen flag is not set as a side effect
// of probing whether the server can decode the part. Native BINARY responses
// arrive as literal8 and may contain NUL octets; that is handled by the wire
// codec.
//
// # UNKNOWN-CTE fallback
//
// When the server cannot decode the part's Content-Transfer-Encoding it MUST
// answer NO with the UNKNOWN-CTE response code (RFC 3516 section 4.1). This
// method then issues a BODY.PEEK for the same part, reads the MIME headers to
// learn the CTE, and decodes base64 or quoted-printable locally. The fallback
// transfers the encoded octets and performs the decode in-process — cheaper
// than failing, more expensive than a server that supports BINARY for the
// part. Callers that want to handle UNKNOWN-CTE themselves set
// [BinaryFetchOptions.DisableUnknownCTEFallback].
func (c *Client) FetchBinary(ctx context.Context, seqNum imap.SeqNum, section *imap.FetchItemBinarySection, options *BinaryFetchOptions) (*BinaryFetchData, error) {
	return c.fetchBinary(ctx, false, uint32(seqNum), section, options)
}

// FetchBinaryUID is [Client.FetchBinary] addressing the message by UID.
func (c *Client) FetchBinaryUID(ctx context.Context, uid imap.UID, section *imap.FetchItemBinarySection, options *BinaryFetchOptions) (*BinaryFetchData, error) {
	return c.fetchBinary(ctx, true, uint32(uid), section, options)
}

func (c *Client) fetchBinary(ctx context.Context, byUID bool, id uint32, section *imap.FetchItemBinarySection, options *BinaryFetchOptions) (*BinaryFetchData, error) {
	if ctx == nil {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "BINARY fetch requires a non-nil context"}
	}
	if id == 0 {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "BINARY fetch requires a non-zero message identifier"}
	}
	if section == nil {
		section = &imap.FetchItemBinarySection{Peek: true}
	}
	req := *section
	req.Peek = true

	if !c.Supports("BINARY") {
		if !options.allowFallback() {
			return nil, capabilityError("BINARY", "BINARY")
		}
		return c.fetchBinaryFallback(ctx, byUID, id, &req)
	}

	var cmd *FetchCommand
	if byUID {
		cmd = c.FetchUID(imap.UIDSetNum(imap.UID(id)), &req)
	} else {
		cmd = c.Fetch(imap.SeqSetNum(imap.SeqNum(id)), &req)
	}
	msg, err := cmd.Next(ctx)
	if err != nil {
		// Drain completion so a NO [UNKNOWN-CTE] is visible on Wait.
		waitErr := cmd.Wait(ctx)
		if options.allowFallback() && isUnknownCTE(waitErr) {
			return c.fetchBinaryFallback(ctx, byUID, id, &req)
		}
		if waitErr != nil {
			return nil, waitErr
		}
		// Tagged OK with no FETCH is not a successful BINARY response; do not
		// surface a bare io.EOF to callers that expect *imap.Error.
		if errors.Is(err, io.EOF) {
			return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "BINARY fetch returned no message data"}
		}
		return nil, err
	}
	content, err := readBinarySection(msg)
	if err != nil {
		// Some servers answer with a FETCH that lacks BINARY and then tag
		// NO [UNKNOWN-CTE]. Prefer the tagged code over the parse error so the
		// documented fallback still runs (RFC 3516 section 4.1).
		waitErr := cmd.Wait(ctx)
		if options.allowFallback() && isUnknownCTE(waitErr) {
			return c.fetchBinaryFallback(ctx, byUID, id, &req)
		}
		if waitErr != nil {
			return nil, waitErr
		}
		return nil, err
	}
	if err := cmd.Wait(ctx); err != nil {
		if options.allowFallback() && isUnknownCTE(err) {
			return c.fetchBinaryFallback(ctx, byUID, id, &req)
		}
		return nil, err
	}
	return &BinaryFetchData{Content: content}, nil
}

func isUnknownCTE(err error) bool {
	var ie *imap.Error
	if err == nil {
		return false
	}
	if e, ok := err.(*imap.Error); ok {
		ie = e
	}
	return ie != nil && strings.EqualFold(string(ie.Code), string(imap.CodeUnknownCTE))
}

func readBinarySection(msg *imap.FetchMessageData) ([]byte, error) {
	if msg == nil {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "BINARY fetch returned no message data"}
	}
	for _, values := range msg.Items {
		for _, value := range values {
			if bin, ok := value.(*imap.FetchDataBinarySection); ok {
				return readFetchLiteral(bin.Literal)
			}
		}
	}
	return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "BINARY fetch response contained no BINARY section"}
}

func readFetchLiteral(r io.Reader) ([]byte, error) {
	if r == nil {
		return nil, nil
	}
	if c, ok := r.(io.Closer); ok {
		defer c.Close()
	}
	return io.ReadAll(r)
}

func (c *Client) fetchBinaryFallback(ctx context.Context, byUID bool, id uint32, section *imap.FetchItemBinarySection) (*BinaryFetchData, error) {
	body := &imap.FetchItemBodySection{
		Part:    append([]int(nil), section.Part...),
		Partial: section.Partial,
		Peek:    true,
	}
	var cmd *FetchCommand
	if byUID {
		cmd = c.FetchUID(imap.UIDSetNum(imap.UID(id)), body)
	} else {
		cmd = c.Fetch(imap.SeqSetNum(imap.SeqNum(id)), body)
	}
	msg, err := cmd.Next(ctx)
	if err != nil {
		_ = cmd.Wait(ctx)
		return nil, err
	}
	raw, headers, err := readBodySectionWithHeaders(msg)
	if err != nil {
		_ = cmd.Wait(ctx)
		return nil, err
	}
	if err := cmd.Wait(ctx); err != nil {
		return nil, err
	}
	cte := headerValue(headers, "Content-Transfer-Encoding")
	decoded, err := decodeTransferEncoding(raw, cte)
	if err != nil {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "BINARY fallback CTE decode failed", Err: err}
	}
	return &BinaryFetchData{Content: decoded, FellBack: true, CTE: cte}, nil
}

func readBodySectionWithHeaders(msg *imap.FetchMessageData) (body []byte, headers string, err error) {
	if msg == nil {
		return nil, "", &imap.Error{Type: imap.ErrorTypeProtocol, Text: "BODY fetch returned no message data"}
	}
	for _, values := range msg.Items {
		for _, value := range values {
			switch v := value.(type) {
			case *imap.FetchDataBodySection:
				raw, err := readFetchLiteral(v.Literal)
				if err != nil {
					return nil, "", err
				}
				return splitMIMEHeaders(raw)
			}
		}
	}
	return nil, "", &imap.Error{Type: imap.ErrorTypeProtocol, Text: "BODY fetch response contained no body section"}
}

func splitMIMEHeaders(raw []byte) (body []byte, headers string, err error) {
	// When the section is the whole message, headers precede the body.
	// Part fetches of a leaf often return body octets only; CTE then has to
	// come from a separate HEADER fetch — callers of the fallback for a
	// nested part should prefer native BINARY. Best-effort: if there is a
	// blank-line separator, split; otherwise treat everything as body.
	text := string(raw)
	if i := strings.Index(text, "\r\n\r\n"); i >= 0 {
		return raw[i+4:], text[:i], nil
	}
	if i := strings.Index(text, "\n\n"); i >= 0 {
		return raw[i+2:], text[:i], nil
	}
	return raw, "", nil
}

func headerValue(headers, name string) string {
	for _, line := range strings.Split(headers, "\n") {
		line = strings.TrimRight(line, "\r")
		if i := strings.IndexByte(line, ':'); i >= 0 {
			if strings.EqualFold(strings.TrimSpace(line[:i]), name) {
				return strings.TrimSpace(line[i+1:])
			}
		}
	}
	return ""
}

func decodeTransferEncoding(raw []byte, cte string) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(cte)) {
	case "", "7bit", "8bit", "binary":
		return raw, nil
	case "base64":
		// MIME base64 may contain whitespace; StdEncoding rejects it.
		compact := make([]byte, 0, len(raw))
		for _, b := range raw {
			if b != ' ' && b != '\t' && b != '\r' && b != '\n' {
				compact = append(compact, b)
			}
		}
		out := make([]byte, base64.StdEncoding.DecodedLen(len(compact)))
		n, err := base64.StdEncoding.Decode(out, compact)
		if err != nil {
			return nil, err
		}
		return out[:n], nil
	case "quoted-printable":
		return io.ReadAll(quotedprintable.NewReader(strings.NewReader(string(raw))))
	default:
		return nil, fmt.Errorf("unsupported Content-Transfer-Encoding %q", cte)
	}
}

// FetchBinarySize requests BINARY.SIZE for one message part.
// BINARY, RFC 3516 section 4.2.
func (c *Client) FetchBinarySize(ctx context.Context, seqNum imap.SeqNum, part []int) (int64, error) {
	return c.fetchBinarySize(ctx, false, uint32(seqNum), part)
}

// FetchBinarySizeUID is [Client.FetchBinarySize] by UID.
func (c *Client) FetchBinarySizeUID(ctx context.Context, uid imap.UID, part []int) (int64, error) {
	return c.fetchBinarySize(ctx, true, uint32(uid), part)
}

func (c *Client) fetchBinarySize(ctx context.Context, byUID bool, id uint32, part []int) (int64, error) {
	if ctx == nil {
		return 0, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "BINARY.SIZE requires a non-nil context"}
	}
	if id == 0 {
		return 0, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "BINARY.SIZE requires a non-zero message identifier"}
	}
	if !c.Supports("BINARY") {
		return 0, capabilityError("BINARY.SIZE", "BINARY")
	}
	item := &imap.FetchItemBinarySectionSize{Part: part}
	var cmd *FetchCommand
	if byUID {
		cmd = c.FetchUID(imap.UIDSetNum(imap.UID(id)), item)
	} else {
		cmd = c.Fetch(imap.SeqSetNum(imap.SeqNum(id)), item)
	}
	msg, err := cmd.Next(ctx)
	if err != nil {
		_ = cmd.Wait(ctx)
		return 0, err
	}
	var size int64
	found := false
	for _, values := range msg.Items {
		for _, value := range values {
			if v, ok := value.(imap.FetchDataBinarySectionSize); ok {
				size = int64(v)
				found = true
			}
		}
	}
	if err := cmd.Wait(ctx); err != nil {
		return 0, err
	}
	if !found {
		return 0, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "BINARY.SIZE missing from FETCH response"}
	}
	return size, nil
}

// writeBinaryLiteral announces a literal8 when the payload may contain NUL,
// which APPEND under BINARY / UTF8=ACCEPT requires. RFC 3516, RFC 9755.
func writeBinaryLiteral(enc *imapwire.Encoder, size int64, binary bool, r io.Reader, ctx context.Context) error {
	literal, err := enc.Literal(size, binary)
	if err != nil {
		return err
	}
	if _, err := io.Copy(literal, appendContextReader{ctx: ctx, reader: io.LimitReader(r, size)}); err != nil {
		_ = literal.Close()
		return err
	}
	return literal.Close()
}
