package imapwire

import (
	"io"
	"strings"
)

// SetLiteralDecision installs the server-side decision hook for literals
// encountered by String, Astring, Mailbox and other command productions. The
// hook runs after the announcement and before the payload is opened. For a
// synchronising literal it must send and flush the continuation before
// returning nil. Returning an error rejects that literal; a non-synchronising
// payload is drained first because it is already in flight.
//
// A nil hook restores response-decoder behaviour, where literals are opened
// immediately because a client never grants a continuation to a server.
func (d *Decoder) SetLiteralDecision(fn func(LiteralInfo) error) {
	d.literalDecision = fn
}

// BeginCommand reads the tag and command name at the start of one client
// command. The decoder is left immediately after the command name; the caller
// decodes command-specific arguments and finishes with [Decoder.ExpectCRLF].
//
// UID commands return "UID" as name. The command-specific decoder then reads
// the subcommand atom, which keeps this framing primitive independent of the
// command set.
//
// It returns io.EOF when the stream ends cleanly at a command boundary.
func (d *Decoder) BeginCommand() (tag, name string, err error) {
	if !d.ready("command") {
		return "", "", d.errOrSyntax("command")
	}
	b, ok := d.peek()
	if !ok {
		if d.err == nil && d.eof {
			return "", "", io.EOF
		}
		return "", "", d.errOrSyntax("command")
	}
	if b == '*' || b == '+' {
		d.fail("tag", "command tag may not start with %q", string(b))
		return "", "", d.Err()
	}
	if !d.readTokenInto(&tag, isTagChar) {
		d.expectFailed("tag")
		return "", "", d.Err()
	}
	if !d.ExpectSP() || !d.ExpectAtom(&name) {
		return tag, "", d.Err()
	}
	return tag, strings.ToUpper(name), nil
}

// SequenceSet matches the lexical sequence-set production. Semantic validation
// and the choice between sequence numbers and UIDs belong to the vocabulary
// layer, so this primitive returns the spelling unchanged.
func (d *Decoder) SequenceSet(dst *string) bool {
	return d.readTokenInto(dst, func(b byte) bool {
		return isDigit(b) || b == '*' || b == ':' || b == ','
	})
}

// ExpectSequenceSet matches a sequence set and records a syntax error when it
// is absent.
func (d *Decoder) ExpectSequenceSet(dst *string) bool {
	if d.SequenceSet(dst) {
		return true
	}
	return d.expectFailed("sequence-set")
}

// SPListAhead reports whether the next two octets are a space followed by an
// opening parenthesis. It resolves optional-argument grammar such as
// "PREVIEW (LAZY)" without consuming an enclosing list's separator.
func (d *Decoder) SPListAhead() bool {
	p := d.peekN(2)
	return len(p) == 2 && p[0] == ' ' && p[1] == '('
}
