package imapserver

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

type storeArgs struct {
	set    string
	op     StoreFlagsOp
	silent bool
	flags  []imap.Flag
	// unchangedSince is CONDSTORE's UNCHANGEDSINCE modifier, and
	// hasUnchangedSince its presence — zero is a legal value.
	// See ext_b_condstore.go.
	unchangedSince    uint64
	hasUnchangedSince bool
}

func parseStore(decoder *imapwire.Decoder) (any, int64, error) {
	if !decoder.ExpectSP() {
		return nil, 0, decoder.Err()
	}
	args := &storeArgs{}
	var operation string
	if !expectMessageSet(decoder, &args.set) || !decoder.ExpectSP() {
		return nil, 0, decoder.Err()
	}
	if err := parseStoreModifiers(decoder, args); err != nil {
		return nil, 0, err
	}
	if !decoder.ExpectAtom(&operation) || !decoder.ExpectSP() {
		return nil, 0, decoder.Err()
	}
	operation = strings.ToUpper(operation)
	if strings.HasSuffix(operation, ".SILENT") {
		args.silent = true
		operation = strings.TrimSuffix(operation, ".SILENT")
	}
	args.op = StoreFlagsOp(operation)
	if args.op != StoreFlagsSet && args.op != StoreFlagsAdd && args.op != StoreFlagsRemove {
		return nil, 0, fmt.Errorf("invalid STORE operation %q", operation)
	}
	// RFC 3501 section 9 and RFC 9051 section 9 both define
	//
	//	store-att-flags = (["+" / "-"] "FLAGS" [".SILENT"]) SP
	//	                  (flag-list / (flag *(SP flag)))
	//
	// so the parentheses are optional: "STORE 1 +FLAGS \Deleted" is as valid as
	// "STORE 1 +FLAGS (\Deleted)". Only the parenthesised form was accepted
	// until T24, which made every client using the bare form — Dovecot's
	// imaptest among them, which is how this was found — unable to set a flag
	// at all. Nothing in this repository generated the bare form, so no test
	// here had ever sent one.
	var rawFlags []string
	if decoder.PeekSpecial('(') {
		if err := decoder.ExpectFlagList(&rawFlags); err != nil {
			return nil, 0, err
		}
	} else {
		for {
			var flag string
			if !decoder.ExpectFlag(&flag) {
				return nil, 0, decoder.Err()
			}
			rawFlags = append(rawFlags, flag)
			if !decoder.SP() {
				break
			}
		}
	}
	for _, flag := range rawFlags {
		args.flags = append(args.flags, imap.Flag(flag))
	}
	if !decoder.ExpectCRLF() {
		return nil, 0, decoder.Err()
	}
	return args, int64(len(args.set) + len(operation) + len(args.flags)*16), nil
}

func handleStore(ctx context.Context, c *conn, command *queuedCommand) error {
	args, _ := command.args.(*storeArgs)
	if args == nil {
		return c.writeBad(command.tag, "invalid STORE arguments")
	}
	if c.state.selected.readOnly {
		return writeTaggedCondition(c, command.tag, "NO", imap.CodeReadOnly, "", "mailbox is read-only")
	}
	if err := requireUIDCommand(c, command); err != nil {
		return c.writeBad(command.tag, err.Error())
	}
	uidMode := commandUsesUIDs(command)
	uids, _, err := resolveMessageSet(c.state.selected, args.set, uidMode)
	if err != nil {
		return c.writeBad(command.tag, "invalid STORE message set")
	}
	if err := validateSelectedCondStoreUse(ctx, c, args.hasUnchangedSince, "STORE UNCHANGEDSINCE"); err != nil {
		return writeCondStoreUseError(c, command.tag, command.name, err)
	}
	// After the CONDSTORE check, which may itself enable CONDSTORE: from then
	// on RFC 7162 section 3.2.4 requires UID in STORE's untagged FETCH.
	includeUID := uidMode || condStoreRequiresUID(c)
	// An unconditional .SILENT store may omit every new value (RFC 3501
	// section 6.4.6), so its output is never collected. A conditional one
	// must still report MODSEQ for each stored message (RFC 7162 section
	// 3.1.3), and which messages were stored is known only once the backend
	// returns, so its output is collected and projected afterwards.
	discardOutput := args.silent && !args.hasUnchangedSince
	origin := nextCommandOrigin()
	responses := &storeResponses{}
	defer responses.cleanup()
	writer := newFetchWriter(func(_ context.Context, data *imap.FetchMessageData) error {
		if discardOutput {
			return nil
		}
		return responses.add(c, data, includeUID)
	})
	options := &StoreOptions{
		MutationOptions:   MutationOptions{Origin: origin},
		Silent:            args.silent,
		UnchangedSince:    args.unchangedSince,
		HasUnchangedSince: args.hasUnchangedSince,
	}
	flags := &StoreFlags{Op: args.op, Flags: args.flags}
	// A conditional store takes the CONDSTORE path so the backend can report
	// which messages it refused; an unconditional one stays on the base method.
	var condStore *CondStoreResult
	if args.hasUnchangedSince {
		condStore, err = storeCondStore(ctx, c, writer, uids, flags, options)
	} else {
		err = c.state.selected.mailbox.Store(ctx, writer, uids, flags, options)
	}
	writer.core.close()
	if err != nil {
		return writeBackendError(c, command.tag, command.name, err)
	}
	if args.hasUnchangedSince && args.silent {
		responses.silenceStored(condStore.Modified)
	}
	if !discardOutput && condStoreMailboxEnabled(c) {
		var modified imap.UIDSet
		if condStore != nil {
			modified = condStore.Modified
		}
		// A backend may honour Silent by writing nothing, so the framework
		// supplies the mandatory MODSEQ itself rather than requiring the
		// backend to ignore the option. MODSEQ is an existing CONDSTORE
		// FETCH obligation, and Fetch skips messages that no longer exist.
		if err := responses.fetchMissingModSeq(ctx, c, uids, modified, includeUID); err != nil {
			return writeBackendError(c, command.tag, command.name, err)
		}
	}
	accounting := updateAccounting{origin: origin, effect: effectStore, silent: args.silent}
	if !discardOutput && !responses.streaming {
		// Only what the response reported is accounted for. A changed
		// message the backend did not report keeps its unsolicited update.
		accounting.covered = responses.reported(args.silent, condStoreMailboxEnabled(c))
	}
	// A keyword must appear in the mailbox's untagged FLAGS response before it
	// first appears in a FETCH FLAGS response (RFC 3501 section 7.2.6). The
	// backend publishes the complete value in the command's update batch; peek
	// at that value here, while leaving the ordered batch queued for normal
	// revision accounting and delivery to every other selected connection.
	if mailboxFlags, ok := c.state.selected.queue.mailboxFlagsForOrigin(origin); ok &&
		!slices.Equal(c.state.selected.flags, mailboxFlags) {
		c.state.selected.flags = slices.Clone(mailboxFlags)
		if err := c.writeUpdate(deliveredUpdate{kind: updateMailboxFlags, flags: mailboxFlags}); err != nil {
			return err
		}
		if err := c.encoder.Flush(); err != nil {
			return err
		}
	}
	if responses.streaming {
		// The mutation succeeded. A buffer budget must not turn its completion
		// into NO or discard a conditional STORE's MODIFIED result.
		if err := streamStoreResponses(ctx, c, uids, condStore, args.silent, includeUID); err != nil {
			c.reportBackendError(command.name, err)
			_ = c.writeBye("unable to report STORE results")
			return err
		}
	} else {
		for _, response := range responses.items {
			if err := writeFetchLikeResponse(c, response.data); err != nil {
				return err
			}
		}
	}
	if len(responses.items) != 0 {
		if err := c.encoder.Flush(); err != nil {
			return err
		}
	}
	// UID STORE's MODIFIED code carries UIDs, so an expunge delivered here
	// cannot invalidate it.
	if err := c.drainBeforeCompletion(command, accounting); err != nil {
		return err
	}
	// RFC 7162 section 3.1.3: rejected messages are reported on a successful
	// tagged OK through MODIFIED, not as a command failure.
	if modified, ok := condStoreModifiedArgs(c, condStore, uidMode); ok {
		if err := writeTaggedCondition(c, command.tag, "OK", imap.CodeModified, modified, command.name+" completed"); err != nil {
			return err
		}
	} else if err := c.writeTagged(command.tag, "OK", command.name+" completed"); err != nil {
		return err
	}
	return c.drainUpdates(accounting)
}

// storeResponses collects STORE's untagged FETCH responses until the backend
// has returned, because what may be written depends on its result.
type storeResponses struct {
	items     []storeResponse
	bytes     int64
	streaming bool
}

const storeResponseOverhead int64 = 1024

type storeResponse struct {
	uid     imap.UID
	data    *imap.FetchMessageData
	cleanup func()
}

func (r *storeResponses) add(c *conn, data *imap.FetchMessageData, includeUID bool) error {
	if r.streaming {
		return nil
	}
	remaining := c.server.options.Limits.MaxStoreResponseBytes - r.bytes
	if remaining < storeResponseOverhead {
		r.streamInstead()
		return nil
	}
	uid, _ := extractFetchUID(data)
	mapped, err := mapFetchResponse(c.state.selected, data, includeUID)
	if err != nil {
		return err
	}
	mapped = stripModSeqUnlessEnabled(c, mapped)
	_, cleanup, err := prepareFetchResponseLiterals(mapped, remaining-storeResponseOverhead)
	if err != nil {
		return err
	}
	wireBytes, err := fetchResponseWireSize(mapped)
	if err != nil {
		cleanup()
		return err
	}
	if wireBytes > remaining-storeResponseOverhead {
		cleanup()
		r.streamInstead()
		return nil
	}
	r.bytes += wireBytes + storeResponseOverhead
	r.items = append(r.items, storeResponse{uid: uid, data: mapped, cleanup: cleanup})
	return nil
}

func (r *storeResponses) streamInstead() {
	r.cleanup()
	r.bytes = 0
	r.streaming = true
}

// Refetch the requested state with the existing FETCH contract after the
// STORE result is known. Memory stays independent of the number of responses;
// .SILENT projection and MODIFIED retain their ordinary meanings.
func streamStoreResponses(ctx context.Context, c *conn, uids imap.UIDSet, result *CondStoreResult, silent, includeUID bool) error {
	items := []imap.FetchItem{imap.FetchItemUID, imap.FetchItemFlags}
	if condStoreMailboxEnabled(c) {
		items = append(items, imap.FetchItemModSeq)
	}
	writer := newFetchWriter(func(_ context.Context, data *imap.FetchMessageData) error {
		mapped, err := mapFetchResponse(c.state.selected, data, includeUID)
		if err != nil {
			return err
		}
		uid, _ := extractFetchUID(data)
		if silent && (result == nil || !result.Modified.Contains(uid)) {
			deleteFetchItem(mapped, imap.FetchItemFlags)
		}
		mapped = stripModSeqUnlessEnabled(c, mapped)
		if condStoreMailboxEnabled(c) && !hasFetchItem(mapped, imap.FetchItemModSeq) {
			return fmt.Errorf("imapserver: STORE result FETCH omitted MODSEQ")
		}
		if err := writeFetchLikeResponse(c, mapped); err != nil {
			return err
		}
		return c.encoder.Flush()
	})
	err := c.state.selected.mailbox.Fetch(ctx, writer, uids, &FetchOptions{Items: items})
	writer.core.close()
	return err
}

func (r *storeResponses) cleanup() {
	for _, response := range r.items {
		response.cleanup()
	}
	r.items = nil
}

// silenceStored applies .SILENT to a conditional store: the new FLAGS of the
// stored messages are omitted, their MODSEQ (and UID) kept. A message named
// in MODIFIED was not stored, so data the backend wrote for it is the
// unsolicited report of a change by someone else, which RFC 7162 section
// 3.1.3 recommends sending and .SILENT does not cover.
func (r *storeResponses) silenceStored(modified imap.UIDSet) {
	kept := r.items[:0]
	for _, response := range r.items {
		if !modified.Contains(response.uid) {
			deleteFetchItem(response.data, imap.FetchItemFlags)
		}
		if len(response.data.Items) == 0 {
			response.cleanup()
			continue
		}
		kept = append(kept, response)
	}
	r.items = kept
}

// fetchMissingModSeq reports MODSEQ for every stored message whose collected
// response lacks it. Stored means requested and not named in MODIFIED;
// a rejected message also needs MODSEQ if the backend wrote its FLAGS.
func (r *storeResponses) fetchMissingModSeq(ctx context.Context, c *conn, requested, modified imap.UIDSet, includeUID bool) error {
	if r.streaming {
		return nil
	}
	have := make(map[imap.UID]bool, len(r.items))
	byUID := make(map[imap.UID][]int, len(r.items))
	for i, response := range r.items {
		byUID[response.uid] = append(byUID[response.uid], i)
	}
	// A backend may split a message's output across responses. Fill each
	// response, and index by UID so a FLAGS-only mailbox stays linear in size.
	merge := func(uid imap.UID, data *imap.FetchMessageData) (bool, error) {
		if r.streaming {
			return true, nil
		}
		if !hasFetchItem(data, imap.FetchItemModSeq) || len(byUID[uid]) == 0 {
			return false, nil
		}
		for _, i := range byUID[uid] {
			response := r.items[i].data
			if hasFetchItem(response, imap.FetchItemModSeq) {
				continue
			}
			before, err := fetchResponseWireSize(response)
			if err != nil {
				return false, err
			}
			for key, values := range data.Items {
				if strings.EqualFold(string(key), string(imap.FetchItemModSeq)) {
					deleteFetchItem(response, imap.FetchItemModSeq)
					response.Items[key] = slices.Clone(values)
				} else if strings.EqualFold(string(key), string(imap.FetchItemFlags)) && hasFetchItem(response, imap.FetchItemFlags) {
					deleteFetchItem(response, imap.FetchItemFlags)
					response.Items[key] = slices.Clone(values)
				}
			}
			after, err := fetchResponseWireSize(response)
			if err != nil {
				return false, err
			}
			if after-before > c.server.options.Limits.MaxStoreResponseBytes-r.bytes {
				r.streamInstead()
				return true, nil
			}
			r.bytes += after - before
		}
		return true, nil
	}
	for _, response := range r.items {
		if hasFetchItem(response.data, imap.FetchItemModSeq) && !have[response.uid] {
			have[response.uid] = true
			if _, err := merge(response.uid, response.data); err != nil {
				return err
			}
			if r.streaming {
				return nil
			}
		}
	}
	var missing []imap.UID
	for _, uid := range c.state.selected.uids {
		if requested.Contains(uid) && (!modified.Contains(uid) || len(byUID[uid]) != 0) && !have[uid] {
			missing = append(missing, uid)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	writer := newFetchWriter(func(_ context.Context, data *imap.FetchMessageData) error {
		uid, _ := extractFetchUID(data)
		if merged, err := merge(uid, data); merged || err != nil {
			return err
		}
		// This UID had no FLAGS response to enrich. The fallback exists
		// only to supply MODSEQ, including for a successful .SILENT store.
		mapped := *data
		mapped.Items = maps.Clone(data.Items)
		deleteFetchItem(&mapped, imap.FetchItemFlags)
		return r.add(c, &mapped, includeUID)
	})
	err := c.state.selected.mailbox.Fetch(ctx, writer, imap.UIDSetNum(missing...), &FetchOptions{
		Items: []imap.FetchItem{imap.FetchItemUID, imap.FetchItemFlags, imap.FetchItemModSeq},
	})
	writer.core.close()
	if err != nil {
		return err
	}
	if r.streaming {
		return nil
	}
	kept := r.items[:0]
	for _, response := range r.items {
		if hasFetchItem(response.data, imap.FetchItemModSeq) {
			kept = append(kept, response)
		} else {
			// FETCH may skip a message expunged since Store returned it.
			// Do not send its stale FLAGS without the mandatory MODSEQ.
			response.cleanup()
		}
	}
	r.items = kept
	// The written order is the client's sequence order, whatever order the
	// backend and the fallback produced their halves in.
	slices.SortStableFunc(r.items, func(a, b storeResponse) int { return cmp.Compare(a.data.SeqNum, b.data.SeqNum) })
	return nil
}

// reported lists the messages whose new state the collected responses tell
// the client: their FLAGS, or under .SILENT, which may omit FLAGS, their
// MODSEQ.
func (r *storeResponses) reported(silent, requireModSeq bool) map[imap.UID]struct{} {
	covered := make(map[imap.UID]struct{}, len(r.items))
	for _, response := range r.items {
		hasModSeq := hasFetchItem(response.data, imap.FetchItemModSeq)
		if (hasFetchItem(response.data, imap.FetchItemFlags) || silent && hasModSeq) && (!requireModSeq || hasModSeq) {
			covered[response.uid] = struct{}{}
		}
	}
	return covered
}

func hasFetchItem(data *imap.FetchMessageData, item imap.FetchItemKeyword) bool {
	for key, values := range data.Items {
		if strings.EqualFold(string(key), string(item)) && len(values) != 0 {
			return true
		}
	}
	return false
}

func deleteFetchItem(data *imap.FetchMessageData, item imap.FetchItemKeyword) {
	for key := range data.Items {
		if strings.EqualFold(string(key), string(item)) {
			delete(data.Items, key)
		}
	}
}
