package imapserver

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/kiliant/go-imap/internal/imapwire"
)

// URLAUTH (RFC 4467) and URL-PARTIAL (RFC 5550).
// URLAUTH=BINARY is not advertised until its extended syntax is implemented.
//
// URLAUTH lets a client hand a third party a URL that grants access to one
// message, without handing over the account. The authorization token is derived
// from a per-mailbox secret, so only the backend can mint or verify one — the
// framework owns the command syntax and nothing else.

// URLAuthSession is the optional URLAUTH support of RFC 4467.
type URLAuthSession interface {
	// GenerateURLAuth returns the authorized form of an IMAP URL: the URL with
	// its mechanism and authorization token appended to the unmodified rump URL.
	GenerateURLAuth(ctx context.Context, url, mechanism string, options *GenURLAuthOptions) (string, error)
	// FetchURLAuth verifies an authorized URL and returns its content. Nil
	// means it did not resolve and is reported as NIL, without failing other
	// URLs. A non-nil result must supply Body, including for empty content.
	// The framework closes Body exactly once, including on errors and refusal.
	// Reads must respect ctx; known lengths stream directly to the client.
	// Errors fail the command through the normal backend-error mapping; use a
	// nil result and nil error for an invalid or unauthorized URL.
	FetchURLAuth(ctx context.Context, url string, options *URLFetchOptions) (*URLFetchResult, error)
	// ResetURLAuthKey invalidates the secrets behind a mailbox's URLs. An empty
	// mailbox name revokes every mailbox key, per RFC 4467's RESETKEY command.
	ResetURLAuthKey(ctx context.Context, mailbox string, options *ResetURLAuthKeyOptions) error
}

// GenURLAuthOptions configures GENURLAUTH. A nil pointer selects the defaults.
// Construct with keyed fields only; fields may be added in a future release.
type GenURLAuthOptions struct{ _ struct{} }

// URLFetchOptions configures URLFETCH. A nil pointer selects the defaults.
// Construct with keyed fields only; fields may be added in a future release.
type URLFetchOptions struct{ _ struct{} }

// ResetURLAuthKeyOptions configures RESETKEY. A nil pointer selects the defaults.
// Construct with keyed fields only; fields may be added in a future release.
type ResetURLAuthKeyOptions struct {
	// Mechanisms selects the keys to replace for a nonempty mailbox. Empty
	// selects INTERNAL. With an empty mailbox all keys are revoked and no
	// mechanisms are supplied. Unknown mechanisms must fail without resetting
	// keys for supported mechanisms.
	Mechanisms []string `imapfeature:"urlauth"`
	_          struct{}
}

// URLFetchResult is resolved URL content. Body transfers to the framework,
// which closes it on every path, even when returned together with an error.
// Construct with keyed fields only; fields may be added in a future release.
type URLFetchResult struct {
	// Body supplies the resolved message or section bytes. It must be non-nil.
	Body io.ReadCloser
	// Size is the exact remaining octet count when HasSize is true. The body
	// must supply that many bytes; failure after literal framing closes the
	// connection. Negative known sizes are invalid. A known zero is empty
	// content, distinct from an unresolved URL (a nil result).
	Size int64
	// HasSize allows streaming without staging. Otherwise Body is spooled to
	// a bounded temporary file before the literal is announced.
	HasSize bool
	_       struct{}
}

const featureURLAuth featureID = "urlauth"

func init() {
	registerFeatures(featureDescriptor{ID: featureURLAuth, Active: func(_ *sessionState, advertised map[string]bool) bool { return advertised["URLAUTH"] }})
	registerCapabilities(
		capabilityDescriptor{
			Name:            "URLAUTH",
			States:          stateMaskAuthenticated | stateMaskSelected,
			RequiresBackend: sessionImplements[URLAuthSession](),
		},
		// URL-PARTIAL (RFC 5550) allows a byte range in the URL. The range is
		// part of the URL text the backend resolves, so the framework's whole
		// contribution is saying the server understands it.
		capabilityDescriptor{
			Name:            "URL-PARTIAL",
			States:          stateMaskAuthenticated | stateMaskSelected,
			Depends:         []string{"URLAUTH"},
			RequiresBackend: backendSupportsCapability("URL-PARTIAL"),
		},
	)
	registerCommand("GENURLAUTH", stateMaskAuthenticated|stateMaskSelected, false, parseGenURLAuth, handleGenURLAuth)
	registerCommand("URLFETCH", stateMaskAuthenticated|stateMaskSelected, false, parseURLFetch, handleURLFetch)
	registerCommand("RESETKEY", stateMaskAuthenticated|stateMaskSelected, false, parseResetKey, handleResetKey)
}

type genURLAuthPair struct{ url, mechanism string }
type genURLAuthArgs struct{ pairs []genURLAuthPair }

// parseGenURLAuth reads one or more rump-URL/mechanism pairs (RFC 4467 §9).
func parseGenURLAuth(decoder *imapwire.Decoder) (any, int64, error) {
	args := &genURLAuthArgs{}
	var size int64
	if !decoder.ExpectSP() {
		return nil, 0, decoder.Err()
	}
	for {
		var pair genURLAuthPair
		if !decoder.ExpectAstring(&pair.url) || !decoder.ExpectSP() || !decoder.ExpectAtom(&pair.mechanism) {
			return nil, 0, decoder.Err()
		}
		args.pairs = append(args.pairs, pair)
		size += int64(len(pair.url) + len(pair.mechanism))
		if !decoder.SP() {
			break
		}
	}
	if !decoder.ExpectCRLF() {
		return nil, 0, decoder.Err()
	}
	return args, size, nil
}

func parseURLFetch(decoder *imapwire.Decoder) (any, int64, error) {
	if !decoder.ExpectSP() {
		return nil, 0, decoder.Err()
	}
	var urls []string
	for {
		var url string
		if !decoder.ExpectAstring(&url) {
			return nil, 0, decoder.Err()
		}
		urls = append(urls, url)
		if !decoder.SP() {
			break
		}
	}
	if !decoder.ExpectCRLF() {
		return nil, 0, decoder.Err()
	}
	size := 0
	for _, url := range urls {
		size += len(url)
	}
	return urls, int64(size), nil
}

type resetURLAuthKeyArgs struct {
	mailbox string
	options ResetURLAuthKeyOptions
}

func parseResetKey(decoder *imapwire.Decoder) (any, int64, error) {
	args := &resetURLAuthKeyArgs{}
	var size int64
	if decoder.SP() {
		if !decoder.ExpectMailbox(&args.mailbox) {
			return nil, 0, decoder.Err()
		}
		size += int64(len(args.mailbox))
		for decoder.SP() {
			var mechanism string
			if !decoder.ExpectAtom(&mechanism) {
				return nil, 0, decoder.Err()
			}
			args.options.Mechanisms = append(args.options.Mechanisms, mechanism)
			size += int64(len(mechanism))
		}
	}
	if !decoder.ExpectCRLF() {
		return nil, 0, decoder.Err()
	}
	return args, size, nil
}

func handleGenURLAuth(ctx context.Context, c *conn, command *queuedCommand) error {
	args, _ := command.args.(*genURLAuthArgs)
	if args == nil || len(args.pairs) == 0 {
		return c.writeBad(command.tag, "invalid GENURLAUTH arguments")
	}
	session, err := urlAuthSession(c, command)
	if err != nil {
		return c.writeBad(command.tag, err.Error())
	}
	for _, pair := range args.pairs {
		if err := requireURLCapabilities(c, pair.url); err != nil {
			return c.writeBad(command.tag, err.Error())
		}
	}
	urls := make([]string, 0, len(args.pairs))
	for _, pair := range args.pairs {
		authorized, backendErr := session.GenerateURLAuth(ctx, pair.url, pair.mechanism, nil)
		if backendErr != nil {
			return writeBackendError(c, command.tag, command.name, backendErr)
		}
		urls = append(urls, authorized)
	}
	c.encoder.BeginResponse(imapwire.ResponseUntagged, "").Atom("GENURLAUTH")
	for _, authorized := range urls {
		c.encoder.SP().String(authorized)
	}
	c.encoder.CRLF()
	if err := c.encoder.Flush(); err != nil {
		return err
	}
	return c.writeTagged(command.tag, "OK", command.name+" completed")
}

func handleURLFetch(ctx context.Context, c *conn, command *queuedCommand) error {
	urls, _ := command.args.([]string)
	if len(urls) == 0 {
		return c.writeBad(command.tag, "invalid URLFETCH arguments")
	}
	session, err := urlAuthSession(c, command)
	if err != nil {
		return c.writeBad(command.tag, err.Error())
	}
	for _, url := range urls {
		if err := requireURLCapabilities(c, url); err != nil {
			return c.writeBad(command.tag, err.Error())
		}
	}
	remaining := c.server.options.Limits.MaxLiteralBytes
	var failure error
	c.encoder.BeginResponse(imapwire.ResponseUntagged, "").Atom("URLFETCH")
	for _, url := range urls {
		c.encoder.SP().String(url).SP()
		if failure != nil {
			c.encoder.NIL()
			continue
		}
		framed, err := writeURLFetchContent(ctx, c, session, url, &remaining)
		if err != nil {
			if framed {
				return err
			}
			failure = err
			c.encoder.NIL()
		}
	}
	c.encoder.CRLF()
	if err := c.encoder.Flush(); err != nil {
		return err
	}
	if failure != nil {
		return writeBackendError(c, command.tag, command.name, failure)
	}
	return c.writeTagged(command.tag, "OK", command.name+" completed")
}

func writeURLFetchContent(ctx context.Context, c *conn, session URLAuthSession, url string, remaining *int64) (framed bool, err error) {
	content, backendErr := session.FetchURLAuth(ctx, url, nil)
	if content != nil && content.Body != nil {
		defer content.Body.Close()
	}
	if backendErr != nil {
		return false, backendErr
	}
	if content == nil {
		c.encoder.NIL()
		return false, nil
	}
	if content.Body == nil {
		c.encoder.NIL()
		return false, nil
	}
	reader := io.Reader(&literalContextReader{ctx: ctx, reader: content.Body})
	size := content.Size
	if !content.HasSize {
		staged, count, cleanup, err := stageResponseLiteral(reader, *remaining, "urlfetch", resolvedContentTooBig())
		if err != nil {
			return false, err
		}
		defer cleanup()
		reader, size = staged, count
	}
	if size < 0 {
		return false, fmt.Errorf("URLFETCH backend returned a negative size")
	}
	if size > *remaining {
		return false, resolvedContentTooBig()
	}
	*remaining -= size
	literal, err := c.encoder.ResponseLiteral(size, false)
	if err != nil {
		return true, err
	}
	if _, err := io.CopyN(literal, reader, size); err != nil {
		return true, fmt.Errorf("stream URLFETCH literal: %w", err)
	}
	return true, literal.Close()
}

func handleResetKey(ctx context.Context, c *conn, command *queuedCommand) error {
	args, _ := command.args.(*resetURLAuthKeyArgs)
	if args == nil {
		return c.writeBad(command.tag, "invalid RESETKEY arguments")
	}
	session, err := urlAuthSession(c, command)
	if err != nil {
		return c.writeBad(command.tag, err.Error())
	}
	if backendErr := session.ResetURLAuthKey(ctx, args.mailbox, &args.options); backendErr != nil {
		return writeBackendError(c, command.tag, command.name, backendErr)
	}
	return c.writeTagged(command.tag, "OK", command.name+" completed")
}

func urlAuthSession(c *conn, _ *queuedCommand) (URLAuthSession, error) {
	if err := requireCapability(c, "URLAUTH"); err != nil {
		return nil, err
	}
	session, ok := c.state.session.(URLAuthSession)
	if !ok {
		return nil, errURLAuthUnavailable
	}
	return session, nil
}

var errURLAuthUnavailable = &urlAuthError{"URLAUTH is not available"}

func requireURLCapabilities(c *conn, url string) error {
	if strings.Contains(strings.ToUpper(url), ";PARTIAL=") {
		return requireCapability(c, "URL-PARTIAL")
	}
	return nil
}

type urlAuthError struct{ text string }

func (e *urlAuthError) Error() string { return e.text }
