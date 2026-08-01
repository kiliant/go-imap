package imapclient

import (
	"context"
	"errors"
	"io"
	"strings"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// MoveOptions configures MOVE and UID MOVE. A nil pointer selects the defaults.
//
// Construct with keyed fields only; fields may be added in a future release.
type MoveOptions struct {
	// SavedSearchResult sends the SEARCHRES "$" marker in place of the message
	// set. When it is non-nil the set argument must be empty, because the two
	// address the same argument position. See [SavedSearchResult].
	SavedSearchResult *SavedSearchResult

	// AllowNonAtomicFallback permits the emulated MOVE described on
	// [Client.Move] when the server does not advertise MOVE. It is false by
	// default: the emulation is not atomic, can leave a message in both
	// mailboxes or in neither, and on a server without UIDPLUS it expunges
	// every \Deleted message in the mailbox rather than only the moved ones.
	// A caller opts into that explicitly or gets
	// [ErrCapabilityNotAdvertised].
	AllowNonAtomicFallback bool

	_ struct{}
}

func (o *MoveOptions) savedSearchResult() *SavedSearchResult {
	if o == nil {
		return nil
	}
	return o.SavedSearchResult
}

func (o *MoveOptions) allowFallback() bool { return o != nil && o.AllowNonAtomicFallback }

// MoveData is the result of a MOVE. RFC 6851.
//
// Construct with keyed fields only; fields may be added in a future release.
type MoveData struct {
	// UIDPlus is the COPYUID data for the move, or a zero-valued [CopyData]
	// when the server sent none. RFC 6851 section 4.3 advises a UIDPLUS server
	// to send COPYUID in an untagged OK before the EXPUNGE responses, which is
	// the form this client reads first; that advice was observed to be followed
	// by Dovecot 2.4.3, Stalwart 0.11.8, Cyrus 3.x and GreenMail 2.1.9. A server
	// that instead uses the tagged OK of RFC 4315 section 3 is also accepted.
	UIDPlus CopyData

	// Emulated reports that the server does not advertise MOVE and the move
	// was performed as COPY, STORE and EXPUNGE. See [Client.Move] for what
	// that costs. UIDPlus may still be filled from the intermediate COPY's
	// tagged COPYUID when the server advertises UIDPLUS.
	Emulated bool

	// ExpungedEveryDeletedMessage reports that the emulated move finished with
	// a bare EXPUNGE because the server advertises neither MOVE nor UIDPLUS,
	// and therefore may have permanently removed messages that this client
	// never selected — any message another client had marked \Deleted in this
	// mailbox. It is never set on the native path.
	ExpungedEveryDeletedMessage bool

	_ struct{}
}

// Move moves messages addressed by sequence number into destination.
// MOVE, RFC 6851.
//
// Move blocks until the move has finished, unlike [Client.Copy], which returns
// a handle. That is deliberate on two counts. The emulated fallback below is
// three commands, so a handle would be dishonest about when the work happens;
// and RFC 6851 section 3.3 warns that MOVE renumbers the source mailbox and may
// emit unrelated EXPUNGE responses, which makes it unsafe to pipeline anything
// depending on sequence numbers behind it.
//
// # Emulated fallback
//
// When the server does not advertise MOVE, and only when
// [MoveOptions.AllowNonAtomicFallback] is set, the move is emulated with the
// sequence RFC 6851 section 3.3 names as having the same effect: COPY, then
// STORE +FLAGS.SILENT (\Deleted), then an expunge. **The emulation is not
// atomic.** The intermediate states RFC 6851 says a real MOVE never produces do
// occur here:
//
//   - between the COPY and the expunge the message exists in both mailboxes,
//     and another client can see it twice;
//   - a failure after the COPY leaves the source copy behind, flagged \Deleted;
//   - the \Deleted flag is really set, so a concurrent EXPUNGE or CLOSE from
//     any client can remove the source messages at a moment of its choosing;
//   - no COPYUID is available on the emulated path's intermediate COPY when
//     the destination rejects UIDPLUS codes, so [MoveData.UIDPlus] stays
//     zero-valued for that case;
//
// The final step is UID EXPUNGE (RFC 4315) wherever UIDPLUS is advertised, so
// only the moved messages are removed. For a sequence-number move that requires
// resolving the set to UIDs first, which this method does with a FETCH before
// touching anything. Without UIDPLUS there is no such precision available and
// the emulation issues a bare EXPUNGE, which removes **every** \Deleted message
// in the mailbox, including messages another client marked. That outcome is
// reported in [MoveData.ExpungedEveryDeletedMessage].
func (c *Client) Move(ctx context.Context, set imap.SeqSet, destination string, options *MoveOptions) (*MoveData, error) {
	return c.move(ctx, false, set.String(), destination, options)
}

// MoveUID moves messages addressed by UID into destination. See [Client.Move]
// for the blocking contract and the emulated fallback.
func (c *Client) MoveUID(ctx context.Context, set imap.UIDSet, destination string, options *MoveOptions) (*MoveData, error) {
	return c.move(ctx, true, set.String(), destination, options)
}

func (c *Client) move(ctx context.Context, uid bool, set, destination string, options *MoveOptions) (*MoveData, error) {
	name := "MOVE"
	if uid {
		name = "UID MOVE"
	}
	if ctx == nil {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: name + " requires a non-nil context"}
	}
	if destination == "" {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: name + " requires a destination mailbox"}
	}
	argument, err := savedResultArgument(c, name, set, options.savedSearchResult())
	if err != nil {
		return nil, err
	}
	if !c.supportsAny("MOVE") {
		if !options.allowFallback() {
			return nil, capabilityError(name, "MOVE")
		}
		return c.moveEmulated(ctx, uid, argument, destination)
	}

	data := &MoveData{}
	cmd := c.beginCommandWithCompletion(name, stateSelected, func(enc *imapwire.Encoder) {
		enc.SP()
		if argument == "$" {
			enc.Atom(argument)
		} else {
			writeNumSet(enc, argument)
		}
		enc.SP().Mailbox(destination)
	}, moveCollector(data), func(success bool, code, args string) {
		// Prefer the untagged COPYUID already claimed by moveCollector. Some
		// servers put COPYUID on the tagged OK instead (RFC 4315 section 3).
		if !success || data.UIDPlus.Received() || !strings.EqualFold(code, string(imap.CodeCopyUID)) {
			return
		}
		parsed, err := parseCopyUID(args)
		if err != nil {
			return
		}
		data.UIDPlus = *parsed
	})
	if err := cmd.Wait(ctx); err != nil {
		return nil, err
	}
	return data, nil
}

// moveCollector claims the untagged OK carrying COPYUID. RFC 6851 section 4.3.
func moveCollector(data *MoveData) commandCollector {
	return func(resp *untaggedResponse) (bool, error) {
		if resp.cond == nil || resp.name != "OK" || resp.cond.Text.Code != string(imap.CodeCopyUID) {
			return false, nil
		}
		parsed, err := parseCopyUID(resp.cond.Text.Args)
		if err != nil {
			return true, err
		}
		data.UIDPlus = *parsed
		return true, nil
	}
}

// moveEmulated performs COPY + STORE \Deleted + expunge. See [Client.Move].
func (c *Client) moveEmulated(ctx context.Context, uid bool, argument, destination string) (*MoveData, error) {
	data := &MoveData{Emulated: true}

	// Resolve the expunge argument before anything is modified. A
	// sequence-number move needs UIDs for UID EXPUNGE, and sequence numbers
	// stop being meaningful the moment the first EXPUNGE response arrives.
	uidplus := c.supportsAny("UIDPLUS")
	expungeArgument := argument
	if uidplus && !uid && argument != "$" {
		resolved, err := c.resolveUIDs(ctx, argument)
		if err != nil {
			return nil, err
		}
		if resolved.IsEmpty() {
			// Nothing matched, so there is nothing to copy or expunge. A real
			// MOVE of an empty set also succeeds without side effects.
			return data, nil
		}
		expungeArgument = resolved.String()
	}

	copyName, storeName := "COPY", "STORE"
	if uid {
		copyName, storeName = "UID COPY", "UID STORE"
	}
	copied, err := c.copy(copyName, argument, destination).Wait(ctx)
	if err != nil {
		return nil, err
	}
	if copied != nil && copied.Received() {
		data.UIDPlus = *copied
	}
	storeOptions := &StoreOptions{Op: StoreFlagsAdd, Silent: true}
	if err := c.store(storeName, argument, []imap.Flag{imap.FlagDeleted}, storeOptions).Wait(ctx); err != nil {
		return nil, err
	}
	if uidplus {
		if err := c.uidExpunge(expungeArgument).Wait(ctx); err != nil {
			return nil, err
		}
		return data, nil
	}
	data.ExpungedEveryDeletedMessage = true
	if err := c.Expunge().Wait(ctx); err != nil {
		return nil, err
	}
	return data, nil
}

// resolveUIDs maps a sequence-number set to the UIDs the server currently
// assigns to it.
func (c *Client) resolveUIDs(ctx context.Context, set string) (imap.UIDSet, error) {
	cmd := c.fetch("FETCH", set, func(imap.SeqNum) bool { return true }, []imap.FetchItem{imap.FetchItemUID})
	var uids imap.UIDSet
	for {
		message, err := cmd.Next(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		// Match on the value type rather than on the item name: the FETCH
		// parser keys the map by the spelling the server used, and the item is
		// only case-insensitively "UID".
		for _, values := range message.Items {
			for _, value := range values {
				if uid, ok := value.(imap.FetchDataUID); ok {
					uids.AddNum(imap.UID(uid))
				}
			}
		}
	}
	if err := cmd.Wait(ctx); err != nil {
		return nil, err
	}
	if uids.IsEmpty() {
		return nil, nil
	}
	return uids.Normalized(), nil
}
