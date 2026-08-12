package imapwire

import (
	"errors"
	"fmt"
)

// Sentinel causes, reachable with [errors.Is] through the *[Error] returned by
// every decoder and encoder operation. The client maps them onto *imap.Error;
// this package cannot do so itself, because it must not import the root package
// (see docs/tasks/T01-wire-codec.md, "Layering rule: primitives only").
var (
	// ErrSyntax reports input that does not match the grammar.
	ErrSyntax = errors.New("imapwire: syntax error")

	// ErrLimitExceeded reports input that exceeds a configured limit: literal
	// size, line length or list nesting depth. It is fatal unless the input is a
	// synchronising command literal, whose payload has not been sent and can be
	// rejected at a known command boundary.
	ErrLimitExceeded = errors.New("imapwire: limit exceeded")

	// ErrLiteralPending reports an attempt to keep decoding while a literal
	// returned by [Decoder.Literal] has not been drained or discarded. Doing so
	// would attribute literal payload bytes to the next response.
	ErrLiteralPending = errors.New("imapwire: literal not drained")

	// ErrUnexpectedEOF reports a truncated response.
	ErrUnexpectedEOF = errors.New("imapwire: unexpected EOF")
)

// Error is the error type produced by this package.
//
// Fatal distinguishes the two recovery regimes. A non-fatal error means the
// decoder knows where the current line ends, so a caller may call
// [Decoder.DiscardLine] and carry on with the next response — the usual response
// to a syntactically odd but self-delimiting server line. A fatal error means the
// stream position is no longer known (a rejected literal count, an exceeded
// limit, an I/O failure); the connection is unusable and must be closed.
type Error struct {
	// Op names the grammar production or operation that failed, e.g. "quoted"
	// or "literal".
	Op string
	// Message describes the failure.
	Message string
	// Fatal reports that the stream is desynchronised.
	Fatal bool
	// Err is the underlying cause: one of the sentinels above, or an I/O error.
	Err error
}

func (e *Error) Error() string {
	s := "imapwire: " + e.Op + ": " + e.Message
	if e.Err != nil && !isSentinel(e.Err) {
		s += ": " + e.Err.Error()
	}
	return s
}

// Unwrap exposes the cause so that errors.Is(err, ErrLimitExceeded) and
// errors.Is(err, io.EOF) both work.
func (e *Error) Unwrap() error { return e.Err }

func isSentinel(err error) bool {
	switch err {
	case ErrSyntax, ErrLimitExceeded, ErrLiteralPending, ErrUnexpectedEOF:
		return true
	}
	return false
}

// IsFatal reports whether err leaves the stream desynchronised, meaning the
// connection cannot be reused. Any error that is not an *[Error] is treated as
// fatal, since its effect on the stream is unknown.
func IsFatal(err error) bool {
	if err == nil {
		return false
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Fatal
	}
	return true
}

func newError(op, format string, args ...any) *Error {
	return &Error{Op: op, Message: fmt.Sprintf(format, args...), Err: ErrSyntax}
}

func newFatalError(op string, cause error, format string, args ...any) *Error {
	return &Error{Op: op, Message: fmt.Sprintf(format, args...), Fatal: true, Err: cause}
}
