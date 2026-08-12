package imapwire

import (
	"bytes"
	"io"
)

// LiteralReader streams the payload of a literal.
//
// It exists so that FETCH BODY[] of a large message never buffers: the client
// hands the reader to the caller and the octets go straight from the socket to
// wherever they are wanted.
//
// The decoder refuses to parse anything else until the payload has been
// consumed, either by reading it to EOF or by calling [LiteralReader.Discard].
// Skipping that step would let the next parse attribute payload octets to the
// following response, which is the classic way an IMAP client desynchronises and
// starts reporting one message's data under another message's number.
type LiteralReader struct {
	d         *Decoder
	size      int64
	remaining int64
	binary    bool
}

// LiteralInfo is a client literal announcement. A server-facing command
// decoder first reads the announcement, then either opens the payload with
// [Decoder.OpenLiteral] or rejects a synchronising literal without reading a
// payload that the client has not sent yet.
type LiteralInfo struct {
	Size             int64
	Binary           bool
	NonSynchronising bool
}

// Size returns the announced payload length in octets.
func (lr *LiteralReader) Size() int64 { return lr.size }

// Binary reports whether the payload arrived as a literal8 (~{n}), which may
// contain NUL octets. literal8 is used by BINARY (RFC 3516) and by UTF8=ACCEPT
// (RFC 9755).
func (lr *LiteralReader) Binary() bool { return lr.binary }

// Read implements [io.Reader]. It returns io.EOF once exactly Size octets have
// been produced; it never reads past the payload.
func (lr *LiteralReader) Read(p []byte) (int, error) {
	if lr.remaining == 0 {
		lr.release()
		return 0, io.EOF
	}
	if lr.d.err != nil {
		return 0, lr.d.err
	}
	if int64(len(p)) > lr.remaining {
		p = p[:lr.remaining]
	}
	n, err := lr.d.r.Read(p)
	lr.remaining -= int64(n)
	if lr.remaining == 0 {
		lr.release()
		// io.Reader permits returning the final bytes together with io.EOF. The
		// promised literal is complete in that case, so EOF is not truncation.
		if err == io.EOF {
			err = nil
		}
	}
	if err != nil {
		if err == io.EOF {
			// The server promised more octets than it delivered; the stream is
			// unusable.
			lr.d.failFatal("literal", ErrUnexpectedEOF,
				"literal truncated with %d octets outstanding", lr.remaining)
		} else {
			lr.d.failFatal("literal", err, "reading literal payload")
		}
		return n, lr.d.err
	}
	if !lr.binary && bytes.IndexByte(p[:n], 0) >= 0 {
		lr.d.failFatal("literal", ErrSyntax, "NUL octet requires literal8")
		return n, lr.d.err
	}
	lr.d.captureBytes(p[:n])
	return n, nil
}

// Discard consumes the rest of the payload and throws it away. It is the
// explicit alternative to draining, and is safe to call more than once.
func (lr *LiteralReader) Discard() error {
	if lr.remaining == 0 {
		lr.release()
		return nil
	}
	if _, err := io.Copy(io.Discard, lr); err != nil {
		return err
	}
	return nil
}

// release clears the decoder's pending-literal interlock once the payload is
// gone.
func (lr *LiteralReader) release() {
	if lr.d.lit == lr {
		lr.d.lit = nil
	}
}

// Literal matches a literal announcement and returns a reader over its payload:
//
//	literal  = "{" number64 ["+"] "}" CRLF *CHAR8   ; RFC 3501 9, RFC 7888
//	literal8 = "~{" number64 "}" CRLF *OCTET        ; RFC 3516, RFC 9755
//
// The optional "+" is the non-synchronising form of RFC 7888. A server never
// sends it — it is a client-to-server marker — but it is accepted so that a
// captured command line decodes with the same code path.
//
// The announced size is validated against Options.MaxLiteralSize before any
// buffer is sized or payload octet is read. An oversized server response, or an
// oversized non-synchronising command literal already in flight, is fatal. A
// synchronising command announcement remains rejectable without losing stream
// alignment; see [Decoder.LiteralAnnouncement].
func (d *Decoder) Literal() (*LiteralReader, bool) {
	if d.literalDecision != nil {
		return d.commandLiteral(d.opts.MaxLiteralSize)
	}
	info, ok := d.literalAnnouncement(false)
	if !ok {
		return nil, false
	}
	return d.OpenLiteral(info), true
}

// LiteralAnnouncement matches a literal announcement and stops before its
// payload. It is the server-direction counterpart of [Decoder.Literal]: a
// server can send a continuation before calling [Decoder.OpenLiteral], reject a
// synchronising literal by not opening it, or immediately open and drain a
// non-synchronising literal whose payload is already in flight. A synchronising
// announcement is returned even when its size exceeds MaxLiteralSize, so the
// server can reject it cleanly; OpenLiteral still refuses to open it.
func (d *Decoder) LiteralAnnouncement() (LiteralInfo, bool) {
	return d.literalAnnouncement(true)
}

func (d *Decoder) literalAnnouncement(serverDirection bool) (LiteralInfo, bool) {
	var info LiteralInfo
	if !d.ready("literal") {
		return info, false
	}
	b, ok := d.peek()
	if !ok {
		return info, false
	}
	if b == '~' {
		// "~" is only meaningful as the start of "~{"; anything else must be
		// left for another production to interpret.
		if p := d.peekN(2); len(p) < 2 || p[1] != '{' {
			return info, false
		}
		if !d.consume() {
			return info, false
		}
		info.Binary = true
	} else if b != '{' {
		return info, false
	}
	if !d.ExpectSpecial('{') {
		return info, false
	}

	// From here on the input is committed to being a literal, and every failure
	// leaves an unknown number of payload octets on the wire: all of them are
	// fatal.
	size, ok := d.readNumber("literal", 1<<63-1)
	if !ok {
		d.failFatal("literal", ErrSyntax, "malformed literal length")
		return info, false
	}
	info.Size = int64(size)
	info.NonSynchronising = d.Special('+')
	if !d.ExpectSpecial('}') {
		d.failFatal("literal", ErrSyntax, "expected } after literal length")
		return info, false
	}
	if !d.ExpectCRLF() {
		d.failFatal("literal", ErrSyntax, "expected CRLF after literal announcement")
		return info, false
	}
	if max := d.opts.MaxLiteralSize; max >= 0 && info.Size > max {
		// A synchronising command literal has no payload on the wire yet, so
		// its caller can reject it and continue at the next command. Every
		// other oversized literal leaves payload in flight or comes from a
		// server response, where recovery is impossible.
		if !serverDirection || info.NonSynchronising {
			d.failFatal("literal", ErrLimitExceeded,
				"literal of %d octets exceeds the limit of %d", info.Size, max)
			return info, false
		}
	}
	d.announced = &info
	return info, true
}

// commandLiteral applies the server's size policy and continuation decision.
// limit may be lower than MaxLiteralSize for a production which materialises
// its value, such as String.
func (d *Decoder) commandLiteral(limit int64) (*LiteralReader, bool) {
	info, ok := d.literalAnnouncement(true)
	if !ok {
		return nil, false
	}
	if max := d.opts.MaxLiteralSize; max >= 0 && info.Size > max {
		d.rejectAnnouncedLiteral(info, ErrLimitExceeded,
			"literal of %d octets exceeds the limit of %d", info.Size, max)
		return nil, false
	}
	if limit >= 0 && info.Size > limit {
		d.rejectAnnouncedLiteral(info, ErrLimitExceeded,
			"literal of %d octets exceeds the in-memory limit of %d", info.Size, limit)
		return nil, false
	}
	if err := d.literalDecision(info); err != nil {
		d.rejectAnnouncedLiteral(info, err, "literal rejected")
		return nil, false
	}
	return d.OpenLiteral(info), true
}

func (d *Decoder) rejectAnnouncedLiteral(info LiteralInfo, cause error, format string, args ...any) {
	if info.NonSynchronising {
		lr := d.OpenLiteral(info)
		if err := lr.Discard(); err != nil {
			return
		}
	} else {
		_ = d.RejectLiteral(info)
		d.commandBoundary = true
	}
	d.failCause("literal", cause, format, args...)
}

// OpenLiteral returns a reader for a previously parsed announcement. It must be
// called at most once and before any other decoder operation.
func (d *Decoder) OpenLiteral(info LiteralInfo) *LiteralReader {
	if d.announced == nil || *d.announced != info {
		d.failFatal("literal", ErrLiteralPending, "literal announcement does not match")
		return &LiteralReader{d: d}
	}
	if max := d.opts.MaxLiteralSize; max >= 0 && info.Size > max {
		d.announced = nil
		d.failFatal("literal", ErrLimitExceeded,
			"literal of %d octets exceeds the limit of %d", info.Size, max)
		return &LiteralReader{d: d}
	}
	d.announced = nil
	lr := &LiteralReader{d: d, size: info.Size, remaining: info.Size, binary: info.Binary}
	if lr.remaining > 0 {
		// A zero-length literal needs no interlock: there is nothing to drain.
		d.lit = lr
	}
	return lr
}

// RejectLiteral rejects a synchronising command literal after its announcement
// has been read. A non-synchronising literal cannot be rejected because its
// payload is already in flight; it must be opened and drained instead.
func (d *Decoder) RejectLiteral(info LiteralInfo) error {
	if d.announced == nil || *d.announced != info {
		return newError("literal", "literal announcement does not match")
	}
	if info.NonSynchronising {
		return newError("literal", "non-synchronising literal must be drained")
	}
	d.announced = nil
	return nil
}
