package imapserver

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/kiliant/go-imap/internal/imapwire"
)

// PARTIAL (RFC 9394): returning a window of a search result rather than all of
// it, so a client can page through a large mailbox.
//
// This is framework-owned. The backend still evaluates the whole search; only
// the response is windowed. That is honest about what it does and does not buy:
// it saves wire bytes and client memory, not backend work. A backend that can
// genuinely stop early would need a different interface, and RFC 9394 does not
// define one.

// searchPartialRange is a PARTIAL window. RFC 9394 section 3.1 counts from 1
// for a positive range and from the end for a negative one, and both ends are
// inclusive.
type searchPartialRange struct {
	from, to int64
}

func init() {
	registerCapabilities(
		// PARTIAL windows the ESEARCH response, so it needs ESEARCH's machinery
		// and nothing from the backend.
		capabilityDescriptor{
			Name:    "PARTIAL",
			States:  stateMaskAuthenticated | stateMaskSelected,
			Depends: []string{"ESEARCH"},
		},
	)
}

// parseSearchPartialRange reads PARTIAL's range argument.
func parseSearchPartialRange(decoder *imapwire.Decoder, args *searchArgs) error {
	if !decoder.ExpectSP() {
		return decoder.Err()
	}
	var raw string
	if !decoder.ExpectAtom(&raw) {
		return decoder.Err()
	}
	window, err := parsePartialRange(raw)
	if err != nil {
		return err
	}
	args.partial = window
	return nil
}

func parsePartialRange(raw string) (*searchPartialRange, error) {
	from, to, found := strings.Cut(raw, ":")
	if !found {
		return nil, fmt.Errorf("PARTIAL range %q is not a range", raw)
	}
	first, err := strconv.ParseInt(from, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("PARTIAL range %q: %w", raw, err)
	}
	last, err := strconv.ParseInt(to, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("PARTIAL range %q: %w", raw, err)
	}
	if first == 0 || last == 0 {
		return nil, fmt.Errorf("PARTIAL range %q: positions are 1-based", raw)
	}
	// RFC 9394 section 3.1: both ends must have the same sign, because one
	// counts from the start and the other from the end and a mixed range has no
	// meaning.
	if (first < 0) != (last < 0) {
		return nil, fmt.Errorf("PARTIAL range %q mixes positive and negative positions", raw)
	}
	return &searchPartialRange{from: first, to: last}, nil
}

// writeSearchPartial appends the PARTIAL return item to an ESEARCH response.
//
// RFC 9394 section 3.2 reports the requested range alongside the messages in it,
// so a client knows which window it received without having to assume the
// server honoured exactly what it asked for. An empty window is reported as NIL
// rather than omitted, which distinguishes "past the end of the result" from
// "the server ignored PARTIAL".
func writeSearchPartial(c *conn, args *searchArgs, numbers []uint32) {
	window := args.partial
	if window == nil {
		return
	}
	selected := partialWindow(window, numbers)
	c.encoder.SP().Atom(searchReturnPartial).SP().Special('(').
		Atom(formatPartialRange(window)).SP()
	if len(selected) == 0 {
		c.encoder.NIL()
	} else {
		c.encoder.Atom(numberSetString(selected))
	}
	c.encoder.Special(')')
}

// partialWindow selects the requested slice of an ordered result.
func partialWindow(window *searchPartialRange, numbers []uint32) []uint32 {
	count := int64(len(numbers))
	if count == 0 {
		return nil
	}
	from, to := window.from, window.to
	if from > to {
		from, to = to, from
	}
	// A negative range counts backwards from the end: -1 is the last message.
	if from < 0 {
		from, to = count+from+1, count+to+1
	}
	if from < 1 {
		from = 1
	}
	if to > count {
		to = count
	}
	if from > to || from > count {
		return nil
	}
	return numbers[from-1 : to]
}

func formatPartialRange(window *searchPartialRange) string {
	return strconv.FormatInt(window.from, 10) + ":" + strconv.FormatInt(window.to, 10)
}
