package imapserver

import (
	"context"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// I18NLEVEL=2 and the COMPARATOR command (RFC 5255 section 4).
//
// FILTERS (RFC 5466) is *not* here, and the reason is a boundary rather than a
// preference. A saved filter is referenced by a FILTER search key, and
// package imap has no criteria type for one — the client's FILTERS work
// recorded that gap and escalated it. Adding `imap.SearchFilter` is additive and
// therefore permitted after v1.0, but it is a change to the frozen root package
// and to the shared codec, which is a decision to take deliberately rather than
// as a side effect of an extension task. docs/RFC-COVERAGE.md records it.
//
// A comparator decides how string SEARCH keys are compared: whether "STRASSE"
// matches "straße", whether case and accents are folded. Only the backend knows,
// because it does the comparing — the framework never sees a search string
// matched against message content.
//
// I18NLEVEL=1 says the server compares with i;unicode-casemap. Level 2 adds the
// ability to *choose*, which is what COMPARATOR negotiates, so the capability is
// advertised only when a backend can honour more than the default.

// ComparatorSession is the optional COMPARATOR support of RFC 5255 section 4.2.
//
// Comparator names are an open registry (RFC 4790), so they cross the boundary
// as strings: a server supporting one this library has never heard of needs no
// change here.
type ComparatorSession interface {
	// Comparators reports the active comparator and the ones available, most
	// preferred first.
	Comparators(ctx context.Context, options *ComparatorOptions) (active string, available []string, err error)
	// SetComparator selects one of the offered names. The returned name is the
	// one adopted, empty when none of the requested names can be served.
	SetComparator(ctx context.Context, order []string, options *ComparatorOptions) (string, error)
}

// ComparatorOptions configures a COMPARATOR operation. A nil pointer selects
// the defaults.
// Construct with keyed fields only; fields may be added in a future release.
type ComparatorOptions struct{ _ struct{} }

func init() {
	registerCapabilities(
		capabilityDescriptor{
			Name:            "I18NLEVEL=2",
			States:          stateMaskAuthenticated | stateMaskSelected,
			Depends:         []string{"I18NLEVEL=1"},
			RequiresBackend: sessionImplements[ComparatorSession](),
		},
	)
	registerCommand("COMPARATOR", stateMaskAuthenticated|stateMaskSelected, false, parseComparator, handleComparator)
}

// parseComparator reads "COMPARATOR [order ...]". With no arguments it reports
// the current state rather than changing it. RFC 5255 section 4.7.
func parseComparator(decoder *imapwire.Decoder) (any, int64, error) {
	var order []string
	for decoder.SP() {
		var name string
		if !decoder.ExpectAstring(&name) {
			return nil, 0, decoder.Err()
		}
		order = append(order, name)
	}
	if !decoder.ExpectCRLF() {
		return nil, 0, decoder.Err()
	}
	return order, int64(len(order) * 32), nil
}

func handleComparator(ctx context.Context, c *conn, command *queuedCommand) error {
	order, _ := command.args.([]string)
	if err := requireCapability(c, "I18NLEVEL=2"); err != nil {
		return c.writeBad(command.tag, err.Error())
	}
	session, ok := c.state.session.(ComparatorSession)
	if !ok {
		return c.writeBad(command.tag, "COMPARATOR is not available")
	}
	if len(order) > 0 {
		adopted, err := session.SetComparator(ctx, order, nil)
		if err != nil {
			return writeBackendError(c, command.tag, command.name, err)
		}
		if adopted == "" {
			// RFC 5255 section 4.7 uses BADCOMPARATOR for a request naming
			// nothing the server can serve, so the client can distinguish it
			// from an ordinary failure and fall back.
			return writeTaggedCondition(c, command.tag, "NO", imap.ResponseCode("BADCOMPARATOR"), "",
				"no requested comparator is available")
		}
	}
	active, available, err := session.Comparators(ctx, nil)
	if err != nil {
		return writeBackendError(c, command.tag, command.name, err)
	}
	c.encoder.BeginResponse(imapwire.ResponseUntagged, "").Atom("COMPARATOR").SP().String(active)
	if len(available) > 0 {
		c.encoder.SP().List(len(available), func(i int) { c.encoder.String(available[i]) })
	}
	c.encoder.CRLF()
	if err := c.encoder.Flush(); err != nil {
		return err
	}
	return c.writeTagged(command.tag, "OK", command.name+" completed")
}
