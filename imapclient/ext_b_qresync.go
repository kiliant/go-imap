package imapclient

import (
	"context"
	"fmt"
	"strings"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// SeqMatchData is the optional fourth QRESYNC argument: a sample of sequence
// numbers paired with the UIDs the client believes they have. QRESYNC, RFC 7162
// section 3.2.5.2.
//
// The server compares each pair with the mailbox's current state. Where a pair
// matches, the client demonstrably knows about every expunge up to and
// including that message, so the server can leave that range out of the
// VANISHED response even when it no longer remembers when those messages were
// expunged. On a large mailbox this can turn a VANISHED response listing tens
// of thousands of UIDs into nothing at all.
//
// Both sets must be in ascending order, must contain the same number of
// elements, and must not contain "*".
//
// Construct with keyed fields only; fields may be added in a future release.
type SeqMatchData struct {
	// SeqNums is the sample of message sequence numbers, ascending.
	SeqNums imap.SeqSet
	// UIDs are the UIDs the client believes correspond to SeqNums, ascending
	// and of the same length.
	UIDs imap.UIDSet
	_    struct{}
}

// QResyncOptions is the QRESYNC parameter to SELECT and EXAMINE. QRESYNC, RFC
// 7162 section 3.2.5.
//
// UIDValidity and ModSeq together form the synchronisation anchor, and neither
// is meaningful without the other. A UID is only interpretable within one
// UIDVALIDITY, and a mod-sequence is only comparable within one UIDVALIDITY
// too: RFC 7162 section 3.1.2.1 says that when the server changes UIDVALIDITY
// it need not keep the same HIGHESTMODSEQ, and requires a client whose cached
// UIDVALIDITY no longer matches to delete its cached HIGHESTMODSEQ. Send the
// pair you cached together, and if the server reports a different UIDVALIDITY,
// discard the whole cached mailbox — see [SyncMailboxStatus.ResyncRejected].
//
// Construct with keyed fields only; fields may be added in a future release.
type QResyncOptions struct {
	// UIDValidity is the last UIDVALIDITY the client cached for this mailbox.
	// It must be non-zero.
	UIDValidity uint32

	// ModSeq is the last HIGHESTMODSEQ the client cached for this mailbox,
	// under that same UIDVALIDITY. It must be between 1 and [MaxModSeq].
	ModSeq uint64

	// KnownUIDs optionally restricts the report to UIDs the client already
	// knows about. An empty set means "everything", which the server treats as
	// 1:<UIDNEXT-1>. It must not contain "*".
	//
	// The client never shortens this set: a silently truncated known-UID list
	// produces a delta that looks complete and is not. RFC 7162 section 4
	// recommends keeping a command line under about 8192 octets, so a caller
	// with a very large set should split its resynchronisation instead.
	KnownUIDs imap.UIDSet

	// SeqMatch is the optional sequence-number/UID sample described by
	// [SeqMatchData]. It is nil for most clients.
	SeqMatch *SeqMatchData

	_ struct{}
}

// SyncSelectOptions configures the synchronisation-aware SELECT and EXAMINE.
// A nil pointer is valid and selects the plain command.
//
// Plain SELECT/EXAMINE options that are not CONDSTORE/QRESYNC enabling
// parameters belong on [SelectOptions]; this type is the home for the
// synchronisation select parameters from RFC 7162.
//
// Construct with keyed fields only; fields may be added in a future release.
type SyncSelectOptions struct {
	// CondStore sends the CONDSTORE select parameter, which makes this a
	// CONDSTORE enabling command. CONDSTORE, RFC 7162 section 3.1.8.
	//
	// It closes a race: without it, a metadata change made by another session
	// between the server's HIGHESTMODSEQ response code and the client's first
	// CONDSTORE enabling command would go unreported.
	CondStore bool

	// QResync sends the QRESYNC select parameter. It requires ENABLE QRESYNC
	// to have succeeded first; see [Client.QResyncEnabled].
	QResync *QResyncOptions

	_ struct{}
}

// SyncMailboxStatus is the result of a synchronisation-aware SELECT or EXAMINE.
//
// Construct with keyed fields only; fields may be added in a future release.
type SyncMailboxStatus struct {
	// Status is the ordinary mailbox status, exactly as [Client.Select]
	// reports it, including UIDValidity, HighestModSeq and
	// UIDValidityChanged. It is never nil.
	Status *MailboxStatus

	// NoModSeq reports that the server returned the NOMODSEQ response code:
	// this mailbox has no persistent storage of mod-sequences. CONDSTORE, RFC
	// 7162 section 3.1.2.2.
	//
	// It is distinct from "HighestModSeq happens to be zero". While it is set,
	// the server rejects FETCH CHANGEDSINCE, FETCH/SEARCH MODSEQ and STORE
	// UNCHANGEDSINCE for this mailbox with a tagged BAD, and no incremental
	// resynchronisation of this mailbox is possible.
	NoModSeq bool

	// Closed reports that the server returned the CLOSED response code,
	// meaning a previously selected mailbox was closed implicitly by this
	// command. QRESYNC, RFC 7162 section 3.2.11. Responses before it belong to
	// the old mailbox; responses after it belong to the new one.
	Closed bool

	// MailboxID is the OBJECTID identifier of the mailbox, from the MAILBOXID
	// response code, or "" if the server sent none. OBJECTID, RFC 8474 section
	// 4.2.
	MailboxID string

	// ResyncRejected reports that the server did not use the QRESYNC anchor
	// that was sent, so Vanished and Fetched are not a delta and must not be
	// applied to a cache.
	//
	// This is the single most dangerous case in QRESYNC, because the server
	// signals it by saying nothing at all: RFC 7162 section 3.2.5 has the
	// server "ignore the remaining parameters and behave as if no dynamic
	// message data changed" when the supplied UIDVALIDITY does not match, and
	// section 3.2.5.1 says the same for a NOMODSEQ mailbox. A client that does
	// not notice concludes that nothing changed while it was away.
	//
	// When it is true, discard every cached UID, flag and mod-sequence for
	// this mailbox and resynchronise from scratch against the newly reported
	// Status.UIDValidity.
	ResyncRejected bool

	// Vanished holds the VANISHED responses received while selecting, in wire
	// order. During quick resynchronisation these carry the (EARLIER) tag; see
	// [VanishedData] for why they must not be treated as EXPUNGE responses.
	Vanished []VanishedData

	// Fetched holds the untagged FETCH responses received while selecting, in
	// wire order: the flag changes that happened since the anchor. Each
	// carries a UID and a MODSEQ. RFC 7162 section 3.2.6 requires the server
	// to send every VANISHED response before any of these.
	Fetched []*imap.FetchMessageData

	_ struct{}
}

// SyncSelectCommand is an in-flight synchronisation-aware SELECT or EXAMINE.
type SyncSelectCommand struct {
	*Command
	data *SyncMailboxStatus
}

// Wait waits for the command to finish and returns the collected status.
func (cmd *SyncSelectCommand) Wait(ctx context.Context) (*SyncMailboxStatus, error) {
	if cmd == nil || cmd.Command == nil {
		return nil, fmt.Errorf("imapclient: nil select command")
	}
	if err := cmd.Command.Wait(ctx); err != nil {
		return nil, err
	}
	return cmd.data, nil
}

// SelectSync selects mailbox for read-write access, optionally carrying the
// CONDSTORE and QRESYNC select parameters. A nil options pointer behaves like
// [Client.Select].
//
// The QRESYNC parameter requires ENABLE QRESYNC to have succeeded on this
// connection; ENABLE is only valid in the authenticated state, so the sequence
// is authenticate, ENABLE QRESYNC, SelectSync. If it has not, the command is
// refused locally with [ErrCapabilityNotAdvertised] rather than sent, because RFC
// 7162 section 3.2.5 requires the server to answer BAD.
func (c *Client) SelectSync(mailbox string, options *SyncSelectOptions) *SyncSelectCommand {
	return c.selectSync("SELECT", mailbox, options)
}

// ExamineSync is [Client.SelectSync] for read-only access.
func (c *Client) ExamineSync(mailbox string, options *SyncSelectOptions) *SyncSelectCommand {
	return c.selectSync("EXAMINE", mailbox, options)
}

func (c *Client) selectSync(name, mailbox string, options *SyncSelectOptions) *SyncSelectCommand {
	o := SyncSelectOptions{}
	if options != nil {
		o = *options
	}
	base := &MailboxStatus{Mailbox: normalisedMailbox(mailbox), ReadOnly: name == "EXAMINE"}
	data := &SyncMailboxStatus{Status: base}
	result := &SyncSelectCommand{data: data}

	if err := c.checkSyncSelectOptions(name, &o); err != nil {
		result.Command = failedCommand(name, err)
		return result
	}

	untagged := 0
	limit := c.maxUntaggedResponses()
	result.Command = c.beginCommandWithCompletion(name, stateAuthenticated|stateSelected, func(enc *imapwire.Encoder) {
		enc.SP().Mailbox(mailbox)
		writeSelectParams(enc, &o)
	}, syncSelectCollector(data, selectCollector(base), &untagged, limit, name), func(success bool, _, _ string) {
		c.finishSelectSync(success, data, &o)
	})
	return result
}

// checkSyncSelectOptions validates every select parameter before a tag is
// allocated, so an illegal QRESYNC argument never reaches the wire.
func (c *Client) checkSyncSelectOptions(name string, o *SyncSelectOptions) *imap.Error {
	if o.CondStore && !c.condStoreAvailable() {
		return capabilityError(name+" (CONDSTORE)", "CONDSTORE")
	}
	q := o.QResync
	if q == nil {
		return nil
	}
	if !c.Supports("QRESYNC") {
		return capabilityError(name+" (QRESYNC)", "QRESYNC")
	}
	if !c.QResyncEnabled() {
		return &imap.Error{
			Type: imap.ErrorTypeProtocol,
			Text: "the QRESYNC select parameter requires a successful ENABLE QRESYNC first (RFC 7162 section 3.2.5)",
			Err:  ErrCapabilityNotAdvertised,
		}
	}
	if q.UIDValidity == 0 {
		return &imap.Error{Type: imap.ErrorTypeProtocol, Text: "QRESYNC requires a non-zero UIDVALIDITY"}
	}
	if !validModSeq(q.ModSeq) {
		return &imap.Error{Type: imap.ErrorTypeProtocol, Text: fmt.Sprintf("QRESYNC mod-sequence %d is not in 1..%d", q.ModSeq, MaxModSeq)}
	}
	if q.KnownUIDs.Dynamic() {
		return &imap.Error{Type: imap.ErrorTypeProtocol, Text: "QRESYNC known UIDs must not contain \"*\""}
	}
	if q.SeqMatch != nil {
		if q.SeqMatch.SeqNums.IsEmpty() || q.SeqMatch.UIDs.IsEmpty() {
			return &imap.Error{Type: imap.ErrorTypeProtocol, Text: "QRESYNC sequence match data requires both a sequence set and a UID set"}
		}
		if q.SeqMatch.SeqNums.Dynamic() || q.SeqMatch.UIDs.Dynamic() {
			return &imap.Error{Type: imap.ErrorTypeProtocol, Text: "QRESYNC sequence match data must not contain \"*\""}
		}
		seqNums, seqOK := q.SeqMatch.SeqNums.Nums()
		uids, uidOK := q.SeqMatch.UIDs.Nums()
		if !seqOK || !uidOK || len(seqNums) != len(uids) {
			return &imap.Error{Type: imap.ErrorTypeProtocol, Text: "QRESYNC sequence match data must pair each sequence number with one UID"}
		}
	}
	return nil
}

func writeSelectParams(enc *imapwire.Encoder, o *SyncSelectOptions) {
	if !o.CondStore && o.QResync == nil {
		return
	}
	enc.SP().Special('(')
	first := true
	if o.CondStore {
		enc.Atom("CONDSTORE")
		first = false
	}
	if q := o.QResync; q != nil {
		if !first {
			enc.SP()
		}
		enc.Atom("QRESYNC").SP().Special('(')
		enc.Number(q.UIDValidity).SP()
		writeModSeq(enc, q.ModSeq)
		if !q.KnownUIDs.IsEmpty() {
			enc.SP()
			writeNumSet(enc, q.KnownUIDs.String())
		}
		if m := q.SeqMatch; m != nil {
			enc.SP().Special('(')
			writeNumSet(enc, m.SeqNums.String())
			enc.SP()
			writeNumSet(enc, m.UIDs.String())
			enc.Special(')')
		}
		enc.Special(')')
	}
	enc.Special(')')
}

// finishSelectSync mirrors the state transitions of the plain SELECT, which
// this command must not diverge from: a failed SELECT leaves no mailbox
// selected, and a successful one records the UIDVALIDITY that every cached UID
// depends on.
func (c *Client) finishSelectSync(success bool, data *SyncMailboxStatus, o *SyncSelectOptions) {
	if !success {
		// RFC 3501 section 6.3.1: a failed SELECT or EXAMINE leaves no mailbox
		// selected, so a session that had one drops back to the authenticated
		// state.
		c.mu.Lock()
		if c.state == StateSelected {
			c.state = StateAuthenticated
			c.selectedMailbox = ""
		}
		c.mu.Unlock()
		return
	}
	status := data.Status
	c.mu.Lock()
	if status.UIDValidity != 0 {
		if old := c.mailboxUIDValidity[status.Mailbox]; old != 0 && old != status.UIDValidity {
			status.UIDValidityChanged = true
		}
		c.mailboxUIDValidity[status.Mailbox] = status.UIDValidity
	}
	c.state = StateSelected
	c.selectedMailbox = status.Mailbox
	c.mu.Unlock()

	if o.CondStore {
		// RFC 7162 section 3.1: SELECT/EXAMINE with the CONDSTORE parameter is
		// a CONDSTORE enabling command, with the same effect as ENABLE
		// CONDSTORE for the rest of the connection.
		c.markCondStoreEnabled()
	}
	if q := o.QResync; q != nil {
		// RFC 7162 sections 3.2.5 and 3.2.5.1: the server silently ignores the
		// remaining QRESYNC parameters when the UIDVALIDITY does not match, or
		// when the mailbox has no persistent mod-sequences. Neither is an
		// error, so the client has to detect it by comparison.
		data.ResyncRejected = data.NoModSeq || status.UIDValidity != q.UIDValidity
	}
}

func syncSelectCollector(data *SyncMailboxStatus, base commandCollector, untagged *int, limit int, name string) commandCollector {
	return func(resp *untaggedResponse) (bool, error) {
		if resp.cond != nil && resp.name == "OK" {
			switch imap.ResponseCode(strings.ToUpper(resp.cond.Text.Code)) {
			case imap.CodeNoModSeq:
				data.NoModSeq = true
				return true, nil
			case imap.CodeClosed:
				data.Closed = true
				return true, nil
			case imap.CodeMailboxID:
				id, err := parseObjectID(resp.cond.Text.Args)
				if err != nil {
					return true, err
				}
				data.MailboxID = id
				return true, nil
			}
			return base(resp)
		}
		if !resp.hasNum && resp.name == "VANISHED" {
			if err := countUntaggedResponse(untagged, limit, name); err != nil {
				return true, err
			}
			vanished, err := readVanished(resp.dec)
			if err != nil {
				return true, err
			}
			data.Vanished = append(data.Vanished, vanished)
			return true, nil
		}
		if resp.hasNum && resp.name == "FETCH" {
			if err := countUntaggedResponse(untagged, limit, name); err != nil {
				return true, err
			}
			message, err := readSyncFetch(resp)
			if err != nil {
				return true, err
			}
			data.Fetched = append(data.Fetched, message)
			return true, nil
		}
		return base(resp)
	}
}
