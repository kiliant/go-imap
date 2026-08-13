package imapserver

import (
	"context"

	"github.com/kiliant/go-imap/internal/imapwire"
)

// LANGUAGE and I18NLEVEL (RFC 5255), and the referral capabilities of RFC 2221
// and RFC 2193.
//
// # Why the referrals need no code
//
// LOGIN-REFERRALS and MAILBOX-REFERRALS are response *codes*, and
// [imap.ResponseCode] is an open string type carried on [imap.Error]. A backend
// referring a client elsewhere returns an *imap.Error with the REFERRAL code and
// the framework already relays it verbatim. That is API rule 5 paying off: the
// extension is a data change, not a type change, so the only thing missing was
// the capability advertisement — which is what this file adds.

// LanguageSession is the optional LANGUAGE support of RFC 5255 section 3.
//
// A backend implements it when it can produce responses in more than one
// language. The framework does not translate anything itself: it has no message
// catalogue, and inventing one would put user-visible text in a protocol
// library.
type LanguageSession interface {
	// Languages reports the language tags this session can serve, most
	// preferred first.
	Languages(ctx context.Context, options *LanguageOptions) ([]string, error)
	// SetLanguage selects one of them. The returned tag is the one actually
	// adopted, which may differ from the request when the backend matched a
	// prefix.
	SetLanguage(ctx context.Context, tag string, options *LanguageOptions) (string, error)
}

// LanguageOptions configures a LANGUAGE operation. A nil pointer selects the
// defaults.
// Construct with keyed fields only; fields may be added in a future release.
type LanguageOptions struct{ _ struct{} }

func init() {
	registerCapabilities(
		// Referral support is a promise that the server may answer with a
		// REFERRAL response code. Only the backend knows whether it ever will.
		capabilityDescriptor{
			Name:            "LOGIN-REFERRALS",
			States:          stateMaskNotAuthenticated,
			RequiresBackend: backendSupportsCapability("LOGIN-REFERRALS"),
		},
		capabilityDescriptor{
			Name:            "MAILBOX-REFERRALS",
			States:          stateMaskAuthenticated | stateMaskSelected,
			RequiresBackend: backendSupportsCapability("MAILBOX-REFERRALS"),
		},
		capabilityDescriptor{
			Name:            "LANGUAGE",
			States:          stateMaskAuthenticated | stateMaskSelected,
			RequiresBackend: sessionImplements[LanguageSession](),
		},
		// I18NLEVEL=1 is the base internationalisation level of RFC 5255
		// section 4: the server compares strings with an i;unicode-casemap
		// comparator. I18NLEVEL=2 additionally requires the COMPARATOR command,
		// which is not implemented, so it is not advertised — claiming level 2
		// without COMPARATOR would fail the first client that used it.
		capabilityDescriptor{
			Name:            "I18NLEVEL=1",
			States:          stateMaskAuthenticated | stateMaskSelected,
			RequiresBackend: backendSupportsCapability("I18NLEVEL=1"),
		},
	)
	registerCommand("LANGUAGE", stateMaskAuthenticated|stateMaskSelected, false, parseLanguage, handleLanguage)
}

// parseLanguage reads the LANGUAGE command's optional tag list. With no tags the
// command reports what is available rather than selecting anything.
// RFC 5255 section 3.2.
func parseLanguage(decoder *imapwire.Decoder) (any, int64, error) {
	var tags []string
	for decoder.SP() {
		var tag string
		if !decoder.ExpectAstring(&tag) {
			return nil, 0, decoder.Err()
		}
		tags = append(tags, tag)
	}
	if !decoder.ExpectCRLF() {
		return nil, 0, decoder.Err()
	}
	return tags, int64(len(tags) * 16), nil
}

func handleLanguage(ctx context.Context, c *conn, command *queuedCommand) error {
	tags, _ := command.args.([]string)
	if err := requireCapability(c, "LANGUAGE"); err != nil {
		return c.writeBad(command.tag, err.Error())
	}
	session, ok := c.state.session.(LanguageSession)
	if !ok {
		return c.writeBad(command.tag, "LANGUAGE is not available")
	}
	if len(tags) == 0 {
		available, err := session.Languages(ctx, nil)
		if err != nil {
			return writeBackendError(c, command.tag, command.name, err)
		}
		c.encoder.BeginResponse(imapwire.ResponseUntagged, "").Atom("LANGUAGE").SP().
			List(len(available), func(i int) { c.encoder.String(available[i]) }).CRLF()
		if err := c.encoder.Flush(); err != nil {
			return err
		}
		return c.writeTagged(command.tag, "OK", command.name+" completed")
	}
	// RFC 5255 section 3.2: the server takes the first tag it can serve, and
	// reports which one that was — the client cannot assume it got its first
	// choice.
	for _, tag := range tags {
		adopted, err := session.SetLanguage(ctx, tag, nil)
		if err != nil || adopted == "" {
			continue
		}
		c.encoder.BeginResponse(imapwire.ResponseUntagged, "").Atom("LANGUAGE").SP().
			List(1, func(int) { c.encoder.String(adopted) }).CRLF()
		if err := c.encoder.Flush(); err != nil {
			return err
		}
		return c.writeTagged(command.tag, "OK", command.name+" completed")
	}
	return c.writeTagged(command.tag, "NO", "no requested language is available")
}
