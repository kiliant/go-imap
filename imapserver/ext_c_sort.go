package imapserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapcodec"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// SORT, SORT=DISPLAY and THREAD (RFC 5256, RFC 5957).
//
// Both are backend-delegated: ordering by subject requires the RFC 5256 base
// subject extraction, and threading requires the References and In-Reply-To
// graph, neither of which the framework has. The framework owns the request
// shape, the number space and the response encoding.

// SortMailbox is the optional SORT support of RFC 5256. A selected mailbox
// implements it when the backend witnesses SORT.
//
// The returned UIDs are in the requested order, and the framework preserves
// that order rather than sorting them — the ordering is the entire result.
type SortMailbox interface {
	Sort(ctx context.Context, query *SearchQuery, keys []imap.SortKeySpec, options *SortOptions) ([]imap.UID, error)
}

// ThreadMailbox is the optional THREAD support of RFC 5256.
//
// The returned nodes carry UIDs; the framework translates them to sequence
// numbers for a non-UID THREAD, which is why a backend never sees a sequence
// number here.
type ThreadMailbox interface {
	Thread(ctx context.Context, query *SearchQuery, algorithm imap.ThreadAlgorithm, options *ThreadOptions) ([]imap.ThreadNode, error)
}

// SortOptions configures SORT. A nil pointer selects the defaults.
// Construct with keyed fields only; fields may be added in a future release.
type SortOptions struct {
	// Charset is the declared charset for string criteria, or empty for the
	// protocol default.
	Charset string `imapfeature:"sort"`
	_       struct{}
}

// ThreadOptions configures THREAD. A nil pointer selects the defaults.
// Construct with keyed fields only; fields may be added in a future release.
type ThreadOptions struct {
	// Charset is the declared charset for string criteria, or empty for the
	// protocol default.
	Charset string `imapfeature:"thread"`
	_       struct{}
}

const (
	featureSort   featureID = "sort"
	featureThread featureID = "thread"
)

func init() {
	registerFeatures(
		featureDescriptor{ID: featureSort, Active: func(_ *sessionState, advertised map[string]bool) bool {
			return advertised["SORT"]
		}},
		featureDescriptor{ID: featureThread, Active: func(_ *sessionState, advertised map[string]bool) bool {
			return advertised["THREAD"]
		}},
	)
	registerCapabilities(
		capabilityDescriptor{
			Name:            "SORT",
			States:          stateMaskAuthenticated | stateMaskSelected,
			RequiresBackend: selectedImplements[SortMailbox]("SORT"),
		},
		// SORT=DISPLAY adds the DISPLAYFROM and DISPLAYTO keys of RFC 5957.
		// They are ordinary sort keys, so the backend witnesses the extra
		// support by name rather than through another interface.
		capabilityDescriptor{
			Name:            "SORT=DISPLAY",
			States:          stateMaskAuthenticated | stateMaskSelected,
			Depends:         []string{"SORT"},
			RequiresBackend: backendSupportsCapability("SORT=DISPLAY"),
		},
		capabilityDescriptor{
			Name:            "THREAD",
			States:          stateMaskAuthenticated | stateMaskSelected,
			RequiresBackend: selectedImplements[ThreadMailbox]("THREAD"),
		},
		// SEARCH=FUZZY (RFC 6203) adds the FUZZY search modifier. It reaches
		// the backend through the open criteria tree with no framework
		// translation, so advertising it is exactly a claim about the backend.
		capabilityDescriptor{
			Name:            "SEARCH=FUZZY",
			States:          stateMaskAuthenticated | stateMaskSelected,
			RequiresBackend: backendSupportsCapability("SEARCH=FUZZY"),
		},
	)
	registerCommand("SORT", stateMaskSelected, false, parseSort, handleSort)
	registerCommand("THREAD", stateMaskSelected, false, parseThread, handleThread)
	uidCommandDescriptors["SORT"] = &commandDescriptor{
		name: "SORT", states: stateMaskSelected, parse: parseSort, handle: handleSort,
	}
	uidCommandDescriptors["THREAD"] = &commandDescriptor{
		name: "THREAD", states: stateMaskSelected, parse: parseThread, handle: handleThread,
	}
}

// requireCapability refuses a command whose capability is not advertised to
// this session.
//
// Holding the optional interface is not sufficient on its own. A backend can
// implement SortMailbox while declining to witness SORT — the reference backend
// wrapped for tests does exactly that — and executing the command anyway would
// let a client use a capability the server never offered, which is precisely
// the drift the descriptor table exists to prevent.
func requireCapability(c *conn, name string) error {
	if advertisedCapabilities(c)[name] {
		return nil
	}
	return fmt.Errorf("%s is not available", name)
}

// selectedImplements builds a capability witness requiring the selected mailbox
// to implement an optional interface.
//
// Before a mailbox is selected there is nothing to test, so the capability is
// advertised on the strength of the backend's spoken witness; once selected, the
// interface must actually be there. This mirrors how MOVE is gated, and it
// matters for the same reason: a capability advertised in the authenticated
// state must not vanish or appear on selection for arbitrary reasons.
func selectedImplements[T any](name string) func(*sessionState, Backend) bool {
	spoken := backendSupportsCapability(name)
	return func(state *sessionState, backend Backend) bool {
		if !spoken(state, backend) {
			return false
		}
		if state == nil || state.selected == nil {
			return true
		}
		_, ok := state.selected.mailbox.(T)
		return ok
	}
}

type sortArgs struct {
	keys     []imap.SortKeySpec
	charset  string
	criteria imap.SearchCriteria
}

func parseSort(decoder *imapwire.Decoder) (any, int64, error) {
	if !decoder.ExpectSP() {
		return nil, 0, decoder.Err()
	}
	args := &sortArgs{}
	// RFC 5256 section 3: the sort key list comes first, then the charset,
	// which is mandatory here unlike in SEARCH.
	if err := decoder.ExpectList(func() error {
		var key string
		if !decoder.ExpectAtom(&key) {
			return decoder.Err()
		}
		spec := imap.SortKeySpec{}
		if strings.EqualFold(key, "REVERSE") {
			spec.Reverse = true
			if !decoder.ExpectSP() || !decoder.ExpectAtom(&key) {
				return decoder.Err()
			}
		}
		spec.Key = imap.SortKey(strings.ToUpper(key))
		args.keys = append(args.keys, spec)
		return nil
	}); err != nil {
		return nil, 0, err
	}
	if len(args.keys) == 0 {
		return nil, 0, fmt.Errorf("SORT requires at least one sort key")
	}
	if !decoder.ExpectSP() || !decoder.ExpectAstring(&args.charset) || !decoder.ExpectSP() {
		return nil, 0, decoder.Err()
	}
	criteria, err := imapcodec.ReadSearchCriteria(decoder)
	if err != nil {
		return nil, 0, err
	}
	if !decoder.ExpectCRLF() {
		return nil, 0, decoder.Err()
	}
	args.criteria = criteria
	return args, int64(len(args.charset) + len(args.keys)*16 + 64), nil
}

type threadArgs struct {
	algorithm imap.ThreadAlgorithm
	charset   string
	criteria  imap.SearchCriteria
}

func parseThread(decoder *imapwire.Decoder) (any, int64, error) {
	if !decoder.ExpectSP() {
		return nil, 0, decoder.Err()
	}
	args := &threadArgs{}
	var algorithm string
	if !decoder.ExpectAtom(&algorithm) || !decoder.ExpectSP() ||
		!decoder.ExpectAstring(&args.charset) || !decoder.ExpectSP() {
		return nil, 0, decoder.Err()
	}
	args.algorithm = imap.ThreadAlgorithm(strings.ToUpper(algorithm))
	criteria, err := imapcodec.ReadSearchCriteria(decoder)
	if err != nil {
		return nil, 0, err
	}
	if !decoder.ExpectCRLF() {
		return nil, 0, decoder.Err()
	}
	args.criteria = criteria
	return args, int64(len(args.charset)+len(algorithm)) + 64, nil
}

func handleSort(ctx context.Context, c *conn, command *queuedCommand) error {
	args, _ := command.args.(*sortArgs)
	if args == nil || args.criteria == nil {
		return c.writeBad(command.tag, "invalid SORT arguments")
	}
	if err := requireCapability(c, "SORT"); err != nil {
		return c.writeBad(command.tag, err.Error())
	}
	mailbox, ok := c.state.selected.mailbox.(SortMailbox)
	if !ok {
		return c.writeBad(command.tag, "SORT is not available")
	}
	query := newSearchQuery(args.criteria, c.state.selected.uids)
	uids, err := mailbox.Sort(ctx, query, args.keys, &SortOptions{Charset: args.charset})
	if err != nil {
		return writeBackendError(c, command.tag, command.name, err)
	}
	if len(uids) > maxCommandSearchResults {
		return writeTaggedCondition(c, command.tag, "NO", imap.CodeLimit, "", "SORT result limit exceeded")
	}
	// The backend's order is the answer, so the result is mapped in place
	// rather than sorted. A message that vanished between the search and the
	// response is dropped rather than reported at a stale sequence number.
	c.encoder.BeginResponse(imapwire.ResponseUntagged, "").Atom("SORT")
	for _, uid := range uids {
		seqNum, present := c.state.selected.sequence(uid)
		if !present {
			continue
		}
		if commandUsesUIDs(command) {
			c.encoder.SP().Number(uint32(uid))
		} else {
			c.encoder.SP().Number(uint32(seqNum))
		}
	}
	c.encoder.CRLF()
	if err := c.encoder.Flush(); err != nil {
		return err
	}
	if err := c.writeTagged(command.tag, "OK", command.name+" completed"); err != nil {
		return err
	}
	return c.drainUpdates(updateAccounting{})
}

func handleThread(ctx context.Context, c *conn, command *queuedCommand) error {
	args, _ := command.args.(*threadArgs)
	if args == nil || args.criteria == nil {
		return c.writeBad(command.tag, "invalid THREAD arguments")
	}
	if err := requireCapability(c, "THREAD"); err != nil {
		return c.writeBad(command.tag, err.Error())
	}
	mailbox, ok := c.state.selected.mailbox.(ThreadMailbox)
	if !ok {
		return c.writeBad(command.tag, "THREAD is not available")
	}
	query := newSearchQuery(args.criteria, c.state.selected.uids)
	roots, err := mailbox.Thread(ctx, query, args.algorithm, &ThreadOptions{Charset: args.charset})
	if err != nil {
		return writeBackendError(c, command.tag, command.name, err)
	}
	c.encoder.BeginResponse(imapwire.ResponseUntagged, "").Atom("THREAD")
	for i := range roots {
		c.encoder.SP()
		writeThreadNode(c, &roots[i], commandUsesUIDs(command))
	}
	c.encoder.CRLF()
	if err := c.encoder.Flush(); err != nil {
		return err
	}
	if err := c.writeTagged(command.tag, "OK", command.name+" completed"); err != nil {
		return err
	}
	return c.drainUpdates(updateAccounting{})
}

// writeThreadNode writes one thread tree.
//
// A node with Num zero is the anonymous container of RFC 5256's ((a)(b))
// grouping: it has no message of its own, so it contributes only its children's
// parentheses.
func writeThreadNode(c *conn, node *imap.ThreadNode, uidMode bool) {
	c.encoder.Special('(')
	if node.Num != 0 {
		number := node.Num
		if !uidMode {
			seqNum, present := c.state.selected.sequence(imap.UID(node.Num))
			if !present {
				number = 0
			} else {
				number = uint32(seqNum)
			}
		}
		if number != 0 {
			c.encoder.Number(number)
		}
	}
	for i := range node.Children {
		if node.Num != 0 || i > 0 {
			c.encoder.SP()
		}
		writeThreadNode(c, &node.Children[i], uidMode)
	}
	c.encoder.Special(')')
}
