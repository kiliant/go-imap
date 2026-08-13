package imapserver

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// MULTIAPPEND (RFC 3502) and CATENATE (RFC 4469).
//
// # Why these needed a second look
//
// Both extend APPEND's arguments with more literals, and a literal's length is
// not on the wire until the previous one has been consumed. The framework runs a
// command's parse to completion before its handler, so the obvious reading is
// that neither can be expressed — which is what an earlier attempt concluded
// before reverting.
//
// That was wrong, and the mechanism was already there. APPEND is a *barrier*
// command, so while its handler runs the reader goroutine sits in waitBarrier
// serving readClientData against the live decoder. Parsing therefore continues
// between literals: the parser reads as far as the first literal's announcement,
// the handler consumes that literal, and then asks the reader for the next
// chunk. That is the same path continuations and IDLE already use.

// CatenateSession is the optional CATENATE support of RFC 4469.
//
// A catenated APPEND builds the new message from a sequence of parts: literal
// text from the client, and IMAP URLs naming text already on the server.
// Resolving those URLs without a round trip is the entire point — a client that
// could fetch and re-upload the part would not need the extension.
type CatenateSession interface {
	// ResolveCatenateURL returns the bytes an IMAP URL names, which must refer
	// to a message the authenticated user can read. A nil reader means the URL
	// did not resolve, which fails the APPEND.
	ResolveCatenateURL(ctx context.Context, url string, options *CatenateOptions) (io.ReadCloser, error)
}

// CatenateOptions configures URL resolution. A nil pointer selects the
// defaults.
// Construct with keyed fields only; fields may be added in a future release.
type CatenateOptions struct{ _ struct{} }

func init() {
	registerCapabilities(
		// MULTIAPPEND needs nothing from the backend: several messages is
		// several calls to the Append it already implements.
		capabilityDescriptor{Name: "MULTIAPPEND", States: stateMaskAuthenticated | stateMaskSelected},
		capabilityDescriptor{
			Name:            "CATENATE",
			States:          stateMaskAuthenticated | stateMaskSelected,
			RequiresBackend: sessionImplements[CatenateSession](),
		},
	)
}

// appendMessage is one message of an APPEND command: its flags, internal date
// and payload. A plain APPEND has one; MULTIAPPEND has several.
type appendMessage struct {
	flags        []imap.Flag
	internalDate time.Time
	// literal is the payload of an ordinary append, and catenate the part list
	// of a catenated one. Exactly one is set.
	literal  *imapwire.LiteralReader
	catenate []catenatePart
}

// catenatePart is one element of a CATENATE list: client-supplied text, or an
// IMAP URL naming text already on the server.
type catenatePart struct {
	literal *imapwire.LiteralReader
	url     string
}

// parseAppendMessagePrefix reads one message's flags and internal date, stopping
// at the payload.
func parseAppendMessagePrefix(decoder *imapwire.Decoder, message *appendMessage) error {
	if decoder.PeekSpecial('(') {
		var rawFlags []string
		if err := decoder.ExpectFlagList(&rawFlags); err != nil {
			return err
		}
		for _, flag := range rawFlags {
			message.flags = append(message.flags, imap.Flag(flag))
		}
		if !decoder.ExpectSP() {
			return decoder.Err()
		}
	}
	if decoder.PeekSpecial('"') {
		if !decoder.ExpectDateTime(&message.internalDate) || !decoder.ExpectSP() {
			return decoder.Err()
		}
	}
	return nil
}

// parseAppendPayloadStart reads as far as the first literal of a message's
// payload — the whole literal for an ordinary append, or CATENATE's opening
// parenthesis and first part.
//
// It stops at a literal announcement rather than consuming it, because the bytes
// have not arrived yet: the handler reads them and then asks for what follows.
func parseAppendPayloadStart(decoder *imapwire.Decoder, message *appendMessage) error {
	if decoder.PeekAtomEqual("CATENATE") {
		var keyword string
		if !decoder.ExpectAtom(&keyword) || !decoder.ExpectSP() || !decoder.ExpectSpecial('(') {
			return decoder.Err()
		}
		return parseCatenatePart(decoder, message)
	}
	literal, ok := decoder.Literal()
	if !ok {
		return decoder.Err()
	}
	if literal.Binary() {
		if err := literal.Discard(); err != nil {
			return err
		}
		return fmt.Errorf("literal8 APPEND requires BINARY")
	}
	message.literal = literal
	return nil
}

// parseCatenatePart reads one CATENATE part. A URL is complete on the line; a
// TEXT part stops at its literal, which the handler then consumes.
func parseCatenatePart(decoder *imapwire.Decoder, message *appendMessage) error {
	var kind string
	if !decoder.ExpectAtom(&kind) || !decoder.ExpectSP() {
		return decoder.Err()
	}
	switch strings.ToUpper(kind) {
	case "TEXT":
		literal, ok := decoder.Literal()
		if !ok {
			return decoder.Err()
		}
		message.catenate = append(message.catenate, catenatePart{literal: literal})
		return nil
	case "URL":
		var url string
		if !decoder.ExpectAstring(&url) {
			return decoder.Err()
		}
		message.catenate = append(message.catenate, catenatePart{url: url})
		return nil
	default:
		return fmt.Errorf("unsupported CATENATE part %q", kind)
	}
}

// appendStep is what the reader found after a payload was consumed.
type appendStep int

const (
	// appendStepDone means the command ended: CRLF was read.
	appendStepDone appendStep = iota
	// appendStepMessage means another MULTIAPPEND message follows.
	appendStepMessage
	// appendStepCatenatePart means another CATENATE part follows.
	appendStepCatenatePart
	// appendStepCatenateEnd means the CATENATE list closed.
	appendStepCatenateEnd
)

type appendContinuation struct {
	step    appendStep
	message appendMessage
}

// readAppendContinuation asks the reader goroutine what follows the payload just
// consumed.
//
// This runs on the reader's decoder while the reader sits in waitBarrier, which
// is why the interleaving works at all. inCatenate selects which grammar applies:
// inside a CATENATE list the next token is another part or the closing
// parenthesis, and outside it is another message or the end of the command.
func readAppendContinuation(ctx context.Context, c *conn, inCatenate bool) (*appendContinuation, error) {
	value, err := c.readClientData(ctx, func(decoder *imapwire.Decoder) (any, error) {
		result := &appendContinuation{}
		if inCatenate {
			if decoder.SP() {
				if err := parseCatenatePart(decoder, &result.message); err != nil {
					return nil, err
				}
				result.step = appendStepCatenatePart
				return result, nil
			}
			if !decoder.ExpectSpecial(')') {
				return nil, decoder.Err()
			}
			result.step = appendStepCatenateEnd
			return result, nil
		}
		// MULTIAPPEND continues with a space before the next message.
		// RFC 3502 section 6.3.11.
		if decoder.SP() {
			if err := parseAppendMessagePrefix(decoder, &result.message); err != nil {
				return nil, err
			}
			if err := parseAppendPayloadStart(decoder, &result.message); err != nil {
				return nil, err
			}
			result.step = appendStepMessage
			return result, nil
		}
		if !decoder.ExpectCRLF() {
			return nil, decoder.Err()
		}
		result.step = appendStepDone
		return result, nil
	})
	if err != nil {
		return nil, err
	}
	continuation, ok := value.(*appendContinuation)
	if !ok {
		return nil, fmt.Errorf("imapserver: reader returned an invalid APPEND continuation")
	}
	return continuation, nil
}

// appendPayload assembles one message's bytes, reading whatever the wire still
// owes as it goes.
//
// A catenated message is assembled part by part rather than buffered whole:
// each literal is streamed into the pipe as it arrives and each URL is resolved
// and streamed, so a large catenated message never exists in memory here.
type appendPayload struct {
	reader io.Reader
	// literal is the wire literal an ordinary append streams from, and pending
	// tracks how much of it the backend left unread.
	literal *imapwire.LiteralReader
	pending *appendLiteral
	// parts holds a catenated message's assembled pieces.
	parts  [][]byte
	closes []io.Closer
}

// drain consumes whatever the backend left unread. The rest of the command
// cannot be parsed until this message's bytes are off the wire, so this is not
// optional cleanup — skipping it desynchronises the connection.
func (p *appendPayload) drain() {
	// Only an unfinished literal is discarded. Discarding a fully-read one
	// consumes the bytes that follow it, which under MULTIAPPEND is the next
	// message's header.
	if p.literal != nil && p.pending != nil && p.pending.remaining != 0 {
		_ = p.literal.Discard()
		p.pending.remaining = 0
	}
}

func (p *appendPayload) close() {
	for i := len(p.closes) - 1; i >= 0; i-- {
		_ = p.closes[i].Close()
	}
}

// bytesReader is a reader over an assembled catenate part.
func bytesReader(part []byte) io.Reader { return &sliceReader{data: part} }

type sliceReader struct {
	data []byte
	at   int
}

func (r *sliceReader) Read(p []byte) (int, error) {
	if r.at >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.at:])
	r.at += n
	return n, nil
}

// validateAppendShape refuses an APPEND whose form the session may not use.
func validateAppendShape(c *conn, messages int, catenated bool) error {
	advertised := advertisedCapabilities(c)
	if messages > 1 && !advertised["MULTIAPPEND"] {
		return fmt.Errorf("MULTIAPPEND is not available")
	}
	if catenated && !advertised["CATENATE"] {
		return fmt.Errorf("CATENATE is not available")
	}
	return nil
}
