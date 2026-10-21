package imapserver

import (
	"context"
	"fmt"
	"strconv"
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
// Keys are classified by the framework before this call: unknown keys are
// refused, DISPLAYFROM/DISPLAYTO require SORT=DISPLAY, and RELEVANCY requires
// SEARCH=FUZZY and a FUZZY criterion. REVERSE applies to exactly one key.
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
			// Any advertised algorithm activates the feature: the THREAD
			// vocabulary is shared, and which algorithms are available is
			// settled per command by requireCapability below.
			return advertised["THREAD=ORDEREDSUBJECT"] || advertised["THREAD=REFERENCES"]
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
		// RFC 5256 section 1 spells the capability "THREAD=" thread-alg, and
		// defines no bare "THREAD" token. The distinction is not cosmetic: the
		// algorithms produce different trees from the same mailbox, so a client
		// has to know which one it will get before it can use the result. A
		// server advertising the bare form tells it nothing, and a client
		// looking for THREAD=ORDEREDSUBJECT never finds this server at all.
		//
		// One descriptor per algorithm, each witnessed by its own token, is
		// also what lets a backend implement one and not the other — which the
		// reference backend does, refusing REFERENCES rather than answering it
		// with ORDEREDSUBJECT results and silently mis-threading the client's
		// view. Found by the goimap interop entry (T24).
		capabilityDescriptor{
			Name:            "THREAD=ORDEREDSUBJECT",
			States:          stateMaskAuthenticated | stateMaskSelected,
			RequiresBackend: selectedImplements[ThreadMailbox]("THREAD=ORDEREDSUBJECT"),
		},
		capabilityDescriptor{
			Name:            "THREAD=REFERENCES",
			States:          stateMaskAuthenticated | stateMaskSelected,
			RequiresBackend: selectedImplements[ThreadMailbox]("THREAD=REFERENCES"),
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

// requireAnyCapability refuses a command unless at least one of the named
// capabilities is advertised.
//
// A few commands are defined by more than one extension and are available under
// any of them — CANCELUPDATE belongs to both halves of RFC 5267. Gating such a
// command on a single name makes it vanish for a backend that implements the
// other half.
func requireAnyCapability(c *conn, names ...string) error {
	advertised := advertisedCapabilities(c)
	for _, name := range names {
		if advertised[name] {
			return nil
		}
	}
	return fmt.Errorf("%s is not available", strings.Join(names, " or "))
}

// selectedImplements builds a capability witness requiring the selected mailbox
// to implement an optional interface.
//
// Before a mailbox is selected there is nothing to test, so the capability is
// advertised on the strength of the backend's spoken witness; once selected, the
// interface must actually be there. This mirrors how MOVE is gated, and it
// matters for the same reason: a capability advertised in the authenticated
// state must not vanish or appear on selection for arbitrary reasons.
func selectedImplements[T any](name string) func(context.Context, *sessionState, Backend) bool {
	spoken := backendSupportsCapability(name)
	return func(ctx context.Context, state *sessionState, backend Backend) bool {
		if !spoken(ctx, state, backend) {
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
	keys []imap.SortKeySpec
	// returnOptions carries ESORT's RETURN clause. See ext_e_context.go.
	returnOptions []string
	// partial is the PARTIAL window, RFC 5267 section 4.4 as extended by RFC
	// 9394. It windows the sorted result.
	partial  *searchPartialRange
	extended bool
	charset  string
	criteria imap.SearchCriteria
}

func parseSort(decoder *imapwire.Decoder) (any, int64, error) {
	if !decoder.ExpectSP() {
		return nil, 0, decoder.Err()
	}
	args := &sortArgs{}
	// ESORT puts a RETURN clause before the sort keys. RFC 5267 section 4.
	if decoder.PeekAtomEqual("RETURN") {
		var keyword string
		if !decoder.ExpectAtom(&keyword) || !decoder.ExpectSP() {
			return nil, 0, decoder.Err()
		}
		args.extended = true
		if err := decoder.ExpectList(func() error {
			var option string
			if !decoder.ExpectAtom(&option) {
				return decoder.Err()
			}
			option = strings.ToUpper(option)
			if option == searchReturnPartial {
				// The one option with an argument. As in SEARCH, a repeated
				// PARTIAL is accepted and the last range applies (#206).
				var raw string
				if !decoder.ExpectSP() || !decoder.ExpectAtom(&raw) {
					return decoder.Err()
				}
				window, err := parsePartialRange(raw)
				if err != nil {
					return err
				}
				args.partial = window
			}
			args.returnOptions = append(args.returnOptions, option)
			return nil
		}); err != nil {
			return nil, 0, err
		}
		if !decoder.ExpectSP() {
			return nil, 0, decoder.Err()
		}
	}
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
		if _, known := sortKeyGates[spec.Key]; !known {
			return fmt.Errorf("unknown SORT key %q", key)
		}
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
	if err := requireUIDSearchCriteria(c, args.criteria); err != nil {
		return writeBackendError(c, command.tag, command.name, err)
	}
	if err := requireUIDCommand(c, command); err != nil {
		return writeBackendError(c, command.tag, command.name, err)
	}
	if err := requireCapability(c, "SORT"); err != nil {
		return c.writeBad(command.tag, err.Error())
	}
	for _, key := range args.keys {
		gate, known := sortKeyGates[key.Key]
		if !known || !gate(&c.state, advertisedCapabilities(c)) {
			return c.writeBad(command.tag, fmt.Sprintf("SORT key %s is not available", key.Key))
		}
	}
	if err := validateSortReturnOptions(c, args.returnOptions); err != nil {
		return c.writeBad(command.tag, err.Error())
	}
	if err := validateUTF8AcceptCharset(c, args.charset, true); err != nil {
		return c.writeBad(command.tag, err.Error())
	}
	mailbox, ok := c.state.selected.mailbox.(SortMailbox)
	if !ok {
		return c.writeBad(command.tag, "SORT is not available")
	}
	// FILTER is a search key, so it is legal wherever one is. The tree is
	// substituted before the backend sees it, exactly as in SEARCH.
	criteria, err := applySearchFilters(ctx, c, args.criteria)
	if err != nil {
		return writeBackendError(c, command.tag, command.name, err)
	}
	if err := requireCriteriaCapabilities(c, criteria); err != nil {
		return writeBackendError(c, command.tag, command.name, err)
	}
	for _, key := range args.keys {
		if key.Key == imap.SortKeyRelevancy && !searchHasFuzzy(criteria) {
			return c.writeBad(command.tag, "SORT RELEVANCY requires FUZZY")
		}
	}
	usesModSeq := sortHasModSeqCriterion(criteria)
	if err := validateSelectedCondStoreUse(ctx, c, usesModSeq, "SORT MODSEQ"); err != nil {
		return writeCondStoreUseError(c, command.tag, command.name, err)
	}
	query := newSearchQuery(criteria, c.state.selected.uids, c.state.selected.savedSearch)
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
	numbers := make([]uint32, 0, len(uids))
	matchedUIDs := make([]imap.UID, 0, len(uids))
	for _, uid := range uids {
		seqNum, present := c.state.selected.sequence(uid)
		if !present {
			continue
		}
		matchedUIDs = append(matchedUIDs, uid)
		if commandUsesUIDs(command) {
			numbers = append(numbers, uint32(uid))
		} else {
			numbers = append(numbers, uint32(seqNum))
		}
	}
	var modSeq uint64
	if usesModSeq && !args.extended && len(matchedUIDs) != 0 {
		modSeq, err = sortResultModSeq(ctx, c, matchedUIDs)
		if err != nil {
			return writeBackendError(c, command.tag, command.name, err)
		}
	}
	if err := writeSortResponse(c, command, args, numbers, modSeq); err != nil {
		return err
	}
	if err := c.drainBeforeCompletion(command, updateAccounting{}); err != nil {
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
	if err := requireUIDSearchCriteria(c, args.criteria); err != nil {
		return writeBackendError(c, command.tag, command.name, err)
	}
	if err := requireUIDCommand(c, command); err != nil {
		return writeBackendError(c, command.tag, command.name, err)
	}
	// Gate on the algorithm the client actually asked for. The old bare-THREAD
	// gate let a request for an unimplemented algorithm through to the backend,
	// which could only answer with an error the client had no way to anticipate
	// from the capability list.
	if err := requireCapability(c, "THREAD="+strings.ToUpper(string(args.algorithm))); err != nil {
		return c.writeBad(command.tag, err.Error())
	}
	if err := validateUTF8AcceptCharset(c, args.charset, true); err != nil {
		return c.writeBad(command.tag, err.Error())
	}
	mailbox, ok := c.state.selected.mailbox.(ThreadMailbox)
	if !ok {
		return c.writeBad(command.tag, "THREAD is not available")
	}
	criteria, err := applySearchFilters(ctx, c, args.criteria)
	if err != nil {
		return writeBackendError(c, command.tag, command.name, err)
	}
	if err := requireCriteriaCapabilities(c, criteria); err != nil {
		return writeBackendError(c, command.tag, command.name, err)
	}
	query := newSearchQuery(criteria, c.state.selected.uids, c.state.selected.savedSearch)
	if err := validateSelectedCondStoreUse(ctx, c, searchHasModSeq(criteria), "THREAD MODSEQ"); err != nil {
		return writeCondStoreUseError(c, command.tag, command.name, err)
	}
	roots, err := mailbox.Thread(ctx, query, args.algorithm, &ThreadOptions{Charset: args.charset})
	if err != nil {
		return writeBackendError(c, command.tag, command.name, err)
	}
	// thread-data = "THREAD" [SP 1*thread-list]: the trees are adjacent.
	uidMode := commandUsesUIDs(command)
	c.encoder.BeginResponse(imapwire.ResponseUntagged, "").Atom("THREAD")
	separated := false
	for i := range roots {
		if !threadNodeWritable(c, &roots[i], uidMode) {
			continue
		}
		if !separated {
			c.encoder.SP()
			separated = true
		}
		writeThreadList(c, &roots[i], uidMode)
	}
	c.encoder.CRLF()
	if err := c.encoder.Flush(); err != nil {
		return err
	}
	if err := c.drainBeforeCompletion(command, updateAccounting{}); err != nil {
		return err
	}
	if err := c.writeTagged(command.tag, "OK", command.name+" completed"); err != nil {
		return err
	}
	return c.drainUpdates(updateAccounting{})
}

// writeSortResponse renders a SORT result in whichever of the two shapes the
// command asked for: the RFC 5256 number list, or ESORT's ESEARCH-shaped
// response when a RETURN clause was present.
//
// The ordered result is what makes MIN and MAX meaningful here: they are the
// first and last of the *sorted* order, not the numerically smallest and
// largest, which is exactly why RFC 5267 defines them separately for SORT.
func writeSortResponse(c *conn, command *queuedCommand, args *sortArgs, numbers []uint32, modSeq uint64) error {
	if !args.extended {
		c.encoder.BeginResponse(imapwire.ResponseUntagged, "").Atom("SORT")
		for _, number := range numbers {
			c.encoder.SP().Number(number)
		}
		if modSeq != 0 {
			c.encoder.SP().Special('(').Atom("MODSEQ").SP().Number64(int64(modSeq)).Special(')')
		}
		c.encoder.CRLF()
		return c.encoder.Flush()
	}
	requested := sortReturnSet(args.returnOptions)
	c.encoder.BeginResponse(imapwire.ResponseUntagged, "").Atom("ESEARCH").SP().
		Special('(').Atom("TAG").SP().Quoted(command.tag).Special(')')
	if commandUsesUIDs(command) {
		c.encoder.SP().Atom("UID")
	}
	if len(numbers) > 0 {
		if requested[sortReturnMin] {
			c.encoder.SP().Atom(sortReturnMin).SP().Number(numbers[0])
		}
		if requested[sortReturnMax] {
			c.encoder.SP().Atom(sortReturnMax).SP().Number(numbers[len(numbers)-1])
		}
		if requested[sortReturnAll] {
			// ALL keeps the sorted order; only ascending runs become ranges.
			c.encoder.SP().Atom(sortReturnAll).SP().Atom(numberListString(numbers))
		}
	}
	if requested[sortReturnCount] {
		c.encoder.SP().Atom(sortReturnCount).SP().Number(uint32(len(numbers)))
	}
	if args.partial != nil {
		// The window is taken from the sorted order and reported with the
		// requested range, or NIL when it holds nothing (RFC 9394 section 3.1).
		c.encoder.SP().Atom(searchReturnPartial).SP().Special('(').Atom(formatPartialRange(args.partial)).SP()
		if window := partialWindow(args.partial, numbers); len(window) > 0 {
			c.encoder.Atom(numberListString(window))
		} else {
			c.encoder.NIL()
		}
		c.encoder.Special(')')
	}
	c.encoder.CRLF()
	return c.encoder.Flush()
}

// numberListString renders a sorted result as a sequence-set that keeps its
// order. RFC 5267 section 3.2 expands every range in increasing order, so an
// ascending run of consecutive numbers may be written "3:5", while any other
// order must be spelled out: "5,3:4,2" is 5, 3, 4, 2.
func numberListString(numbers []uint32) string {
	var builder strings.Builder
	for i := 0; i < len(numbers); {
		end := i
		for end+1 < len(numbers) && numbers[end+1] == numbers[end]+1 {
			end++
		}
		if i > 0 {
			builder.WriteByte(',')
		}
		builder.WriteString(strconv.FormatUint(uint64(numbers[i]), 10))
		if end > i {
			builder.WriteByte(':')
			builder.WriteString(strconv.FormatUint(uint64(numbers[end]), 10))
		}
		i = end + 1
	}
	return builder.String()
}

// writeThreadList writes one thread tree as an RFC 5256 section 5 thread-list:
//
//	thread-list    = "(" (thread-members / thread-nested) ")"
//	thread-members = nz-number *(SP nz-number) [SP thread-nested]
//	thread-nested  = 2*thread-list
//
// A single reply continues its parent's number run, so a chain is "(1 2 3)",
// never "(1 (2 (3)))". Only two or more replies open nested lists, and those are
// adjacent with no SP between them. A node with Num zero is the anonymous
// container of the ((a)(b)) grouping; a message no longer in the selection is
// written the same way, contributing only its replies.
func writeThreadList(c *conn, node *imap.ThreadNode, uidMode bool) {
	c.encoder.Special('(')
	writeThreadMembers(c, node, uidMode)
	c.encoder.Special(')')
}

func writeThreadMembers(c *conn, node *imap.ThreadNode, uidMode bool) {
	number, ok := threadNumber(c, node, uidMode)
	if !ok {
		writeThreadReplies(c, node.Children, uidMode)
		return
	}
	c.encoder.Number(number)
	for len(node.Children) == 1 {
		next, ok := threadNumber(c, &node.Children[0], uidMode)
		if !ok {
			break
		}
		c.encoder.SP().Number(next)
		node = &node.Children[0]
	}
	if threadRepliesWritable(c, node.Children, uidMode) {
		c.encoder.SP()
		writeThreadReplies(c, node.Children, uidMode)
	}
}

// writeThreadReplies writes sibling replies. thread-nested needs at least two
// lists, so a lone writable reply continues the run instead.
func writeThreadReplies(c *conn, children []imap.ThreadNode, uidMode bool) {
	var writable []*imap.ThreadNode
	for i := range children {
		if threadNodeWritable(c, &children[i], uidMode) {
			writable = append(writable, &children[i])
		}
	}
	if len(writable) == 1 {
		writeThreadMembers(c, writable[0], uidMode)
		return
	}
	for _, child := range writable {
		writeThreadList(c, child, uidMode)
	}
}

func threadNumber(c *conn, node *imap.ThreadNode, uidMode bool) (uint32, bool) {
	if node.Num == 0 {
		return 0, false
	}
	if uidMode {
		return node.Num, true
	}
	seqNum, present := c.state.selected.sequence(imap.UID(node.Num))
	return uint32(seqNum), present
}

// threadNodeWritable reports whether a tree contributes at least one number;
// an empty list "()" is not a thread-list.
func threadNodeWritable(c *conn, node *imap.ThreadNode, uidMode bool) bool {
	if _, ok := threadNumber(c, node, uidMode); ok {
		return true
	}
	return threadRepliesWritable(c, node.Children, uidMode)
}

func threadRepliesWritable(c *conn, children []imap.ThreadNode, uidMode bool) bool {
	for i := range children {
		if threadNodeWritable(c, &children[i], uidMode) {
			return true
		}
	}
	return false
}

func sortHasModSeqCriterion(criteria imap.SearchCriteria) bool {
	return imapcodec.SearchCriteriaMentions(criteria, func(node imap.SearchCriteria) bool {
		_, ok := node.(imap.SearchModSeq)
		return ok
	})
}

// sortResultModSeq obtains clocks for exactly the returned UIDs through the
// existing FETCH contract. SORT's UID-only backend interface stays unchanged;
// a mailbox-wide HIGHESTMODSEQ could include messages outside this result.
func sortResultModSeq(ctx context.Context, c *conn, uids []imap.UID) (uint64, error) {
	wanted := imap.UIDSetNum(uids...)
	seen := make(map[imap.UID]bool, len(uids))
	var highest uint64
	writer := newFetchWriter(func(_ context.Context, data *imap.FetchMessageData) error {
		uid, ok := extractFetchUID(data)
		if !ok || !wanted.Contains(uid) {
			return fmt.Errorf("imapserver: SORT clock FETCH returned an unexpected UID")
		}
		for key, values := range data.Items {
			if !strings.EqualFold(string(key), "MODSEQ") {
				continue
			}
			for _, value := range values {
				if clock, ok := value.(imap.FetchDataModSeq); ok {
					if clock == 0 || uint64(clock) > 1<<63-1 {
						return fmt.Errorf("imapserver: SORT clock FETCH returned an invalid MODSEQ")
					}
					seen[uid] = true
					highest = max(highest, uint64(clock))
				}
			}
		}
		return nil
	})
	err := c.state.selected.mailbox.Fetch(ctx, writer, wanted, &FetchOptions{Items: []imap.FetchItem{imap.FetchItemUID, imap.FetchItemModSeq}})
	writer.core.close()
	if err != nil {
		return 0, err
	}
	for _, uid := range uids {
		if !seen[uid] {
			return 0, fmt.Errorf("imapserver: SORT clock FETCH omitted a result's MODSEQ")
		}
	}
	return highest, nil
}
