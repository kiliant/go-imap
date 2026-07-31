package imapwire

import "time"

// Default limits. They are deliberately generous enough for real servers and
// small enough that a hostile one cannot exhaust memory: every one of them is
// checked *before* the corresponding allocation happens.
const (
	// DefaultMaxLiteralSize caps a streamed literal ({n} / ~{n}). 100 MB is
	// above any realistic message size while still refusing the classic
	// {4294967295} announcement.
	DefaultMaxLiteralSize = 100 << 20

	// DefaultMaxBufferedLiteralSize caps a literal that is materialised as an
	// in-memory string (the string / astring / nstring productions). Callers
	// that expect bulk data — FETCH BODY[] and friends — must take the
	// streaming [LiteralReader] instead, which is bounded by MaxLiteralSize.
	DefaultMaxBufferedLiteralSize = 8 << 20

	// DefaultMaxLineLength caps a single response line, excluding literal
	// payloads. RFC 9051 section 4 recommends that a server accept at least
	// 8000 octets in a command line; responses in practice stay well below
	// that, and everything bulky arrives as a literal.
	DefaultMaxLineLength = 8 << 10

	// DefaultMaxListDepth caps parenthesised-list nesting. The grammar puts no
	// bound on BODYSTRUCTURE nesting, so a server can otherwise drive the
	// recursive-descent decoder into a stack overflow.
	DefaultMaxListDepth = 100

	// DefaultMaxUntaggedPerCommand caps how many untagged responses a client
	// buffers for one command. Enforced by the client (it owns the buffers),
	// not by the decoder; the knob lives here so all wire limits are in one
	// place.
	DefaultMaxUntaggedPerCommand = 4096
)

// Options configures the decoder limits. The zero value is valid and selects
// every default; a nil *Options is likewise valid.
//
// Every limit is checked before the memory it guards is allocated. A field left
// at zero means "use the default"; a negative value means "no limit" and is only
// appropriate in tests.
type Options struct {
	// MaxLiteralSize is the largest literal, in octets, that may be announced
	// by a server. Bigger announcements are rejected without reading or
	// allocating anything.
	MaxLiteralSize int64

	// MaxBufferedLiteralSize is the largest literal that may be decoded into a
	// string in memory.
	MaxBufferedLiteralSize int64

	// MaxLineLength is the largest number of octets a single response line may
	// occupy, not counting literal payloads. It doubles as the bound on any
	// single token, since tokens are accumulated byte by byte through the same
	// counter.
	MaxLineLength int

	// MaxListDepth is the deepest parenthesised-list nesting accepted.
	MaxListDepth int

	// MaxUntaggedPerCommand is advisory; see [DefaultMaxUntaggedPerCommand].
	MaxUntaggedPerCommand int

	// ReadTimeout, if non-zero, is applied as a read deadline before every
	// underlying read, provided the reader implements
	//
	//	interface{ SetReadDeadline(time.Time) error }
	//
	// as *net.Conn does. It bounds a server that announces a literal and then
	// stalls.
	ReadTimeout time.Duration

	// UTF8Accept selects raw UTF-8 mailbox names instead of modified UTF-7
	// (RFC 9755, RFC 6855). It is normally toggled at runtime with
	// [Decoder.SetUTF8Accept] once ENABLE UTF8=ACCEPT has succeeded.
	UTF8Accept bool
}

// withDefaults returns a copy of opts with every unset field filled in.
func (opts *Options) withDefaults() Options {
	var o Options
	if opts != nil {
		o = *opts
	}
	if o.MaxLiteralSize == 0 {
		o.MaxLiteralSize = DefaultMaxLiteralSize
	}
	if o.MaxBufferedLiteralSize == 0 {
		o.MaxBufferedLiteralSize = DefaultMaxBufferedLiteralSize
	}
	if o.MaxLineLength == 0 {
		o.MaxLineLength = DefaultMaxLineLength
	}
	if o.MaxListDepth == 0 {
		o.MaxListDepth = DefaultMaxListDepth
	}
	if o.MaxUntaggedPerCommand == 0 {
		o.MaxUntaggedPerCommand = DefaultMaxUntaggedPerCommand
	}
	return o
}
