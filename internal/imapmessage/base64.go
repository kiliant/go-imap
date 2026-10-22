package imapmessage

import (
	"bufio"
	"io"
)

// base64Reader decodes a base64 body the way RFC 2045 section 6.8 requires a
// decoder to: characters outside the base64 alphabet are ignored, so line
// breaks, spaces and tabs inside the data do not fail it, and "=" marks the end
// of the data. A final group of two or three characters still yields its
// octets; a single dangling character carries less than one octet and is
// dropped. encoding/base64 rejects all of these.
type base64Reader struct {
	src  *bufio.Reader
	acc  uint32
	bits int
	done bool
}

func newBase64Reader(r io.Reader) io.Reader {
	return &base64Reader{src: bufio.NewReader(r)}
}

// Read decodes at most one octet per input character, so it never has output
// left over between calls.
func (r *base64Reader) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		if r.done {
			if n > 0 {
				return n, nil
			}
			return 0, io.EOF
		}
		b, err := r.src.ReadByte()
		if err != nil {
			if err != io.EOF {
				return n, err
			}
			r.done = true
			continue
		}
		if b == '=' {
			r.done = true
			continue
		}
		value, ok := base64Value(b)
		if !ok {
			continue
		}
		r.acc = r.acc<<6 | uint32(value)
		r.bits += 6
		if r.bits >= 8 {
			r.bits -= 8
			p[n] = byte(r.acc >> r.bits)
			r.acc &= 1<<r.bits - 1
			n++
		}
	}
	return n, nil
}

func base64Value(b byte) (byte, bool) {
	switch {
	case b >= 'A' && b <= 'Z':
		return b - 'A', true
	case b >= 'a' && b <= 'z':
		return b - 'a' + 26, true
	case b >= '0' && b <= '9':
		return b - '0' + 52, true
	case b == '+':
		return 62, true
	case b == '/':
		return 63, true
	}
	return 0, false
}
