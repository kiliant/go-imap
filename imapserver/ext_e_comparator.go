package imapserver

import (
	"context"
	"fmt"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// I18NLEVEL=2 and the COMPARATOR command (RFC 5255 section 4), and FILTERS
// (RFC 5466).
//
// FILTERS needed `imap.SearchFilter` in the root package, which the client's own
// FILTERS work had recorded as missing and escalated. Adding a type there after
// v1.0 is additive and permitted; reshaping one is not, and nothing was
// reshaped.
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
	// Comparators reports the active comparator and the ones available.
	Comparators(ctx context.Context, options *ComparatorOptions) (*ComparatorData, error)
	// SetComparator selects one of the offered names. A nil result means none of
	// the requested names can be served.
	SetComparator(ctx context.Context, order []string, options *ComparatorOptions) (*ComparatorResult, error)
}

// ComparatorOptions configures a COMPARATOR operation. A nil pointer selects
// the defaults.
// Construct with keyed fields only; fields may be added in a future release.
type ComparatorOptions struct{ _ struct{} }

// ComparatorData is a backend's report of its comparator state.
//
// It is a struct rather than two return values for the reason [LanguageResult]
// is one: RFC 4790 section 3.1 has a comparator declare which of equality,
// substring and ordering matching it supports, and RFC 5255 section 4.6 requires
// an ordering-capable comparator for SORT. A server offering both
// i;ascii-casemap and a collation that cannot order needs somewhere to say so,
// or the framework cannot refuse a SORT under the wrong one. Nothing reads that
// yet; the room for it is the point.
// Construct with keyed fields only; fields may be added in a future release.
type ComparatorData struct {
	// Active is the comparator currently in force.
	Active string
	// Available lists the comparators this session can adopt, most preferred
	// first.
	Available []string
	_         struct{}
}

// ComparatorResult is a backend's answer to a COMPARATOR selection.
// Construct with keyed fields only; fields may be added in a future release.
type ComparatorResult struct {
	// Active is the comparator actually adopted, which may differ from the
	// request when the backend matched a name it considers equivalent.
	Active string
	_      struct{}
}

// FilterSession is the optional FILTERS support of RFC 5466: named, server-side
// search filters a client references instead of restating the criteria.
//
// A filter is stored criteria, so only a backend that stores them can serve it.
// The framework substitutes the named filter into the search tree before the
// backend evaluates it, so a backend implements the lookup and nothing else.
type FilterSession interface {
	// Filter returns the criteria a named filter stands for. A nil result with
	// no error means the name is not defined.
	Filter(ctx context.Context, name string, options *FilterOptions) (imap.SearchCriteria, error)
}

// FilterOptions configures a filter lookup. A nil pointer selects the defaults.
// Construct with keyed fields only; fields may be added in a future release.
type FilterOptions struct{ _ struct{} }

func init() {
	registerCapabilities(
		capabilityDescriptor{
			Name:            "FILTERS",
			States:          stateMaskAuthenticated | stateMaskSelected,
			RequiresBackend: sessionImplements[FilterSession](),
		},
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
		if adopted == nil || adopted.Active == "" {
			// RFC 5255 section 4.7 uses BADCOMPARATOR for a request naming
			// nothing the server can serve, so the client can distinguish it
			// from an ordinary failure and fall back.
			return writeTaggedCondition(c, command.tag, "NO", imap.ResponseCode("BADCOMPARATOR"), "",
				"no requested comparator is available")
		}
	}
	data, err := session.Comparators(ctx, nil)
	if err != nil {
		return writeBackendError(c, command.tag, command.name, err)
	}
	if data == nil {
		return writeBackendError(c, command.tag, command.name,
			fmt.Errorf("imapserver: backend COMPARATOR returned nil"))
	}
	c.encoder.BeginResponse(imapwire.ResponseUntagged, "").Atom("COMPARATOR").SP().String(data.Active)
	if len(data.Available) > 0 {
		c.encoder.SP().List(len(data.Available), func(i int) { c.encoder.String(data.Available[i]) })
	}
	c.encoder.CRLF()
	if err := c.encoder.Flush(); err != nil {
		return err
	}
	return c.writeTagged(command.tag, "OK", command.name+" completed")
}

// applySearchFilters substitutes every FILTER key in a search tree with the
// criteria the backend stores under that name.
//
// An undefined name fails the command with UNDEFINED-FILTER rather than matching
// nothing: a filter the server does not know is a client error, and a silent
// empty result is indistinguishable from a correct search that matched nothing.
// RFC 5466 section 3.
func applySearchFilters(ctx context.Context, c *conn, criteria imap.SearchCriteria) (imap.SearchCriteria, error) {
	if !searchMentionsFilter(criteria) {
		return criteria, nil
	}
	if err := requireCapability(c, "FILTERS"); err != nil {
		return nil, err
	}
	return resolveSearchFilters(ctx, c, criteria, 0)
}

// maxFilterDepth bounds filter expansion. A filter whose criteria name further
// filters is legal, but a cycle between two of them is not detectable by
// inspection, so the depth is capped rather than trusted.
const maxFilterDepth = 8

func resolveSearchFilters(ctx context.Context, c *conn, criteria imap.SearchCriteria, depth int) (imap.SearchCriteria, error) {
	if depth > maxFilterDepth {
		return nil, &imap.Error{
			Type: imap.ErrorTypeNo,
			Code: imap.CodeUndefinedFilter,
			Text: "filter expansion is too deeply nested",
		}
	}
	switch node := criteria.(type) {
	case imap.SearchFilter:
		session, ok := c.state.session.(FilterSession)
		if !ok {
			return nil, &imap.Error{Type: imap.ErrorTypeNo, Code: imap.CodeUndefinedFilter, Text: "filters are not available"}
		}
		resolved, err := session.Filter(ctx, string(node), nil)
		if err != nil {
			return nil, err
		}
		if resolved == nil {
			return nil, &imap.Error{
				Type: imap.ErrorTypeNo,
				Code: imap.CodeUndefinedFilter,
				Text: "no such filter " + string(node),
			}
		}
		return resolveSearchFilters(ctx, c, resolved, depth+1)
	case imap.SearchAnd:
		resolved := make(imap.SearchAnd, len(node))
		for i, child := range node {
			child, err := resolveSearchFilters(ctx, c, child, depth)
			if err != nil {
				return nil, err
			}
			resolved[i] = child
		}
		return resolved, nil
	case imap.SearchOr:
		left, err := resolveSearchFilters(ctx, c, node.Left, depth)
		if err != nil {
			return nil, err
		}
		right, err := resolveSearchFilters(ctx, c, node.Right, depth)
		if err != nil {
			return nil, err
		}
		return imap.SearchOr{Left: left, Right: right}, nil
	case imap.SearchNot:
		inner, err := resolveSearchFilters(ctx, c, node.Criteria, depth)
		if err != nil {
			return nil, err
		}
		return imap.SearchNot{Criteria: inner}, nil
	default:
		return criteria, nil
	}
}

// searchMentionsFilter skips the substitution walk for the overwhelmingly common
// tree that names no filter.
func searchMentionsFilter(criteria imap.SearchCriteria) bool {
	switch node := criteria.(type) {
	case imap.SearchFilter:
		return true
	case imap.SearchAnd:
		for _, child := range node {
			if searchMentionsFilter(child) {
				return true
			}
		}
		return false
	case imap.SearchOr:
		return searchMentionsFilter(node.Left) || searchMentionsFilter(node.Right)
	case imap.SearchNot:
		return searchMentionsFilter(node.Criteria)
	default:
		return false
	}
}
