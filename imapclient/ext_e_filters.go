package imapclient

import (
	"errors"
	"fmt"
	"strings"

	"github.com/kiliant/go-imap"
)

// ParseUndefinedFilterArgs extracts the filter name from an UNDEFINED-FILTER
// response code. FILTERS, RFC 5466 section 3.1.
func ParseUndefinedFilterArgs(args string) (name string, err error) {
	name = strings.TrimSpace(args)
	if name == "" {
		return "", fmt.Errorf("UNDEFINED-FILTER response code requires a filter name")
	}
	// Filter names are astrings; servers typically send an atom. Strip one
	// layer of quoting when present.
	if len(name) >= 2 && name[0] == '"' && name[len(name)-1] == '"' {
		name = name[1 : len(name)-1]
	}
	if name == "" {
		return "", fmt.Errorf("UNDEFINED-FILTER response code requires a filter name")
	}
	return name, nil
}

// SupportsFilters reports the FILTERS capability. RFC 5466.
func (c *Client) SupportsFilters() bool { return c.Supports("FILTERS") }

// FilterSearchKey documents the FILTER search criterion. The criterion itself
// lives on [imap.SearchCriteria] (package imap, owned by T02); until a
// SearchFilter type lands there, callers cannot express FILTER without a core
// API addition — see the T11 escalation note.
//
// Metadata for named filters uses /private/vendor/<vendor-token>/filters/<name>
// entries via [Client.GetMetadata] / [Client.SetMetadata] (RFC 5466 section 3.2).
const FilterSearchKey = "FILTER"

// UndefinedFilterFromError returns the undefined filter name from err, if any.
func UndefinedFilterFromError(err error) (string, bool) {
	var ierr *imap.Error
	if !errors.As(err, &ierr) || ierr.Code != imap.CodeUndefinedFilter {
		return "", false
	}
	name, parseErr := ParseUndefinedFilterArgs(ierr.CodeArgs)
	if parseErr != nil {
		return "", false
	}
	return name, true
}
