package imapwire

import (
	"encoding/base64"
	"errors"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Modified UTF-7, the mailbox-name encoding of RFC 3501 section 5.1.3.
//
// The rules, quoting the relevant constraints because each of them is a place
// implementations go wrong:
//
//   - Printable US-ASCII except "&" represents itself: octets 0x20-0x25 and
//     0x27-0x7e. Note what this excludes — 0x00-0x1f and 0x7f are ASCII but must
//     still be encoded, so a control character in a mailbox name is not passed
//     through.
//   - "&" (0x26) is represented by the two-octet sequence "&-".
//   - Everything else is modified BASE64 of the UTF-16BE encoding, with ","
//     substituted for "/" and no "=" padding.
//   - "&" shifts into BASE64 and "-" shifts back. There is no implicit shift
//     back, so a name ending in a non-ASCII character ends with "-". Null shifts
//     ("&-" while already in BASE64) are forbidden.
//
// RFC 9051 removes the encoding entirely: a rev2 server, or a rev1 server with
// UTF8=ACCEPT enabled, uses raw UTF-8 instead. Both paths exist here and the
// decoder and encoder select between them; see [Decoder.SetUTF8Accept].

// modifiedBase64 is RFC 4648 base64 with "," in place of "/" and no padding.
// Strict rejects trailing bits that are not zero, which a lenient decoder would
// otherwise accept as several distinct encodings of the same name.
var modifiedBase64 = base64.NewEncoding(
	"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+,",
).WithPadding(base64.NoPadding).Strict()

// ErrInvalidMailboxName reports a mailbox name that is not valid modified UTF-7
// (decoding) or not valid UTF-8 (encoding).
var ErrInvalidMailboxName = errors.New("imapwire: invalid mailbox name")

// selfRepresenting reports whether r is a printable US-ASCII character other
// than "&", which is exactly the set that survives encoding unchanged.
func selfRepresenting(r rune) bool {
	return (r >= 0x20 && r <= 0x25) || (r >= 0x27 && r <= 0x7e)
}

// EncodeMailboxName converts a UTF-8 mailbox name to modified UTF-7 (RFC 3501
// section 5.1.3). Input that is not valid UTF-8 is rejected rather than encoded
// approximately: a mailbox name that does not round-trip is a mailbox the caller
// cannot later select.
func EncodeMailboxName(name string) (string, error) {
	if !utf8.ValidString(name) {
		return "", ErrInvalidMailboxName
	}
	// Fast path: names are overwhelmingly plain ASCII.
	plain := true
	for _, r := range name {
		if !selfRepresenting(r) {
			plain = false
			break
		}
	}
	if plain {
		return name, nil
	}

	var sb strings.Builder
	sb.Grow(len(name) + 8)
	var pending []uint16 // UTF-16 code units awaiting a BASE64 run

	flush := func() {
		if len(pending) == 0 {
			return
		}
		buf := make([]byte, 2*len(pending))
		for i, u := range pending {
			buf[2*i] = byte(u >> 8)
			buf[2*i+1] = byte(u)
		}
		sb.WriteByte('&')
		sb.WriteString(modifiedBase64.EncodeToString(buf))
		sb.WriteByte('-')
		pending = pending[:0]
	}

	for _, r := range name {
		switch {
		case r == '&':
			flush()
			sb.WriteString("&-")
		case selfRepresenting(r):
			flush()
			sb.WriteRune(r)
		default:
			// utf16.Encode handles the surrogate pair for r > 0xFFFF. r cannot
			// be a lone surrogate here: those are not valid UTF-8 and were
			// rejected above.
			pending = append(pending, utf16.Encode([]rune{r})...)
		}
	}
	flush()
	return sb.String(), nil
}

// DecodeMailboxName converts a modified UTF-7 mailbox name to UTF-8.
//
// The decoder is deliberately more permissive than the encoder in one respect:
// it accepts BASE64 runs that encode characters which could have represented
// themselves (a strict reading of the RFC forbids "&AEE-" for "A"). Servers echo
// names in whatever form they store them, and refusing one makes the mailbox
// unreachable for no gain in safety. What it does not accept is anything
// ambiguous or lossy: an unterminated shift sequence, a null shift, base64 that
// is not a whole number of UTF-16 code units, or an unpaired surrogate.
func DecodeMailboxName(name string) (string, error) {
	if !strings.ContainsRune(name, '&') {
		for i := 0; i < len(name); i++ {
			if !selfRepresenting(rune(name[i])) {
				return "", ErrInvalidMailboxName
			}
		}
		return name, nil
	}
	var sb strings.Builder
	sb.Grow(len(name))
	for i := 0; i < len(name); {
		c := name[i]
		if c != '&' {
			if !selfRepresenting(rune(c)) {
				return "", ErrInvalidMailboxName
			}
			sb.WriteByte(c)
			i++
			continue
		}
		i++ // consume "&"
		if i < len(name) && name[i] == '-' {
			sb.WriteByte('&')
			i++
			continue
		}
		start := i
		for i < len(name) && isModifiedBase64Char(name[i]) {
			i++
		}
		run := name[start:i]
		if run == "" {
			// "&" followed by something that cannot start a BASE64 run: either
			// the forbidden null shift or a stray "&".
			return "", ErrInvalidMailboxName
		}
		if i >= len(name) || name[i] != '-' {
			// RFC 3501 has no implicit shift back to US-ASCII.
			return "", ErrInvalidMailboxName
		}
		i++ // consume the terminating "-"
		s, err := decodeBase64Run(run)
		if err != nil {
			return "", err
		}
		sb.WriteString(s)
	}
	return sb.String(), nil
}

func isModifiedBase64Char(b byte) bool {
	switch {
	case b >= 'A' && b <= 'Z', b >= 'a' && b <= 'z', b >= '0' && b <= '9':
		return true
	case b == '+' || b == ',':
		return true
	}
	return false
}

// decodeBase64Run decodes one shift sequence body: modified BASE64 of UTF-16BE.
func decodeBase64Run(run string) (string, error) {
	// A run of length 1 mod 4 carries fewer than 8 bits of payload in its last
	// group and cannot be the encoding of any octet string.
	if len(run)%4 == 1 {
		return "", ErrInvalidMailboxName
	}
	b, err := modifiedBase64.DecodeString(run)
	if err != nil {
		return "", ErrInvalidMailboxName
	}
	if len(b)%2 != 0 {
		return "", ErrInvalidMailboxName
	}
	var sb strings.Builder
	sb.Grow(len(b))
	for i := 0; i < len(b); i += 2 {
		u := uint16(b[i])<<8 | uint16(b[i+1])
		switch {
		case u >= 0xd800 && u < 0xdc00: // high surrogate: a low one must follow
			if i+3 >= len(b) {
				return "", ErrInvalidMailboxName
			}
			lo := uint16(b[i+2])<<8 | uint16(b[i+3])
			if lo < 0xdc00 || lo >= 0xe000 {
				return "", ErrInvalidMailboxName
			}
			sb.WriteRune(utf16.DecodeRune(rune(u), rune(lo)))
			i += 2
		case u >= 0xdc00 && u < 0xe000: // unpaired low surrogate
			return "", ErrInvalidMailboxName
		default:
			sb.WriteRune(rune(u))
		}
	}
	return sb.String(), nil
}
