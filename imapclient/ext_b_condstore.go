package imapclient

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// SyncFetchOptions carries the CONDSTORE and QRESYNC FETCH modifiers. A nil
// pointer is valid and issues a plain FETCH.
//
// Construct with keyed fields only; fields may be added in a future release.
type SyncFetchOptions struct {
	// ChangedSince restricts the response to messages whose mod-sequence is
	// greater than this value. CONDSTORE, RFC 7162 section 3.1.4.1. Zero
	// omits the modifier; the grammar's mod-sequence-value starts at 1, so
	// zero is not a meaningful CHANGEDSINCE argument.
	//
	// Sending it implicitly adds the MODSEQ data item to the request and makes
	// this a CONDSTORE enabling command.
	ChangedSince uint64

	// ReportVanished adds the VANISHED FETCH modifier, which makes the server
	// also report, as VANISHED (EARLIER), those messages in the requested set
	// that were expunged since ChangedSince. QRESYNC, RFC 7162 section 3.2.6.
	//
	// RFC 7162 section 3.2.6 allows it only on UID FETCH, only together with
	// ChangedSince, and only after a successful ENABLE QRESYNC. All three are
	// checked locally, because the server's answer to any of them is a tagged
	// BAD.
	ReportVanished bool

	_ struct{}
}

// SyncFetchCommand is an in-flight FETCH or UID FETCH carrying CONDSTORE or
// QRESYNC modifiers. Message data is delivered through the embedded
// [FetchCommand]; expunges are collected separately and read with
// [SyncFetchCommand.Vanished].
type SyncFetchCommand struct {
	*FetchCommand

	mu       sync.Mutex
	vanished []VanishedData
}

// Vanished returns the VANISHED responses this command received.
//
// RFC 7162 section 3.2.6 requires the server to send every VANISHED response
// before any FETCH response, so the set is complete by the time
// [FetchCommand.Next] first returns data. It is safe to call at any time; the
// returned slice is a copy owned by the caller.
//
// These are expunges, not message data, and they are reported by UID: see
// [VanishedData] for why they must not be applied like EXPUNGE responses.
func (cmd *SyncFetchCommand) Vanished() []VanishedData {
	if cmd == nil {
		return nil
	}
	cmd.mu.Lock()
	defer cmd.mu.Unlock()
	return append([]VanishedData(nil), cmd.vanished...)
}

func (cmd *SyncFetchCommand) addVanished(v VanishedData) {
	cmd.mu.Lock()
	cmd.vanished = append(cmd.vanished, v)
	cmd.mu.Unlock()
}

// FetchSync issues FETCH for a sequence-number set with the CONDSTORE
// CHANGEDSINCE modifier. The VANISHED modifier is not legal on the
// sequence-number form; use [Client.FetchUIDSync] for it.
func (c *Client) FetchSync(set imap.SeqSet, options *SyncFetchOptions, items ...imap.FetchItem) *SyncFetchCommand {
	return c.fetchSync("FETCH", set.String(), func(n imap.SeqNum) bool { return set.Contains(n) }, options, items)
}

// FetchUIDSync issues UID FETCH with the CONDSTORE CHANGEDSINCE and QRESYNC
// VANISHED modifiers. Together they are the incremental resynchronisation
// command: one round trip reports both the flag changes and the expunges that
// happened since a known mod-sequence.
//
// FETCH responses still carry the server's sequence number; include
// [imap.FetchItemUID] when the UID is needed in the returned data.
func (c *Client) FetchUIDSync(set imap.UIDSet, options *SyncFetchOptions, items ...imap.FetchItem) *SyncFetchCommand {
	return c.fetchSync("UID FETCH", set.String(), func(imap.SeqNum) bool { return true }, options, items)
}

func (c *Client) fetchSync(name, set string, matches func(imap.SeqNum) bool, options *SyncFetchOptions, items []imap.FetchItem) *SyncFetchCommand {
	o := SyncFetchOptions{}
	if options != nil {
		o = *options
	}
	fc := &FetchCommand{responses: make(chan *imap.FetchMessageData), stop: make(chan struct{})}
	result := &SyncFetchCommand{FetchCommand: fc}
	fail := func(err error) *SyncFetchCommand {
		fc.Command = failedCommand(name, err)
		close(fc.stop)
		return result
	}

	if set == "" || len(items) == 0 {
		return fail(&imap.Error{Type: imap.ErrorTypeProtocol, Text: "FETCH requires a non-empty set and at least one item"})
	}
	if err := validateFetchItems(items); err != nil {
		return fail(&imap.Error{Type: imap.ErrorTypeProtocol, Text: err.Error()})
	}
	if o.ChangedSince != 0 && !validModSeq(o.ChangedSince) {
		return fail(&imap.Error{Type: imap.ErrorTypeProtocol, Text: fmt.Sprintf("CHANGEDSINCE mod-sequence %d is not in 1..%d", o.ChangedSince, MaxModSeq)})
	}
	if o.ChangedSince != 0 && !c.condStoreAvailable() {
		return fail(capabilityError("FETCH CHANGEDSINCE", "CONDSTORE"))
	}
	if o.ReportVanished {
		if name != "UID FETCH" {
			return fail(&imap.Error{Type: imap.ErrorTypeProtocol, Text: "the VANISHED FETCH modifier is only allowed on UID FETCH (RFC 7162 section 3.2.6)"})
		}
		if o.ChangedSince == 0 {
			return fail(&imap.Error{Type: imap.ErrorTypeProtocol, Text: "the VANISHED FETCH modifier requires CHANGEDSINCE (RFC 7162 section 3.2.6)"})
		}
		if !c.Supports("QRESYNC") {
			return fail(capabilityError("FETCH VANISHED", "QRESYNC"))
		}
		if !c.QResyncEnabled() {
			return fail(&imap.Error{
				Type: imap.ErrorTypeProtocol,
				Text: "the VANISHED FETCH modifier requires a successful ENABLE QRESYNC first (RFC 7162 section 3.2.6)",
				Err:  ErrCapabilityNotAdvertised,
			})
		}
	}

	untagged := 0
	limit := c.maxUntaggedResponses()
	fc.Command = c.beginCommandWithCompletion(name, stateSelected, func(enc *imapwire.Encoder) {
		enc.SP()
		writeNumSet(enc, set)
		enc.SP().List(len(items), func(i int) { writeFetchItem(enc, items[i]) })
		if o.ChangedSince == 0 {
			return
		}
		enc.SP().Special('(').Atom("CHANGEDSINCE").SP()
		writeModSeq(enc, o.ChangedSince)
		if o.ReportVanished {
			enc.SP().Atom("VANISHED")
		}
		enc.Special(')')
	}, func(resp *untaggedResponse) (bool, error) {
		if !resp.hasNum && resp.name == "VANISHED" {
			if err := countUntaggedResponse(&untagged, limit, name); err != nil {
				return true, err
			}
			vanished, err := readVanished(resp.dec)
			if err != nil {
				return true, err
			}
			result.addVanished(vanished)
			return true, nil
		}
		if !resp.hasNum || resp.name != "FETCH" || !matches(imap.SeqNum(resp.number)) {
			return false, nil
		}
		return true, readFetchResponse(resp, fc.deliver)
	}, func(success bool, _, _ string) {
		if success && o.ChangedSince != 0 {
			// RFC 7162 section 3.1: a FETCH with the CHANGEDSINCE modifier is a
			// CONDSTORE enabling command in its own right.
			c.markCondStoreEnabled()
		}
	})
	go func() {
		<-fc.Command.done
		close(fc.stop)
	}()
	return result
}

// SyncStoreOptions carries the CONDSTORE UNCHANGEDSINCE STORE modifier. A nil
// pointer replaces FLAGS unconditionally, exactly like [Client.Store].
//
// Construct with keyed fields only; fields may be added in a future release.
type SyncStoreOptions struct {
	// Op is FLAGS (the default), +FLAGS, or -FLAGS.
	Op StoreFlagsOp

	// Silent requests the .SILENT form. Note that RFC 7162 section 3.1.3
	// requires the server to send an untagged FETCH carrying MODSEQ for every
	// message the conditional store did change, even with .SILENT, so that the
	// client's cached mod-sequences stay correct.
	Silent bool

	// UnchangedSince makes the store conditional: a message is only modified
	// if every metadata item's mod-sequence is at most this value. CONDSTORE,
	// RFC 7162 section 3.1.3. A nil pointer omits the modifier.
	//
	// It is a pointer because zero is a meaningful value here — the grammar is
	// mod-sequence-valzer, and RFC 7162 Example 8 uses UNCHANGEDSINCE 0 as a
	// probe that always fails if the metadata item exists, which is how a
	// client tests for the presence of a keyword atomically.
	//
	// Messages that fail the test are reported through
	// [SyncStoreData.ModifiedSeqNums] or [SyncStoreData.ModifiedUIDs].
	UnchangedSince *uint64

	_ struct{}
}

// SyncStoreData reports which messages a conditional STORE did not modify.
//
// Exactly one of the two sets is ever populated, matching the address space of
// the command that produced it: STORE reports sequence numbers and UID STORE
// reports UIDs. Keeping them apart is deliberate — a UID silently read as a
// sequence number points at the wrong message.
//
// Construct with keyed fields only; fields may be added in a future release.
type SyncStoreData struct {
	// ModifiedSeqNums are the sequence numbers from a MODIFIED response code
	// on a STORE. Empty when every message passed the UNCHANGEDSINCE test.
	ModifiedSeqNums imap.SeqSet

	// ModifiedUIDs are the UIDs from a MODIFIED response code on a UID STORE.
	ModifiedUIDs imap.UIDSet

	_ struct{}
}

// HasModified reports whether any message failed the UNCHANGEDSINCE test.
func (d *SyncStoreData) HasModified() bool {
	if d == nil {
		return false
	}
	return !d.ModifiedSeqNums.IsEmpty() || !d.ModifiedUIDs.IsEmpty()
}

// SyncStoreCommand is an in-flight conditional STORE or UID STORE.
type SyncStoreCommand struct {
	*Command
	uid  bool
	data *SyncStoreData
}

// Wait waits for the conditional STORE and returns the set of messages that
// failed the UNCHANGEDSINCE test.
//
// A conditional STORE that leaves some messages unmodified is not itself a
// failure: RFC 7162 section 3.1.3 has the server report those messages in a
// MODIFIED response code and complete the command with a tagged OK. The
// returned data is therefore non-nil whenever the command completed, and the
// caller inspects [SyncStoreData.HasModified] rather than the error.
//
// When a server instead reports MODIFIED on a tagged NO — which RFC 7162
// section 3.1 permits — the returned error is the usual [imap.Error] carrying
// [imap.CodeModified], and the failure set is still filled in.
func (cmd *SyncStoreCommand) Wait(ctx context.Context) (*SyncStoreData, error) {
	if cmd == nil || cmd.Command == nil {
		return nil, fmt.Errorf("imapclient: nil store command")
	}
	err := cmd.Command.Wait(ctx)
	if err != nil {
		if parseErr := cmd.collectModified(err); parseErr != nil {
			return nil, parseErr
		}
		return cmd.data, err
	}
	return cmd.data, nil
}

// collectModified fills the failure set from a MODIFIED response code carried
// on a tagged NO.
func (cmd *SyncStoreCommand) collectModified(err error) error {
	var ierr *imap.Error
	if !errors.As(err, &ierr) || ierr.Code != imap.CodeModified {
		return nil
	}
	return cmd.data.parseModified(ierr.CodeArgs, cmd.uid)
}

func (d *SyncStoreData) parseModified(args string, uid bool) error {
	args = strings.TrimSpace(args)
	if args == "" {
		return nil
	}
	if uid {
		set, err := imap.ParseUIDSet(args)
		if err != nil {
			return &imap.Error{Type: imap.ErrorTypeProtocol, Text: fmt.Sprintf("invalid MODIFIED response code %q", args), Err: err}
		}
		d.ModifiedUIDs = set
		return nil
	}
	set, err := imap.ParseSeqSet(args)
	if err != nil {
		return &imap.Error{Type: imap.ErrorTypeProtocol, Text: fmt.Sprintf("invalid MODIFIED response code %q", args), Err: err}
	}
	d.ModifiedSeqNums = set
	return nil
}

// StoreSync changes flags for a sequence-number set, optionally conditionally.
// Failures are reported as sequence numbers in
// [SyncStoreData.ModifiedSeqNums].
func (c *Client) StoreSync(set imap.SeqSet, flags []imap.Flag, options *SyncStoreOptions) *SyncStoreCommand {
	return c.storeSync("STORE", set.String(), false, flags, options)
}

// StoreUIDSync changes flags for a UID set, optionally conditionally. Failures
// are reported as UIDs in [SyncStoreData.ModifiedUIDs].
func (c *Client) StoreUIDSync(set imap.UIDSet, flags []imap.Flag, options *SyncStoreOptions) *SyncStoreCommand {
	return c.storeSync("UID STORE", set.String(), true, flags, options)
}

func (c *Client) storeSync(name, set string, uid bool, flags []imap.Flag, options *SyncStoreOptions) *SyncStoreCommand {
	data := &SyncStoreData{}
	result := &SyncStoreCommand{uid: uid, data: data}
	fail := func(err error) *SyncStoreCommand {
		result.Command = failedCommand(name, err)
		return result
	}
	o := SyncStoreOptions{Op: StoreFlagsSet}
	if options != nil {
		o = *options
		if o.Op == "" {
			o.Op = StoreFlagsSet
		}
	}
	if set == "" {
		return fail(&imap.Error{Type: imap.ErrorTypeProtocol, Text: "STORE requires a non-empty set"})
	}
	if o.Op != StoreFlagsSet && o.Op != StoreFlagsAdd && o.Op != StoreFlagsRemove {
		return fail(&imap.Error{Type: imap.ErrorTypeProtocol, Text: "invalid STORE flag operation"})
	}
	if o.UnchangedSince != nil {
		if !validModSeqZero(*o.UnchangedSince) {
			return fail(&imap.Error{Type: imap.ErrorTypeProtocol, Text: fmt.Sprintf("UNCHANGEDSINCE mod-sequence %d is not in 0..%d", *o.UnchangedSince, MaxModSeq)})
		}
		if !c.condStoreAvailable() {
			return fail(capabilityError("STORE UNCHANGEDSINCE", "CONDSTORE"))
		}
	}
	result.Command = c.beginCommandWithCompletion(name, stateSelected, func(enc *imapwire.Encoder) {
		enc.SP()
		writeNumSet(enc, set)
		if o.UnchangedSince != nil {
			enc.SP().Special('(').Atom("UNCHANGEDSINCE").SP()
			writeModSeq(enc, *o.UnchangedSince)
			enc.Special(')')
		}
		op := string(o.Op)
		if o.Silent {
			op += ".SILENT"
		}
		enc.SP().Atom(op).SP().List(len(flags), func(i int) { enc.Flag(string(flags[i])) })
	}, nil, func(success bool, code, args string) {
		if success && o.UnchangedSince != nil {
			// RFC 7162 section 3.1: a STORE with UNCHANGEDSINCE is a CONDSTORE
			// enabling command.
			c.markCondStoreEnabled()
		}
		if success && strings.EqualFold(code, string(imap.CodeModified)) {
			// RFC 7162 section 3.1.3: a partial conditional STORE completes OK
			// with [MODIFIED …] naming the messages that failed the test.
			_ = data.parseModified(args, uid)
		}
	})
	return result
}
