package imapserver

import (
	"context"

	"github.com/kiliant/go-imap/internal/imapwire"
)

// URLAUTH (RFC 4467), URLAUTH=BINARY (RFC 5524) and URL-PARTIAL (RFC 5550).
//
// URLAUTH lets a client hand a third party a URL that grants access to one
// message, without handing over the account. The authorization token is derived
// from a per-mailbox secret, so only the backend can mint or verify one — the
// framework owns the command syntax and nothing else.

// URLAuthSession is the optional URLAUTH support of RFC 4467.
type URLAuthSession interface {
	// GenerateURLAuth returns the authorized form of an IMAP URL: the URL with
	// its access identifier and authorization token appended.
	GenerateURLAuth(ctx context.Context, url, mechanism string, options *URLAuthOptions) (string, error)
	// FetchURLAuth returns the content an authorized URL names, verifying the
	// token first.
	FetchURLAuth(ctx context.Context, url string, options *URLAuthOptions) ([]byte, error)
	// ResetURLAuthKey invalidates the secrets behind a mailbox's URLs. An empty
	// mailbox name resets every mailbox, which RFC 4467 section 5.1 defines as
	// the way to revoke everything at once.
	ResetURLAuthKey(ctx context.Context, mailbox string, options *URLAuthOptions) error
}

// URLAuthOptions configures a URLAUTH operation. A nil pointer selects the
// defaults.
// Construct with keyed fields only; fields may be added in a future release.
type URLAuthOptions struct{ _ struct{} }

func init() {
	registerCapabilities(
		capabilityDescriptor{
			Name:            "URLAUTH",
			States:          stateMaskAuthenticated | stateMaskSelected,
			RequiresBackend: sessionImplements[URLAuthSession](),
		},
		// URLAUTH=BINARY says URLFETCH can return the content with its
		// transfer encoding undone, which needs the same BINARY decoding the
		// backend already does for BINARY[].
		capabilityDescriptor{
			Name:            "URLAUTH=BINARY",
			States:          stateMaskAuthenticated | stateMaskSelected,
			Depends:         []string{"URLAUTH", "BINARY"},
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

type genURLAuthArgs struct{ url, mechanism string }

// parseGenURLAuth reads "GENURLAUTH url mechanism [url mechanism ...]".
// RFC 4467 section 5.2. Only the first pair is served; the rest are rejected
// rather than silently dropped.
func parseGenURLAuth(decoder *imapwire.Decoder) (any, int64, error) {
	if !decoder.ExpectSP() {
		return nil, 0, decoder.Err()
	}
	args := &genURLAuthArgs{}
	if !decoder.ExpectAstring(&args.url) || !decoder.ExpectSP() || !decoder.ExpectAtom(&args.mechanism) {
		return nil, 0, decoder.Err()
	}
	if !decoder.ExpectCRLF() {
		return nil, 0, decoder.Err()
	}
	return args, int64(len(args.url) + len(args.mechanism)), nil
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

func parseResetKey(decoder *imapwire.Decoder) (any, int64, error) {
	var mailbox string
	if decoder.SP() {
		if !decoder.ExpectMailbox(&mailbox) {
			return nil, 0, decoder.Err()
		}
	}
	if !decoder.ExpectCRLF() {
		return nil, 0, decoder.Err()
	}
	return mailbox, int64(len(mailbox)), nil
}

func handleGenURLAuth(ctx context.Context, c *conn, command *queuedCommand) error {
	args, _ := command.args.(*genURLAuthArgs)
	if args == nil {
		return c.writeBad(command.tag, "invalid GENURLAUTH arguments")
	}
	session, err := urlAuthSession(c, command)
	if err != nil {
		return c.writeBad(command.tag, err.Error())
	}
	authorized, backendErr := session.GenerateURLAuth(ctx, args.url, args.mechanism, nil)
	if backendErr != nil {
		return writeBackendError(c, command.tag, command.name, backendErr)
	}
	c.encoder.BeginResponse(imapwire.ResponseUntagged, "").Atom("GENURLAUTH").SP().String(authorized).CRLF()
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
	c.encoder.BeginResponse(imapwire.ResponseUntagged, "").Atom("URLFETCH")
	for _, url := range urls {
		content, backendErr := session.FetchURLAuth(ctx, url, nil)
		c.encoder.SP().Special('(').String(url).SP()
		// RFC 4467 section 5.3: a URL that cannot be resolved is reported as
		// NIL beside its URL rather than failing the command, so one bad URL
		// does not discard the others.
		if backendErr != nil || content == nil {
			c.encoder.NIL()
		} else {
			c.encoder.Literal8(string(content))
		}
		c.encoder.Special(')')
	}
	c.encoder.CRLF()
	if err := c.encoder.Flush(); err != nil {
		return err
	}
	return c.writeTagged(command.tag, "OK", command.name+" completed")
}

func handleResetKey(ctx context.Context, c *conn, command *queuedCommand) error {
	mailbox, _ := command.args.(string)
	session, err := urlAuthSession(c, command)
	if err != nil {
		return c.writeBad(command.tag, err.Error())
	}
	if backendErr := session.ResetURLAuthKey(ctx, mailbox, nil); backendErr != nil {
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

type urlAuthError struct{ text string }

func (e *urlAuthError) Error() string { return e.text }
