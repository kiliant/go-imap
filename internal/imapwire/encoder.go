package imapwire

import (
	"bufio"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// literalMinusMaxSize is the largest non-synchronising literal permitted when
// the server advertises LITERAL- rather than LITERAL+ (RFC 7888).
const literalMinusMaxSize = 4096

// EncoderOptions configures command serialisation. The zero value is valid: it
// selects synchronising literals and modified UTF-7 mailbox names, which every
// RFC 3501 server understands.
type EncoderOptions struct {
	// LiteralPlus enables the non-synchronising literal of RFC 7888 for
	// literals of any size. Set it when the server advertises LITERAL+.
	LiteralPlus bool

	// LiteralMinus enables the non-synchronising literal for payloads of at
	// most 4096 octets. Set it when the server advertises LITERAL-.
	LiteralMinus bool

	// UTF8Accept sends mailbox names as raw UTF-8 instead of modified UTF-7
	// (RFC 9755). Set it after ENABLE UTF8=ACCEPT succeeds.
	UTF8Accept bool

	// WaitContinuation blocks until the server has sent the continuation
	// request that a synchronising literal requires, and returns an error if
	// the server refused the command instead. Without it, only
	// non-synchronising literals can be written.
	//
	// It is a function rather than a channel so that the caller can decide what
	// "the server refused" means — a rejected APPEND and a dead connection need
	// different handling upstream.
	WaitContinuation func() error
}

// Encoder serialises IMAP commands.
//
// Like the decoder it carries a sticky error, so a command can be written as a
// chain of calls with one check at the end:
//
//	e.Atom(tag).SP().Atom("SELECT").SP().Mailbox(name).CRLF()
//	err := e.Flush()
//
// The value-writing methods choose the shortest representation the grammar
// allows for their argument — atom, then quoted string, then literal — which
// keeps command lines short and, more importantly, keeps a synchronising literal
// (a full network round trip) out of the common case.
//
// An Encoder is not safe for concurrent use.
type Encoder struct {
	w    *bufio.Writer
	opts EncoderOptions
	err  error
	lw   *LiteralWriter
}

// NewEncoder returns an Encoder writing to w. A nil opts selects the defaults.
func NewEncoder(w io.Writer, opts *EncoderOptions) *Encoder {
	e := &Encoder{w: bufio.NewWriter(w)}
	if opts != nil {
		e.opts = *opts
	}
	return e
}

// SetLiteralPlus enables or disables RFC 7888 LITERAL+ behaviour.
func (e *Encoder) SetLiteralPlus(v bool) { e.opts.LiteralPlus = v }

// SetLiteralMinus enables or disables RFC 7888 LITERAL- behaviour.
func (e *Encoder) SetLiteralMinus(v bool) { e.opts.LiteralMinus = v }

// SetWaitContinuation installs the callback used for synchronising literals.
// It is normally supplied by the connection layer immediately before issuing a
// command that owns a literal.
func (e *Encoder) SetWaitContinuation(fn func() error) { e.opts.WaitContinuation = fn }

// SetUTF8Accept selects raw UTF-8 (true) or modified UTF-7 (false) mailbox
// names.
func (e *Encoder) SetUTF8Accept(v bool) { e.opts.UTF8Accept = v }

// UTF8Accept reports the current mailbox-name encoding mode.
func (e *Encoder) UTF8Accept() bool { return e.opts.UTF8Accept }

// Err returns the sticky error, or nil.
func (e *Encoder) Err() error { return e.err }

// Flush writes any buffered output to the underlying writer.
func (e *Encoder) Flush() error {
	if e.err != nil {
		return e.err
	}
	if err := e.w.Flush(); err != nil {
		e.err = newFatalError("write", err, "flushing output")
		return e.err
	}
	return nil
}

func (e *Encoder) fail(op, format string, args ...any) *Encoder {
	if e.err == nil {
		e.err = newError(op, format, args...)
	}
	return e
}

// write is the single funnel for output, so the pending-literal interlock and
// the I/O error path exist in exactly one place.
func (e *Encoder) write(s string) *Encoder {
	if e.err != nil {
		return e
	}
	if e.lw != nil {
		return e.fail("literal", "%d octets of the pending literal are unwritten", e.lw.remaining)
	}
	if _, err := e.w.WriteString(s); err != nil {
		e.err = newFatalError("write", err, "writing output")
	}
	return e
}

// Special writes a single syntactic octet such as "(" or "]".
func (e *Encoder) Special(b byte) *Encoder { return e.write(string(b)) }

// SP writes one space.
func (e *Encoder) SP() *Encoder { return e.write(" ") }

// CRLF writes a line terminator. It does not flush; a caller that wants the
// line on the wire calls Flush.
func (e *Encoder) CRLF() *Encoder { return e.write("\r\n") }

// Atom writes s as a bare atom. It is for keywords the caller knows are atoms —
// command names, response codes, FETCH item names. An argument that is not a
// legal atom is a programming error and is reported as one rather than being
// quoted silently, because silently quoting a command name produces a
// mysterious BAD from the server. Use [Encoder.Astring] for values.
func (e *Encoder) Atom(s string) *Encoder {
	if e.err != nil {
		return e
	}
	if s == "" {
		return e.fail("atom", "empty atom")
	}
	for i := 0; i < len(s); i++ {
		if !isAtomChar(s[i]) || s[i] >= 0x80 {
			return e.fail("atom", "octet %#02x cannot appear in an atom", s[i])
		}
	}
	return e.write(s)
}

// Tag writes a command tag: an atom that additionally may not contain "+".
func (e *Encoder) Tag(s string) *Encoder {
	if e.err != nil {
		return e
	}
	if s == "" {
		return e.fail("tag", "empty tag")
	}
	for i := 0; i < len(s); i++ {
		if !isTagChar(s[i]) || s[i] >= 0x80 {
			return e.fail("tag", "octet %#02x cannot appear in a tag", s[i])
		}
	}
	return e.write(s)
}

// Quoted writes s as a quoted string. 8-bit octets, CR, LF and NUL are refused:
// the grammar restricts QUOTED-CHAR to 7-bit text, and a bare 8-bit octet inside
// a quoted string is the classic way to desynchronise a strict server. Values
// that may contain such octets go through [Encoder.String], which falls back to
// a literal.
func (e *Encoder) Quoted(s string) *Encoder {
	if e.err != nil {
		return e
	}
	if !canBeQuoted(s) {
		return e.fail("quoted", "value requires a literal")
	}
	buf := make([]byte, 0, len(s)+2)
	buf = append(buf, '"')
	for i := 0; i < len(s); i++ {
		if s[i] == '"' || s[i] == '\\' {
			buf = append(buf, '\\')
		}
		buf = append(buf, s[i])
	}
	buf = append(buf, '"')
	return e.write(string(buf))
}

// Number writes the number production.
func (e *Encoder) Number(n uint32) *Encoder { return e.write(strconv.FormatUint(uint64(n), 10)) }

// Number64 writes the number64 production. Negative values are refused.
func (e *Encoder) Number64(n int64) *Encoder {
	if n < 0 {
		return e.fail("number64", "negative number %d", n)
	}
	return e.write(strconv.FormatInt(n, 10))
}

// NIL writes the nil token of the nstring production.
func (e *Encoder) NIL() *Encoder { return e.write("NIL") }

// String writes the string production: a quoted string when the content allows
// it, a literal otherwise.
func (e *Encoder) String(s string) *Encoder {
	if canBeQuoted(s) {
		return e.Quoted(s)
	}
	return e.literalString(s, false)
}

// Astring writes the astring production, choosing the shortest legal form: an
// atom, else a quoted string, else a literal.
func (e *Encoder) Astring(s string) *Encoder {
	if canBeAtom(s, isAstringChar) {
		return e.write(s)
	}
	return e.String(s)
}

// NString writes the nstring production. isNil distinguishes an absent value
// from an empty one.
func (e *Encoder) NString(s string, isNil bool) *Encoder {
	if isNil {
		return e.NIL()
	}
	return e.String(s)
}

// Mailbox writes a mailbox name, encoding it as modified UTF-7 unless
// UTF8=ACCEPT is in effect.
func (e *Encoder) Mailbox(name string) *Encoder {
	if e.err != nil {
		return e
	}
	if e.opts.UTF8Accept {
		if !utf8.ValidString(name) {
			return e.fail("mailbox", "invalid UTF-8 mailbox name")
		}
	} else {
		enc, err := EncodeMailboxName(name)
		if err != nil {
			return e.fail("mailbox", "invalid mailbox name")
		}
		name = enc
	}
	return e.Astring(name)
}

// ListMailbox writes the list-mailbox production, the pattern argument of LIST
// and LSUB. It differs from Astring in that "%" and "*" survive in the atom
// form, which is what makes them wildcards rather than literal characters.
func (e *Encoder) ListMailbox(pattern string) *Encoder {
	if e.err != nil {
		return e
	}
	if e.opts.UTF8Accept {
		if !utf8.ValidString(pattern) {
			return e.fail("list-mailbox", "invalid UTF-8 mailbox pattern")
		}
	} else {
		enc, err := EncodeMailboxName(pattern)
		if err != nil {
			return e.fail("list-mailbox", "invalid mailbox pattern")
		}
		pattern = enc
	}
	if canBeAtom(pattern, isListChar) {
		return e.write(pattern)
	}
	return e.String(pattern)
}

// Flag writes a flag. The value is expected to carry its leading backslash, if
// it has one.
func (e *Encoder) Flag(flag string) *Encoder {
	if e.err != nil {
		return e
	}
	if flag == `\*` {
		return e.write(flag)
	}
	if len(flag) > 0 && flag[0] == '\\' {
		return e.write(`\`).Atom(flag[1:])
	}
	return e.Atom(flag)
}

// List writes a parenthesised list of n elements, calling f for each index.
func (e *Encoder) List(n int, f func(i int)) *Encoder {
	e.Special('(')
	for i := 0; i < n; i++ {
		if i > 0 {
			e.SP()
		}
		f(i)
	}
	return e.Special(')')
}

// DateTime writes a date-time as a quoted string, with the space-padded day the
// grammar requires.
func (e *Encoder) DateTime(t time.Time) *Encoder {
	if t.Year() < 0 || t.Year() > 9999 {
		return e.fail("date-time", "year %d does not fit date-year", t.Year())
	}
	return e.write(`"` + t.Format(dateTimeLayout) + `"`)
}

// Date writes a date in the bare date-text form, which SEARCH keys use.
func (e *Encoder) Date(t time.Time) *Encoder {
	if t.Year() < 0 || t.Year() > 9999 {
		return e.fail("date", "year %d does not fit date-year", t.Year())
	}
	// Format renders "_2" space-padded; date-day is 1*2DIGIT, so the pad is
	// dropped to keep the shortest legal spelling.
	s := t.Format(dateLayout)
	if len(s) > 0 && s[0] == ' ' {
		s = s[1:]
	}
	return e.write(s)
}

// BodySection writes a section, including any partial suffix.
func (e *Encoder) BodySection(s *BodySection) *Encoder {
	if e.err != nil {
		return e
	}
	if s == nil {
		return e.fail("section", "nil body section")
	}
	if len(s.Part) > maxSectionPartDepth {
		return e.fail("section-part", "more than %d part numbers", maxSectionPartDepth)
	}
	for _, n := range s.Part {
		if n == 0 {
			return e.fail("section-part", "part numbers must be non-zero")
		}
	}
	if s.Specifier == SpecifierMIME && len(s.Part) == 0 {
		return e.fail("section-spec", "MIME requires a section-part")
	}
	if (s.Specifier == SpecifierHeaderFields || s.Specifier == SpecifierHeaderFieldsNot) && len(s.Fields) == 0 {
		return e.fail("header-list", "empty header field list")
	}
	e.Special('[')
	for i, n := range s.Part {
		if i > 0 {
			e.Special('.')
		}
		e.Number(n)
	}
	if s.Specifier != SpecifierNone {
		if len(s.Part) > 0 {
			e.Special('.')
		}
		e.Atom(s.Specifier)
		if s.Specifier == SpecifierHeaderFields || s.Specifier == SpecifierHeaderFieldsNot {
			e.SP()
			e.List(len(s.Fields), func(i int) { e.Astring(s.Fields[i]) })
		}
	}
	e.Special(']')
	if p := s.Partial; p != nil {
		e.Special('<').Number(p.Offset)
		if p.Count > 0 {
			e.Special('.').Number(p.Count)
		}
		e.Special('>')
	}
	return e
}

// LiteralWriter accepts the payload of a literal written by
// [Encoder.Literal]. Exactly Size octets must be written before the encoder can
// be used for anything else; Close reports a short payload, which would
// otherwise leave the server waiting for octets that never arrive.
type LiteralWriter struct {
	e         *Encoder
	size      int64
	remaining int64
}

// Size returns the announced payload length.
func (w *LiteralWriter) Size() int64 { return w.size }

// Write implements [io.Writer]. Writing more than the announced size is an
// error: the surplus would be read by the server as the start of the next
// command.
func (w *LiteralWriter) Write(p []byte) (int, error) {
	if w.e.err != nil {
		return 0, w.e.err
	}
	if int64(len(p)) > w.remaining {
		w.e.fail("literal", "%d octets written past the announced size", int64(len(p))-w.remaining)
		return 0, w.e.err
	}
	n, err := w.e.w.Write(p)
	w.remaining -= int64(n)
	if err != nil {
		w.e.err = newFatalError("write", err, "writing literal payload")
		return n, w.e.err
	}
	if w.remaining == 0 {
		w.e.lw = nil
	}
	return n, nil
}

// Close reports whether the announced payload was written in full. It does not
// flush.
func (w *LiteralWriter) Close() error {
	if w.e.err != nil {
		return w.e.err
	}
	if w.remaining > 0 {
		w.e.lw = nil
		w.e.fail("literal", "%d octets of the announced payload were not written", w.remaining)
		return w.e.err
	}
	return nil
}

// Literal announces a literal of the given size and returns a writer for its
// payload.
//
// The form depends on the negotiated capabilities:
//
//	{n}\r\n    synchronising (RFC 3501): the caller blocks in WaitContinuation
//	           until the server sends "+", then writes the payload
//	{n+}\r\n   non-synchronising (RFC 7888 LITERAL+ / LITERAL-)
//	~{n}\r\n   literal8 (RFC 3516 BINARY, RFC 9755 UTF8=ACCEPT), which may
//	           contain NUL octets
//
// With LITERAL- the non-synchronising form is used only for payloads of at most
// 4096 octets, and never for literal8: the synchronising form is always legal,
// so where the extension's exact scope is not certain the encoder takes the
// conservative branch and pays one round trip.
func (e *Encoder) Literal(size int64, binary bool) (*LiteralWriter, error) {
	if e.err != nil {
		return nil, e.err
	}
	if e.lw != nil {
		e.fail("literal", "a literal is already open")
		return nil, e.err
	}
	if size < 0 {
		e.fail("literal", "negative literal size %d", size)
		return nil, e.err
	}
	sync := !e.nonSynchronising(size, binary)
	if sync && e.opts.WaitContinuation == nil {
		e.fail("literal", "a synchronising literal needs a continuation handler")
		return nil, e.err
	}

	if binary {
		e.write("~")
	}
	e.write("{").Number64(size)
	if !sync {
		e.write("+")
	}
	e.write("}\r\n")
	if e.err != nil {
		return nil, e.err
	}

	if sync {
		if err := e.Flush(); err != nil {
			return nil, err
		}
		if err := e.opts.WaitContinuation(); err != nil {
			// The command is now half-written; nothing further may be sent on
			// this connection until the caller resolves it.
			e.err = newFatalError("literal", err, "waiting for a continuation request")
			return nil, e.err
		}
	}

	lw := &LiteralWriter{e: e, size: size, remaining: size}
	if size > 0 {
		e.lw = lw
	}
	return lw, nil
}

// nonSynchronising decides whether the non-synchronising form may be used.
func (e *Encoder) nonSynchronising(size int64, binary bool) bool {
	switch {
	case e.opts.LiteralPlus:
		return true
	case e.opts.LiteralMinus:
		return !binary && size <= literalMinusMaxSize
	}
	return false
}

// literalString writes a whole in-memory value as a literal.
func (e *Encoder) literalString(s string, binary bool) *Encoder {
	if !binary && strings.IndexByte(s, 0) >= 0 {
		return e.fail("literal", "NUL requires literal8")
	}
	lw, err := e.Literal(int64(len(s)), binary)
	if err != nil {
		return e
	}
	if len(s) > 0 {
		if _, err := lw.Write([]byte(s)); err != nil {
			return e
		}
	}
	lw.Close()
	return e
}

// Literal8 writes a whole in-memory value as a literal8 (~{n}), which is how
// BINARY (RFC 3516) and UTF8=ACCEPT (RFC 9755) carry data that may contain NUL.
func (e *Encoder) Literal8(s string) *Encoder { return e.literalString(s, true) }
