package imapwire

import (
	"bufio"
	"io"
	"strings"
	"time"
)

// Decoder is a recursive-descent decoder for the IMAP wire grammar of RFC 3501
// section 9, extended with the additive productions of RFC 9051 section 9.
//
// # Error convention
//
// The decoder carries a sticky error: once any operation fails, every later one
// fails immediately without touching the stream, and [Decoder.Err] reports the
// cause. This is what lets a production be written as a straight sequence of
// calls with a single check at the end, rather than an error check per token.
//
// Two shapes of method exist, and the distinction is load-bearing:
//
//   - Optional matchers — Special, SP, Atom, Quoted, Literal, … — return false
//     when the next token is simply not of that shape, *without* recording an
//     error. They are the decoder's lookahead.
//   - Expect* methods return false and record an error when the token is
//     missing. They are used where the grammar leaves no choice.
//
// A matcher that has already consumed a committing byte (the opening quote of a
// quoted string, say) records an error rather than returning a bare false,
// because the input can no longer be reinterpreted.
//
// # Recovery
//
// [Decoder.DiscardLine] clears a non-fatal error and skips to the end of the
// current line, which is how a client survives a response it cannot parse. A
// fatal error — see [Error] — cannot be cleared: the decoder no longer knows
// where the next response begins.
//
// A Decoder is not safe for concurrent use.
type Decoder struct {
	r    *bufio.Reader
	opts Options

	err error
	eof bool

	// lineLen counts octets consumed on the current line, literal payloads
	// excluded. It bounds every token accumulator in the package: tokens are
	// appended byte by byte and each byte passes through this counter, so no
	// single token can allocate more than MaxLineLength.
	lineLen int

	// depth is the current parenthesised-list nesting.
	depth int

	// lit is the literal handed to the caller by Literal and not yet drained.
	// While it is set, every other operation fails: reading past it would
	// attribute payload octets to the next response.
	lit *LiteralReader

	// discardRaw is set when a quoted string has consumed its opening quote but
	// then proved malformed. Recovery must scan to the physical line ending in
	// that case: treating its closing quote as a new opening quote would consume
	// into the following response.
	discardRaw bool

	utf8Accept bool
}

// deadlineSetter is implemented by *net.Conn and by *tls.Conn.
type deadlineSetter interface {
	SetReadDeadline(t time.Time) error
}

// timeoutReader keeps an active read deadline on every underlying read, so that
// a server which announces a literal and then stalls cannot block the reader
// goroutine forever. It refreshes the deadline after half the timeout has
// elapsed; refreshing on every small buffer refill would allocate a network
// timer per chunk during a large streaming literal.
type timeoutReader struct {
	r        io.Reader
	setter   deadlineSetter
	timeout  time.Duration
	deadline time.Time
}

func (t *timeoutReader) Read(p []byte) (int, error) {
	now := time.Now()
	if t.deadline.IsZero() || !now.Add(t.timeout/2).Before(t.deadline) {
		t.deadline = now.Add(t.timeout)
		if err := t.setter.SetReadDeadline(t.deadline); err != nil {
			return 0, err
		}
	}
	return t.r.Read(p)
}

// NewDecoder returns a Decoder reading from r. A nil opts selects the defaults.
//
// If r implements SetReadDeadline and opts.ReadTimeout is non-zero, every read
// is bounded by that timeout.
func NewDecoder(r io.Reader, opts *Options) *Decoder {
	o := opts.withDefaults()
	if o.ReadTimeout > 0 {
		if ds, ok := r.(deadlineSetter); ok {
			r = &timeoutReader{r: r, setter: ds, timeout: o.ReadTimeout}
		}
	}
	return &Decoder{r: bufio.NewReader(r), opts: o, utf8Accept: o.UTF8Accept}
}

// NewDecoderString returns a Decoder over s. It is meant for decoding a
// fragment that has already been read off the wire, such as the arguments of a
// resp-text-code.
func NewDecoderString(s string, opts *Options) *Decoder {
	return NewDecoder(strings.NewReader(s), opts)
}

// SetUTF8Accept selects raw UTF-8 (true) or modified UTF-7 (false) for mailbox
// names. A client calls it after ENABLE UTF8=ACCEPT succeeds (RFC 9755).
func (d *Decoder) SetUTF8Accept(v bool) { d.utf8Accept = v }

// UTF8Accept reports the current mailbox-name encoding mode.
func (d *Decoder) UTF8Accept() bool { return d.utf8Accept }

// Options returns the limits in force, with defaults resolved.
func (d *Decoder) Options() Options { return d.opts }

// Err returns the sticky error, or nil.
func (d *Decoder) Err() error { return d.err }

// Fatal reports whether the sticky error left the stream desynchronised.
func (d *Decoder) Fatal() bool { return IsFatal(d.err) }

func (d *Decoder) fail(op, format string, args ...any) bool {
	if d.err == nil {
		d.err = newError(op, format, args...)
	}
	return false
}

// failFatal records a fatal error, escalating a non-fatal one if necessary.
func (d *Decoder) failFatal(op string, cause error, format string, args ...any) bool {
	if d.err == nil || !IsFatal(d.err) {
		d.err = newFatalError(op, cause, format, args...)
	}
	return false
}

// failEOF converts an exhausted stream into an error. A truncated response is
// always fatal: there is no position left to resynchronise to.
func (d *Decoder) failEOF(op string) bool {
	if d.err == nil {
		d.err = newFatalError(op, ErrUnexpectedEOF, "unexpected end of stream")
	}
	return false
}

// ready reports whether an operation may proceed: no sticky error, and no
// undrained literal outstanding.
func (d *Decoder) ready(op string) bool {
	if d.lit != nil {
		if d.lit.remaining > 0 {
			return d.failFatal(op, ErrLiteralPending,
				"%d octets of the pending literal are unread", d.lit.remaining)
		}
		d.lit = nil
	}
	return d.err == nil
}

// peek returns the next octet without consuming it. It reports false at end of
// stream — which is not by itself an error, see [Decoder.AtEOF] — and on I/O
// failure, which is.
func (d *Decoder) peek() (byte, bool) {
	if d.err != nil {
		return 0, false
	}
	b, err := d.r.ReadByte()
	if err != nil {
		if err == io.EOF {
			d.eof = true
		} else {
			d.failFatal("read", err, "read failed")
		}
		return 0, false
	}
	_ = d.r.UnreadByte()
	return b, true
}

// peekN returns up to n buffered octets without consuming them. A short result
// is not an error; callers use it only for lookahead decisions.
func (d *Decoder) peekN(n int) []byte {
	if d.err != nil {
		return nil
	}
	b, err := d.r.Peek(n)
	if err != nil && err != io.EOF && err != bufio.ErrBufferFull {
		d.failFatal("read", err, "read failed")
		return nil
	}
	return b
}

// consume advances one octet, charging it to the line-length budget. It must
// only be called after a successful peek.
func (d *Decoder) consume() bool {
	d.lineLen++
	if d.opts.MaxLineLength >= 0 && d.lineLen > d.opts.MaxLineLength {
		return d.failFatal("line", ErrLimitExceeded,
			"line longer than %d octets", d.opts.MaxLineLength)
	}
	if _, err := d.r.ReadByte(); err != nil {
		return d.failFatal("read", err, "read failed")
	}
	return true
}

// AtEOF reports whether the stream is exhausted. It is the only legitimate way
// for a response loop to end.
func (d *Decoder) AtEOF() bool {
	if d.err != nil {
		return false
	}
	_, ok := d.peek()
	return !ok && d.err == nil && d.eof
}

// Special consumes the octet b if it is next. It never records an error.
func (d *Decoder) Special(b byte) bool {
	if !d.ready("special") {
		return false
	}
	c, ok := d.peek()
	if !ok || c != b {
		return false
	}
	return d.consume()
}

// ExpectSpecial consumes the octet b, recording an error if it is absent.
func (d *Decoder) ExpectSpecial(b byte) bool {
	if d.Special(b) {
		return true
	}
	if d.err != nil {
		return false
	}
	if d.eof {
		return d.failEOF("special")
	}
	c, _ := d.peek()
	return d.fail("special", "expected %q, got %q", string(b), string(c))
}

// SP consumes one space. Runs of spaces are not folded: the grammar is exact
// about where SP appears, and a server that doubles one is producing input the
// caller should see rejected rather than silently repaired.
func (d *Decoder) SP() bool { return d.Special(' ') }

// ExpectSP consumes one space, recording an error if it is absent.
func (d *Decoder) ExpectSP() bool { return d.ExpectSpecial(' ') }

// CRLF consumes a line terminator and resets the line-length budget.
//
// A bare LF is accepted in addition to CRLF. This is a deliberate leniency:
// several deployed servers and more than one transparent proxy drop the CR, and
// no ambiguity arises because LF cannot appear anywhere else in the grammar.
func (d *Decoder) CRLF() bool {
	if !d.ready("CRLF") {
		return false
	}
	b, ok := d.peek()
	if !ok {
		return false
	}
	switch b {
	case '\n':
		if !d.consume() {
			return false
		}
		d.lineLen = 0
		return true
	case '\r':
		if !d.consume() {
			return false
		}
		c, ok := d.peek()
		if !ok {
			return d.failEOF("CRLF")
		}
		if c != '\n' {
			// CR is not a TEXT-CHAR, so it cannot be part of anything else.
			return d.fail("CRLF", "expected LF after CR, got %q", string(c))
		}
		if !d.consume() {
			return false
		}
		d.lineLen = 0
		return true
	}
	return false
}

// ExpectCRLF consumes a line terminator, recording an error if it is absent.
func (d *Decoder) ExpectCRLF() bool {
	if d.CRLF() {
		return true
	}
	if d.err != nil {
		return false
	}
	if d.eof {
		return d.failEOF("CRLF")
	}
	b, _ := d.peek()
	return d.fail("CRLF", "expected CRLF, got %q", string(b))
}

// readToken accumulates octets while class accepts them. The line-length
// counter bounds the allocation, so no explicit token cap is needed.
func (d *Decoder) readToken(class func(byte) bool) (string, bool) {
	var sb []byte
	for {
		b, ok := d.peek()
		if !ok || !class(b) {
			break
		}
		if !d.consume() {
			return "", false
		}
		sb = append(sb, b)
	}
	if d.err != nil {
		return "", false
	}
	return string(sb), len(sb) > 0
}

// Atom matches the atom production. It stops at "]", which belongs to
// resp-specials and therefore terminates an atom but not an astring.
func (d *Decoder) Atom(dst *string) bool {
	if !d.ready("atom") {
		return false
	}
	s, ok := d.readToken(isAtomChar)
	if !ok {
		return false
	}
	*dst = s
	return true
}

// ExpectAtom matches an atom, recording an error if none is present.
func (d *Decoder) ExpectAtom(dst *string) bool {
	if d.Atom(dst) {
		return true
	}
	return d.expectFailed("atom")
}

// ExpectFetchItemName decodes the leading name of a FETCH response item. BODY
// and BINARY are context-sensitive: '[' is an atom character in the general
// grammar, but starts their following section without intervening whitespace.
// Keeping that one exception here avoids treating "BODY[..." as an unknown
// atom and losing stream alignment at a literal.
func (d *Decoder) ExpectFetchItemName(dst *string) bool {
	if !d.ready("fetch-item") {
		return false
	}
	for _, name := range []string{"BINARY.SIZE", "BINARY", "BODY"} {
		p := d.peekN(len(name) + 1)
		if len(p) != len(name)+1 || p[len(name)] != '[' || !equalFold(string(p[:len(name)]), name) {
			continue
		}
		for range name {
			if !d.consume() {
				return false
			}
		}
		*dst = name
		return true
	}
	return d.ExpectAtom(dst)
}

func (d *Decoder) expectFailed(op string) bool {
	if d.err != nil {
		return false
	}
	if d.eof {
		return d.failEOF(op)
	}
	b, _ := d.peek()
	return d.fail(op, "expected %s, got %q", op, string(b))
}

// Quoted matches the quoted production. The opening double quote commits: a
// malformed body is reported as an error rather than a non-match.
func (d *Decoder) Quoted(dst *string) bool {
	if !d.ready("quoted") {
		return false
	}
	if !d.Special('"') {
		return false
	}
	var sb []byte
	for {
		b, ok := d.peek()
		if !ok {
			return d.failEOF("quoted")
		}
		if !isTextChar(b) {
			// Leave the line ending (or NUL) unread so DiscardLine can recover at
			// the correct physical response boundary.
			d.discardRaw = true
			return d.fail("quoted", "octet %#02x not allowed in a quoted string", b)
		}
		if !d.consume() {
			return false
		}
		switch {
		case b == '"':
			*dst = string(sb)
			return true
		case b == '\\':
			// QUOTED-CHAR allows exactly \" and \\ as escapes.
			c, ok := d.peek()
			if !ok {
				return d.failEOF("quoted")
			}
			if c != '"' && c != '\\' {
				d.discardRaw = true
				return d.fail("quoted", "invalid escape %q", `\`+string(c))
			}
			if !d.consume() {
				return false
			}
			sb = append(sb, c)
		default:
			sb = append(sb, b)
		}
	}
}

// ExpectQuoted matches a quoted string, recording an error if none is present.
func (d *Decoder) ExpectQuoted(dst *string) bool {
	if d.Quoted(dst) {
		return true
	}
	return d.expectFailed("quoted")
}

// String matches the string production — quoted or literal — and materialises
// the value in memory. A literal larger than Options.MaxBufferedLiteralSize is
// rejected; bulk data must be taken through [Decoder.Literal] instead.
func (d *Decoder) String(dst *string) bool {
	if !d.ready("string") {
		return false
	}
	if b, ok := d.peek(); ok && b == '"' {
		return d.Quoted(dst)
	}
	lr, ok := d.Literal()
	if !ok {
		return false
	}
	return d.bufferLiteral(lr, dst)
}

func (d *Decoder) bufferLiteral(lr *LiteralReader, dst *string) bool {
	max := d.opts.MaxBufferedLiteralSize
	if max >= 0 && lr.size > max {
		// The payload is still on the wire and unread, so this is fatal:
		// discarding it would mean reading it, which is what the limit forbids.
		return d.failFatal("literal", ErrLimitExceeded,
			"literal of %d octets exceeds the in-memory limit of %d", lr.size, max)
	}
	buf := make([]byte, lr.size)
	if _, err := io.ReadFull(lr, buf); err != nil {
		if d.err == nil {
			d.failFatal("literal", err, "reading literal payload")
		}
		return false
	}
	if d.err != nil {
		return false
	}
	*dst = string(buf)
	return true
}

// ExpectString matches a string, recording an error if none is present.
func (d *Decoder) ExpectString(dst *string) bool {
	if d.String(dst) {
		return true
	}
	return d.expectFailed("string")
}

// Astring matches the astring production: an atom that may contain "]", or a
// string.
func (d *Decoder) Astring(dst *string) bool {
	if !d.ready("astring") {
		return false
	}
	b, ok := d.peek()
	if !ok {
		return false
	}
	if b == '"' || b == '{' || d.literal8Ahead() {
		return d.String(dst)
	}
	s, ok := d.readToken(isAstringChar)
	if !ok {
		return false
	}
	*dst = s
	return true
}

// ExpectAstring matches an astring, recording an error if none is present.
func (d *Decoder) ExpectAstring(dst *string) bool {
	if d.Astring(dst) {
		return true
	}
	return d.expectFailed("astring")
}

// ExpectNString matches the nstring production: a string or the atom NIL.
// isNil, if non-nil, reports which of the two was found; dst is set to "" for
// NIL. Distinguishing them matters — an empty subject and an absent subject are
// different facts.
func (d *Decoder) ExpectNString(dst *string, isNil *bool) bool {
	if !d.ready("nstring") {
		return false
	}
	if d.nilToken() {
		*dst = ""
		if isNil != nil {
			*isNil = true
		}
		return true
	}
	if d.err != nil {
		return false
	}
	if isNil != nil {
		*isNil = false
	}
	return d.ExpectString(dst)
}

// nilToken consumes the atom NIL if it is next. The comparison is
// case-insensitive: the grammar spells it uppercase, servers do not always.
func (d *Decoder) nilToken() bool {
	b := d.peekN(3)
	if len(b) < 3 || !equalFold(string(b), "NIL") {
		return false
	}
	// Guard against an atom that merely starts with NIL, e.g. "NILE".
	if b4 := d.peekN(4); len(b4) == 4 && isAstringChar(b4[3]) {
		return false
	}
	for i := 0; i < 3; i++ {
		if !d.consume() {
			return false
		}
	}
	return true
}

// ListMailbox matches the list-mailbox production, which is an astring whose
// atom form additionally admits the wildcards "%" and "*". Only LIST and LSUB
// arguments use it; it exists here so that a captured command line round-trips.
func (d *Decoder) ListMailbox(dst *string) bool {
	if !d.ready("list-mailbox") {
		return false
	}
	b, ok := d.peek()
	if !ok {
		return false
	}
	if b == '"' || b == '{' || d.literal8Ahead() {
		return d.String(dst)
	}
	s, ok := d.readToken(isListChar)
	if !ok {
		return false
	}
	*dst = s
	return true
}

// Number matches the number production, a 32-bit unsigned decimal.
func (d *Decoder) Number(dst *uint32) bool {
	if !d.ready("number") {
		return false
	}
	v, ok := d.readNumber("number", 0xffffffff)
	if !ok {
		return false
	}
	*dst = uint32(v)
	return true
}

// ExpectNumber matches a number, recording an error if none is present.
func (d *Decoder) ExpectNumber(dst *uint32) bool {
	if d.Number(dst) {
		return true
	}
	return d.expectFailed("number")
}

// Number64 matches the number64 production of RFC 9051 (also RFC 8474), a
// 63-bit unsigned decimal.
func (d *Decoder) Number64(dst *int64) bool {
	if !d.ready("number64") {
		return false
	}
	v, ok := d.readNumber("number64", 1<<63-1)
	if !ok {
		return false
	}
	*dst = int64(v)
	return true
}

// ExpectNumber64 matches a number64, recording an error if none is present.
func (d *Decoder) ExpectNumber64(dst *int64) bool {
	if d.Number64(dst) {
		return true
	}
	return d.expectFailed("number64")
}

// ExpectNZNumber matches nz-number: a number that is not zero. Sequence numbers
// and UIDs use it, which is why zero is a grammar error rather than a value.
func (d *Decoder) ExpectNZNumber(dst *uint32) bool {
	if !d.ExpectNumber(dst) {
		return false
	}
	if *dst == 0 {
		return d.fail("nz-number", "expected a non-zero number")
	}
	return true
}

// ExpectUniqueID matches uniqueid, which is nz-number by another name.
func (d *Decoder) ExpectUniqueID(dst *uint32) bool { return d.ExpectNZNumber(dst) }

// readNumber accumulates a decimal, refusing to overflow max. The check is done
// before each multiply, so no intermediate value ever wraps.
func (d *Decoder) readNumber(op string, max uint64) (uint64, bool) {
	var v uint64
	n := 0
	for {
		b, ok := d.peek()
		if !ok || !isDigit(b) {
			break
		}
		if !d.consume() {
			return 0, false
		}
		digit := uint64(b - '0')
		if v > (max-digit)/10 {
			// Not fatal: the line is still self-delimiting, so a caller may
			// discard it and continue.
			d.fail(op, "number larger than %d", max)
			return 0, false
		}
		v = v*10 + digit
		n++
	}
	if d.err != nil || n == 0 {
		return 0, false
	}
	return v, true
}

// ExpectList decodes a parenthesised list, calling f once per element. f must
// consume exactly one element; the separating spaces and the closing paren are
// the decoder's business.
//
// Nesting is counted against Options.MaxListDepth here, which is the single
// place recursion can be entered from, so no grammar-specific depth bookkeeping
// is needed elsewhere.
func (d *Decoder) ExpectList(f func() error) error {
	if !d.ready("list") {
		return d.errOrSyntax("list")
	}
	if !d.ExpectSpecial('(') {
		return d.Err()
	}
	if d.opts.MaxListDepth >= 0 && d.depth >= d.opts.MaxListDepth {
		d.failFatal("list", ErrLimitExceeded,
			"list nested deeper than %d levels", d.opts.MaxListDepth)
		return d.Err()
	}
	d.depth++
	defer func() { d.depth-- }()

	if d.Special(')') {
		return nil
	}
	for {
		if err := f(); err != nil {
			return err
		}
		if d.err != nil {
			return d.err
		}
		if !d.SP() {
			break
		}
	}
	if !d.ExpectSpecial(')') {
		return d.Err()
	}
	return nil
}

// List decodes an optional parenthesised list. It reports false without an
// error if the next token is not "(".
func (d *Decoder) List(f func() error) (bool, error) {
	if !d.ready("list") {
		return false, d.errOrSyntax("list")
	}
	if b, ok := d.peek(); !ok || b != '(' {
		return false, d.Err()
	}
	return true, d.ExpectList(f)
}

func (d *Decoder) errOrSyntax(op string) error {
	if d.err != nil {
		return d.err
	}
	return newError(op, "decoder not ready")
}

// ExpectText matches the text production: the remainder of the line. It is
// permitted to be empty, which the rev1 grammar forbids but servers emit — an
// "OK" with an empty resp-text is harmless and rejecting it is not.
func (d *Decoder) ExpectText(dst *string) bool {
	if !d.ready("text") {
		return false
	}
	s, _ := d.readToken(isTextChar)
	if d.err != nil {
		return false
	}
	*dst = s
	return true
}

// DiscardValue skips exactly one syntactic element: a parenthesised list of any
// shape, a literal of any size, a quoted string, a number or an atom. A client
// uses it to step over response data it does not model, which is what keeps an
// unknown FETCH item from desynchronising the stream.
func (d *Decoder) DiscardValue() error {
	if !d.ready("value") {
		return d.errOrSyntax("value")
	}
	b, ok := d.peek()
	if !ok {
		if d.eof {
			d.failEOF("value")
		}
		return d.Err()
	}
	switch {
	case b == '(':
		return d.ExpectList(func() error { return d.DiscardValue() })
	case b == '"':
		var s string
		if !d.Quoted(&s) {
			return d.errOrSyntax("value")
		}
		return nil
	case b == '{' || d.literal8Ahead():
		lr, ok := d.Literal()
		if !ok {
			return d.errOrSyntax("value")
		}
		return lr.Discard()
	case isAstringChar(b):
		var s string
		if !d.readTokenInto(&s, isAstringChar) {
			return d.errOrSyntax("value")
		}
		return nil
	default:
		d.fail("value", "unexpected %q", string(b))
		return d.Err()
	}
}

func (d *Decoder) readTokenInto(dst *string, class func(byte) bool) bool {
	s, ok := d.readToken(class)
	if !ok {
		return false
	}
	*dst = s
	return true
}

// DiscardLine skips to the end of the current line, consuming any literals it
// meets, and clears a non-fatal error. It is the resynchronisation primitive: a
// client that cannot parse a response discards it and keeps the connection.
//
// A fatal error is not cleared and the line is not skipped — after one, the
// position of the next line is unknown by definition.
func (d *Decoder) DiscardLine() error {
	if d.err != nil {
		if IsFatal(d.err) {
			return d.err
		}
		d.err = nil
	}
	if d.discardRaw {
		d.discardRaw = false
		return d.discardPhysicalLine()
	}
	if d.lit != nil {
		if err := d.lit.Discard(); err != nil {
			return err
		}
	}
	for {
		b, ok := d.peek()
		if !ok {
			if d.eof {
				// A truncated final line is not worth a fatal error: there is
				// nothing left to desynchronise.
				return d.Err()
			}
			return d.Err()
		}
		switch b {
		case '\n':
			if !d.consume() {
				return d.Err()
			}
			d.lineLen = 0
			return nil
		case '"':
			var s string
			if !d.Quoted(&s) {
				return d.errOrSyntax("discard")
			}
		case '{', '~':
			// Only a genuine literal announcement may be skipped as one; "{" is
			// an ordinary TEXT-CHAR and appears in resp-text often enough that
			// treating every "{" as a literal would break real lines.
			if d.looksLikeLiteral() {
				lr, ok := d.Literal()
				if !ok {
					return d.errOrSyntax("discard")
				}
				if err := lr.Discard(); err != nil {
					return err
				}
				continue
			}
			if !d.consume() {
				return d.Err()
			}
		default:
			if !d.consume() {
				return d.Err()
			}
		}
	}
}

// discardPhysicalLine consumes through the next LF without interpreting any
// token. It is used only after a malformed quoted string, where the decoder is
// known to be inside that string and a literal-looking sequence is therefore
// text, not an actual literal announcement.
func (d *Decoder) discardPhysicalLine() error {
	for {
		b, ok := d.peek()
		if !ok {
			return d.Err()
		}
		if !d.consume() {
			return d.Err()
		}
		if b == '\n' {
			d.lineLen = 0
			return nil
		}
	}
}

// looksLikeLiteral reports whether the input at the current position is a
// literal announcement: "{" 1*DIGIT ["+"] "}" CRLF, optionally prefixed by "~".
func (d *Decoder) looksLikeLiteral() bool {
	// Peek only as far as the announcement requires. Peeking the maximum 25
	// bytes in one call would wait for bytes beyond a short announcement such as
	// "{0}\r\n", which can deadlock a streaming decoder even though it already
	// has enough input to make the decision.
	at := func(i int) (byte, bool) {
		b := d.peekN(i + 1)
		if len(b) <= i {
			return 0, false
		}
		return b[i], true
	}
	i := 0
	b, ok := at(i)
	if !ok {
		return false
	}
	if b == '~' {
		i++
	}
	b, ok = at(i)
	if !ok || b != '{' {
		return false
	}
	i++
	digits := 0
	for {
		b, ok = at(i)
		if !ok || !isDigit(b) {
			break
		}
		i++
		digits++
		if digits > 19 { // a number64 has at most 19 decimal digits
			return false
		}
	}
	if digits == 0 {
		return false
	}
	if b == '+' {
		i++
		b, ok = at(i)
	}
	if !ok || b != '}' {
		return false
	}
	i++
	b, ok = at(i)
	if !ok {
		return false
	}
	if b == '\n' {
		return true
	}
	if b != '\r' {
		return false
	}
	b, ok = at(i + 1)
	return ok && b == '\n'
}

// literal8Ahead reports whether the current "~" begins a literal8 rather than
// an ordinary atom. Tilde itself is an ATOM-CHAR, so treating every value that
// starts with it as a string would incorrectly reject valid values such as
// "~user".
func (d *Decoder) literal8Ahead() bool {
	b := d.peekN(2)
	return len(b) == 2 && b[0] == '~' && b[1] == '{'
}
