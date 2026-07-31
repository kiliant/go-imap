package imapwire

import (
	"io"
	"unicode/utf8"
)

// ResponseKind distinguishes the three shapes a server line can take, per the
// response production of RFC 3501 section 9.
type ResponseKind int

const (
	// ResponseTagged is "<tag> SP resp-cond-state CRLF": a command completion.
	ResponseTagged ResponseKind = iota + 1
	// ResponseUntagged is "* ...": data, or a status update that belongs to no
	// particular command.
	ResponseUntagged
	// ResponseContinuation is "+ ...": the server is ready for a literal, for
	// the next SASL step, or for DONE.
	ResponseContinuation
)

func (k ResponseKind) String() string {
	switch k {
	case ResponseTagged:
		return "tagged"
	case ResponseUntagged:
		return "untagged"
	case ResponseContinuation:
		return "continuation"
	}
	return "unknown"
}

// isTagChar implements the tag production: 1*<any ASTRING-CHAR except "+">.
// Excluding "+" is what lets the framing distinguish a tag from a continuation
// request without lookahead.
func isTagChar(b byte) bool { return b != '+' && isAstringChar(b) }

// BeginResponse reads the leading token of a response line and reports what
// kind of line follows. The caller then decodes the remainder and finishes with
// [Decoder.ExpectCRLF]; framing and content are separate so that a body section
// can be streamed in between.
//
// It returns io.EOF, and no error of this package, when the stream ends cleanly
// at a line boundary — the normal end of a session.
func (d *Decoder) BeginResponse() (kind ResponseKind, tag string, err error) {
	if !d.ready("response") {
		return 0, "", d.errOrSyntax("response")
	}
	b, ok := d.peek()
	if !ok {
		if d.err == nil && d.eof {
			return 0, "", io.EOF
		}
		return 0, "", d.errOrSyntax("response")
	}
	switch b {
	case '+':
		if !d.consume() {
			return 0, "", d.Err()
		}
		return ResponseContinuation, "", nil
	case '*':
		if !d.consume() {
			return 0, "", d.Err()
		}
		return ResponseUntagged, "", nil
	}
	if !d.readTokenInto(&tag, isTagChar) {
		d.expectFailed("tag")
		return 0, "", d.Err()
	}
	return ResponseTagged, tag, nil
}

// ExpectContinuationText reads the remainder of a continuation request:
//
//	continue-req = "+" SP (resp-text / base64) CRLF
//
// The payload is returned verbatim — it is either human-readable text or a
// base64 SASL challenge, and only the caller knows which. The separating space
// is optional here because servers that have nothing to say send a bare "+".
func (d *Decoder) ExpectContinuationText(dst *string) bool {
	if !d.ready("continue-req") {
		return false
	}
	d.SP()
	if !d.ExpectText(dst) {
		return false
	}
	return d.ExpectCRLF()
}

// RespText is the resp-text production: an optional bracketed response code
// followed by human-readable text.
//
//	resp-text = ["[" resp-text-code "]" SP] text
//
// The code is split into its name and its raw arguments rather than being
// interpreted here. Response codes are the main extension point of IMAP — every
// other RFC adds one — so the wire layer hands over the text and lets the client
// decode the codes it knows with [NewDecoderString], while unknown codes survive
// intact instead of becoming a parse failure.
type RespText struct {
	// Code is the response code name, uppercase, or "" if there was none.
	Code string
	// Args is everything between the code name and the closing "]", or "".
	Args string
	// Text is the human-readable remainder of the line.
	Text string
}

// ExpectRespText decodes a resp-text.
func (d *Decoder) ExpectRespText(dst *RespText) bool {
	if !d.ready("resp-text") {
		return false
	}
	*dst = RespText{}
	if d.Special('[') {
		var code string
		if !d.ExpectAtom(&code) {
			return false
		}
		dst.Code = asciiUpper(code)
		if d.SP() {
			args, ok := d.respTextCodeArgs()
			if !ok {
				return false
			}
			dst.Args = args
		}
		if !d.ExpectSpecial(']') {
			return false
		}
		// The grammar requires SP after "]", but a server with nothing to add
		// after the code sometimes omits it along with the text.
		d.SP()
	}
	return d.ExpectText(&dst.Text)
}

// respTextCodeArgs reads the arguments of a response code up to the "]" that
// closes it.
//
// The generic production is <any TEXT-CHAR except "]">, but BADCHARSET and
// friends take astrings, which may be quoted strings containing "]". Tracking
// quoting and bracket depth accepts both readings; nothing in the grammar is
// made ambiguous by doing so.
func (d *Decoder) respTextCodeArgs() (string, bool) {
	var sb []byte
	depth := 0
	inQuote := false
	escaped := false
	for {
		b, ok := d.peek()
		if !ok {
			return "", d.failEOF("resp-text-code")
		}
		if !isTextChar(b) {
			break // CR or LF: ExpectSpecial(']') will report the truncation
		}
		if !inQuote && depth == 0 && b == ']' {
			break
		}
		if !d.consume() {
			return "", false
		}
		sb = append(sb, b)
		switch {
		case escaped:
			escaped = false
		case inQuote && b == '\\':
			escaped = true
		case b == '"':
			inQuote = !inQuote
		case !inQuote && b == '[':
			depth++
		case !inQuote && b == ']':
			depth--
		}
	}
	return string(sb), true
}

// RespCond is a status condition and its text. It covers resp-cond-state
// (OK/NO/BAD), resp-cond-auth (OK/PREAUTH) and resp-cond-bye (BYE), which differ
// only in which conditions the grammar permits at that point.
type RespCond struct {
	// Status is one of OK, NO, BAD, PREAUTH, BYE, uppercase.
	Status string
	Text   RespText
}

// ExpectRespCond decodes a status condition followed by its resp-text.
//
// Which conditions are legal depends on where in the grammar the caller is —
// PREAUTH only in the greeting, BYE only untagged — so that check is left to the
// caller, which knows its context. Anything outside the five known conditions is
// rejected here.
func (d *Decoder) ExpectRespCond(dst *RespCond) bool {
	if !d.ready("resp-cond") {
		return false
	}
	var status string
	if !d.ExpectAtom(&status) {
		return false
	}
	status = asciiUpper(status)
	switch status {
	case "OK", "NO", "BAD", "PREAUTH", "BYE":
	default:
		return d.fail("resp-cond", "unknown status condition %q", status)
	}
	dst.Status = status
	dst.Text = RespText{}
	// "OK" with neither text nor even a trailing space is not in the grammar,
	// but arrives from real servers on otherwise valid connections.
	if d.SP() {
		return d.ExpectRespText(&dst.Text)
	}
	return true
}

// ExpectMailbox decodes the mailbox production:
//
//	mailbox = "INBOX" / astring
//
// INBOX is case-insensitive by RFC 3501 section 5.1 and is normalised to
// uppercase here, so that comparisons upstream do not each have to remember. Any
// other name is decoded from modified UTF-7 unless UTF8=ACCEPT is in effect.
//
// A name that is not valid modified UTF-7 is returned verbatim rather than
// rejected. Servers do send raw UTF-8 without negotiating UTF8=ACCEPT, and a
// mailbox called "R&D" stored by such a server would otherwise be permanently
// unreachable; passing the bytes through costs nothing, since a name the client
// cannot decode it also cannot have created.
func (d *Decoder) ExpectMailbox(dst *string) bool {
	var raw string
	if !d.ExpectAstring(&raw) {
		return false
	}
	if equalFold(raw, "INBOX") {
		*dst = "INBOX"
		return true
	}
	if d.utf8Accept {
		if !utf8.ValidString(raw) {
			return d.fail("mailbox", "mailbox name is not valid UTF-8")
		}
		*dst = raw
		return true
	}
	name, err := DecodeMailboxName(raw)
	if err != nil {
		name = raw
	}
	*dst = name
	return true
}

// ExpectFlag decodes one flag:
//
//	flag        = "\Answered" / "\Flagged" / "\Deleted" / "\Seen" / "\Draft" /
//	              flag-keyword / flag-extension
//	flag-perm   = flag / "\*"
//	flag-extension = "\" atom
//
// The value is returned as it appeared on the wire, backslash included and case
// preserved. Flag names are case-insensitive, but normalising them is the job of
// the layer that owns the flag type, not of the codec.
//
// "\*" is accepted wherever a flag is: it is only legal in flag-perm and in
// mbx-list-flags, and rejecting it elsewhere would buy nothing but a lost
// mailbox listing.
func (d *Decoder) ExpectFlag(dst *string) bool {
	if !d.ready("flag") {
		return false
	}
	if d.Special('\\') {
		if d.Special('*') {
			*dst = `\*`
			return true
		}
		var name string
		if !d.ExpectAtom(&name) {
			return false
		}
		*dst = `\` + name
		return true
	}
	var kw string
	if !d.ExpectAtom(&kw) {
		return false
	}
	*dst = kw
	return true
}

// ExpectFlagList decodes a parenthesised list of flags, as used by FLAGS,
// PERMANENTFLAGS, the FETCH FLAGS item and mbx-list-flags. The result is
// non-nil even when the list is empty, so that "no flags" and "not reported"
// stay distinguishable upstream.
func (d *Decoder) ExpectFlagList(dst *[]string) error {
	flags := []string{}
	err := d.ExpectList(func() error {
		var f string
		if !d.ExpectFlag(&f) {
			return d.errOrSyntax("flag")
		}
		flags = append(flags, f)
		return nil
	})
	if err != nil {
		return err
	}
	*dst = flags
	return nil
}

func asciiUpper(s string) string {
	b := []byte(s)
	changed := false
	for i := range b {
		if u := toUpper(b[i]); u != b[i] {
			b[i] = u
			changed = true
		}
	}
	if !changed {
		return s
	}
	return string(b)
}
