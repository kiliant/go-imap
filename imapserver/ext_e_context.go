package imapserver

// ESORT and CONTEXT=SEARCH / CONTEXT=SORT (RFC 5267), and FILTERS (RFC 5466).
//
// # What is implemented, and what is not
//
// ESORT is the SORT counterpart of ESEARCH: it adds the MIN, MAX, ALL and COUNT
// return options to SORT. Those are computed from the ordered result the backend
// already returns, exactly as they are for SEARCH, so ESORT needs no backend
// surface beyond the SORT support that must be there anyway.
//
// CONTEXT=SEARCH and CONTEXT=SORT are a different proposition. They ask the
// server to hold a search result open and push incremental updates to it as the
// mailbox changes — a notification lifetime that outlives the command, like
// NOTIFY's and unlike anything the framework has today. They are therefore *not*
// advertised. Advertising them and then never sending an update would be worse
// than not offering them: a client using CONTEXT relies on the updates to keep
// its view correct, so silence reads as "nothing changed" rather than "not
// implemented".
//
// FILTERS (RFC 5466) names server-side saved search filters, which only a
// backend that stores them can serve. No optional interface is defined for it
// here: the extension's value is in the filters existing, and inventing an
// interface no backend asked for would be surface without a caller. It stays
// unadvertised and is recorded as such in docs/RFC-COVERAGE.md.

// SORT return options, from RFC 5267 section 4. They are the same tokens ESEARCH
// uses, which is deliberate on the RFC's part and lets the same computation
// serve both.
const (
	sortReturnMin   = "MIN"
	sortReturnMax   = "MAX"
	sortReturnAll   = "ALL"
	sortReturnCount = "COUNT"
)

func init() {
	registerCapabilities(
		// ESORT rides on SORT: without it there is no ordered result to return
		// a window of.
		capabilityDescriptor{
			Name:    "ESORT",
			States:  stateMaskAuthenticated | stateMaskSelected,
			Depends: []string{"SORT", "ESEARCH"},
		},
	)
}

// sortReturnSet resolves which data items an ESORT response carries, on the
// same terms as ESEARCH: an empty RETURN list means ALL.
func sortReturnSet(options []string) map[string]bool {
	requested := make(map[string]bool, len(options))
	for _, option := range options {
		requested[option] = true
	}
	if !requested[sortReturnMin] && !requested[sortReturnMax] &&
		!requested[sortReturnAll] && !requested[sortReturnCount] {
		requested[sortReturnAll] = true
	}
	return requested
}

// validateSortReturnOptions rejects options this server does not implement.
func validateSortReturnOptions(c *conn, options []string) error {
	if len(options) == 0 {
		return nil
	}
	if err := requireCapability(c, "ESORT"); err != nil {
		return err
	}
	for _, option := range options {
		switch option {
		case sortReturnMin, sortReturnMax, sortReturnAll, sortReturnCount:
		default:
			return errUnsupportedSortReturn(option)
		}
	}
	return nil
}

type unsupportedSortReturnError struct{ option string }

func (e *unsupportedSortReturnError) Error() string {
	return "unsupported SORT return option " + e.option
}

func errUnsupportedSortReturn(option string) error {
	return &unsupportedSortReturnError{option: option}
}
