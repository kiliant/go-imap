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
// The announced size is validated against Options.MaxLiteralSize *before* any
// buffer is sized or any payload octet is read, which is what makes {4294967295}
// a cheap rejection rather than an out-of-memory condition. Because the payload
// is then neither consumed nor consumable, that rejection is fatal to the
// connection.
func (d *Decoder) Literal() (*LiteralReader, bool) {
	if !d.ready("literal") {
		return nil, false
	}
	b, ok := d.peek()
	if !ok {
		return nil, false
	}
	binary := false
	if b == '~' {
		// "~" is only meaningful as the start of "~{"; anything else must be
		// left for another production to interpret.
		if p := d.peekN(2); len(p) < 2 || p[1] != '{' {
			return nil, false
		}
		if !d.consume() {
			return nil, false
		}
		binary = true
	} else if b != '{' {
		return nil, false
	}
	if !d.ExpectSpecial('{') {
		return nil, false
	}

	// From here on the input is committed to being a literal, and every failure
	// leaves an unknown number of payload octets on the wire: all of them are
	// fatal.
	size, ok := d.readNumber("literal", 1<<63-1)
	if !ok {
		d.failFatal("literal", ErrSyntax, "malformed literal length")
		return nil, false
	}
	if max := d.opts.MaxLiteralSize; max >= 0 && int64(size) > max {
		d.failFatal("literal", ErrLimitExceeded,
			"literal of %d octets exceeds the limit of %d", size, max)
		return nil, false
	}
	d.Special('+') // RFC 7888 non-synchronising marker, ignored on decode
	if !d.ExpectSpecial('}') {
		d.failFatal("literal", ErrSyntax, "expected } after literal length")
		return nil, false
	}
	if !d.ExpectCRLF() {
		d.failFatal("literal", ErrSyntax, "expected CRLF after literal announcement")
		return nil, false
	}

	lr := &LiteralReader{d: d, size: int64(size), remaining: int64(size), binary: binary}
	if lr.remaining > 0 {
		// A zero-length literal needs no interlock: there is nothing to drain.
		d.lit = lr
	}
	return lr, true
}
