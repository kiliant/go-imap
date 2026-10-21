// Package imapdeflate handles IMAP compression boundaries without losing
// octets prefetched from the next transport layer.
package imapdeflate

import (
	"bufio"
	"compress/flate"
	"errors"
	"fmt"
	"io"
)

// Reader preserves compressed input buffering across UNAUTHENTICATE.
type Reader struct {
	input    *compressionInput
	inflater io.ReadCloser
}

func NewReader(input io.Reader) *Reader {
	r := &Reader{input: &compressionInput{Reader: bufio.NewReader(input)}}
	r.inflater = flate.NewReader(r.input)
	return r
}
func (r *Reader) Read(p []byte) (int, error) { return r.inflater.Read(p) }
func (r *Reader) Close() error               { return r.inflater.Close() }

// Finish consumes the trailer after the final IMAP line. A sync flush may
// terminate the layer; an optional final DEFLATE block is consumed lazily once
// the next layer has input, so a peer waiting for OK cannot deadlock here.
func (r *Reader) Finish() error {
	r.input.ending = true
	var extra [1]byte
	n, err := r.inflater.Read(extra[:])
	if n != 0 {
		return fmt.Errorf("imap: unexpected data after compression boundary")
	}
	if err == io.EOF || r.input.stopped && errors.Is(err, io.ErrUnexpectedEOF) {
		return nil
	}
	return err
}

// PlainReader preserves prefetched plaintext and tolerates Z_FINISH following
// a sync flush. Plain IMAP lines start with printable ASCII; a low first byte
// is the optional final compressed block, which must decode to no data.
func (r *Reader) PlainReader() io.Reader { return &plainReader{input: r.input.Reader} }

// ResumeReader reuses a completed plaintext layer's buffer when COMPRESS is
// negotiated again. Repeated UNAUTHENTICATE cycles must not retain a chain of
// old transport readers. Call only at a reader barrier.
func ResumeReader(input io.Reader) io.Reader {
	if plain, ok := input.(*plainReader); ok && plain.started {
		return plain.input
	}
	return input
}

type plainReader struct {
	input   *bufio.Reader
	started bool
}

func (r *plainReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if !r.started {
		first, err := r.input.Peek(1)
		if err != nil {
			return 0, err
		}
		if first[0] < 0x21 {
			final := flate.NewReader(r.input)
			var extra [1]byte
			n, err := final.Read(extra[:])
			_ = final.Close()
			if n != 0 {
				return 0, fmt.Errorf("imap: data in final compression block")
			}
			if err != io.EOF {
				return 0, fmt.Errorf("imap: invalid final compression block: %w", err)
			}
		}
		r.started = true
	}
	return r.input.Read(p)
}

// flate can return the last command bytes at a full dictionary boundary,
// before consuming the following sync-flush marker. Once UNAUTHENTICATE has
// been parsed, let it consume that trailer without reading the next transport
// layer. Outside this transition the byte reader behaves like bufio.Reader.
type compressionInput struct {
	*bufio.Reader
	tail    uint32
	ending  bool
	stopped bool
}

func (r *compressionInput) ReadByte() (byte, error) {
	if r.ending && r.tail == 0x0000ffff {
		r.stopped = true
		return 0, io.EOF
	}
	b, err := r.Reader.ReadByte()
	if err == nil {
		r.tail = r.tail<<8 | uint32(b)
	}
	return b, err
}

func (r *compressionInput) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.ending {
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		p[0] = b
		return 1, nil
	}
	n, err := r.Reader.Read(p)
	for _, b := range p[:n] {
		r.tail = r.tail<<8 | uint32(b)
	}
	return n, err
}
