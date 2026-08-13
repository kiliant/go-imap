package imapserver

import (
	"context"
	"fmt"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// QRESYNC (RFC 7162 section 3.2).
//
// QRESYNC is the extension the design named as needing durable backend state no
// protocol layer can reconstruct: a record of which UIDs were expunged and at
// which modification sequence, retained after the messages themselves are gone.
// A client that was offline asks "what vanished since modseq N", and only the
// backend can answer.

// QResyncMailbox is the optional resynchronisation report of QRESYNC. A selected
// mailbox implements it when the backend witnesses QRESYNC.
//
// The framework calls Resync once, immediately after a SELECT carrying the
// QRESYNC parameter, before it writes the selection's untagged responses.
type QResyncMailbox interface {
	// Resync reports the messages removed since the client's claimed
	// modification sequence. Changed messages are reported by the framework
	// through an ordinary FETCH, so this answers only the question a FETCH
	// cannot: what is no longer there.
	Resync(ctx context.Context, params *QResyncSelect, options *QResyncOptions) (*QResyncResult, error)
}

// QResyncOptions configures a resynchronisation. A nil pointer selects the
// defaults. It is empty today and exists so a future QRESYNC parameter has a
// framework-side home: QResyncSelect carries what the *client* claimed, which
// is a different thing.
// Construct with keyed fields only; fields may be added in a future release.
type QResyncOptions struct{ _ struct{} }

// QResyncResult is a backend's answer to a QRESYNC resynchronisation.
// Construct with keyed fields only; fields may be added in a future release.
type QResyncResult struct {
	// Vanished lists the UIDs removed since the client's modification
	// sequence, including messages the backend no longer stores.
	Vanished imap.UIDSet
	// Changed lists the UIDs modified since then, which the framework reports
	// through FETCH. An empty set means nothing changed.
	Changed imap.UIDSet
	_       struct{}
}

func init() {
	registerCapabilities(
		capabilityDescriptor{
			Name:            "QRESYNC",
			States:          stateMaskAuthenticated | stateMaskSelected,
			RequiresBackend: backendSupportsCapability("QRESYNC"),
			// RFC 7162 section 3.2: QRESYNC implies CONDSTORE, and enabling
			// QRESYNC enables CONDSTORE with it.
			Depends: []string{"CONDSTORE"},
			Enable: func(state *sessionState) bool {
				state.enable("CONDSTORE")
				return state.enable("QRESYNC")
			},
		},
	)
}

// parseQResyncParams reads the QRESYNC selection parameter:
//
//	QRESYNC (uidvalidity modseq [known-uids [(known-seqnums known-uids)]])
//
// RFC 7162 section 3.2.5.
func parseQResyncParams(decoder *imapwire.Decoder) (*QResyncSelect, error) {
	params := &QResyncSelect{}
	err := decoder.ExpectList(func() error {
		if !decoder.ExpectNumber(&params.UIDValidity) || !decoder.ExpectSP() {
			return decoder.Err()
		}
		var modSeq int64
		if !decoder.ExpectNumber64(&modSeq) {
			return decoder.Err()
		}
		if modSeq < 0 {
			return fmt.Errorf("QRESYNC modification sequence %d is negative", modSeq)
		}
		params.ModSeq = uint64(modSeq)
		if !decoder.SP() {
			return nil
		}
		// The known-uids argument is optional, and may itself be followed by
		// the parenthesised sequence match data.
		if !decoder.PeekSpecial('(') {
			var raw string
			if !decoder.ExpectSequenceSet(&raw) {
				return decoder.Err()
			}
			set, err := imap.ParseUIDSet(raw)
			if err != nil {
				return err
			}
			params.KnownUIDs = set
			if !decoder.SP() {
				return nil
			}
		}
		return parseQResyncSeqMatch(decoder, params)
	})
	if err != nil {
		return nil, err
	}
	return params, nil
}

// parseQResyncSeqMatch reads the optional sequence match data of RFC 7162
// section 3.2.5.2, which lets a backend detect a stale client view without the
// client having to send every UID it knows.
func parseQResyncSeqMatch(decoder *imapwire.Decoder, params *QResyncSelect) error {
	return decoder.ExpectList(func() error {
		var rawSeqNums, rawUIDs string
		if !decoder.ExpectSequenceSet(&rawSeqNums) || !decoder.ExpectSP() || !decoder.ExpectSequenceSet(&rawUIDs) {
			return decoder.Err()
		}
		seqNums, err := imap.ParseSeqSet(rawSeqNums)
		if err != nil {
			return err
		}
		uids, err := imap.ParseUIDSet(rawUIDs)
		if err != nil {
			return err
		}
		params.SeqMatchSeqNums = seqNums
		params.SeqMatchUIDs = uids
		return nil
	})
}

// qresyncEnabled reports whether removals must be reported as VANISHED rather
// than as EXPUNGE. RFC 7162 section 3.2.7 makes this a property of the session,
// not of the command.
func qresyncEnabled(c *conn) bool {
	return c.state.enabledCapability("QRESYNC")
}

// writeVanished writes an untagged VANISHED response.
//
// earlier marks the removals as historical — the answer to a resynchronisation
// rather than something that happened just now. RFC 7162 section 3.2.10.1 keeps
// the two forms distinct because a client applies them differently: EARLIER
// updates its cache without disturbing the message counts it has already seen.
func writeVanished(c *conn, uids imap.UIDSet, earlier bool) error {
	if uids.IsEmpty() || uids.Dynamic() {
		return nil
	}
	c.encoder.BeginResponse(imapwire.ResponseUntagged, "").Atom("VANISHED")
	if earlier {
		c.encoder.SP().Special('(').Atom("EARLIER").Special(')')
	}
	c.encoder.SP().Atom(uids.String()).CRLF()
	return c.encoder.Flush()
}

// writeQResyncSelection answers a QRESYNC SELECT, after the ordinary selection
// responses have been written.
//
// A backend that witnesses QRESYNC but whose selected mailbox does not implement
// QResyncMailbox is a backend bug: the capability was advertised on its behalf.
// Reporting it is better than returning a silently empty resynchronisation,
// which a client would read as "nothing vanished" and act on.
func writeQResyncSelection(ctx context.Context, c *conn, params *QResyncSelect, snapshot *SelectSnapshot) error {
	if params == nil {
		return nil
	}
	// A UIDVALIDITY change invalidates every UID the client holds, so the
	// resynchronisation is meaningless and the client must start over.
	// RFC 7162 section 3.2.5.1.
	if snapshot != nil && params.UIDValidity != 0 && snapshot.Status.UIDValidity != params.UIDValidity {
		return nil
	}
	mailbox, ok := c.state.selected.mailbox.(QResyncMailbox)
	if !ok {
		return fmt.Errorf("imapserver: backend advertises QRESYNC but the selected mailbox does not implement QResyncMailbox")
	}
	result, err := mailbox.Resync(ctx, params, nil)
	if err != nil {
		return err
	}
	if result == nil {
		return nil
	}
	if err := writeVanished(c, result.Vanished, true); err != nil {
		return err
	}
	return writeQResyncChanged(ctx, c, result.Changed)
}

// writeQResyncChanged reports the changed messages of a resynchronisation as
// ordinary FETCH responses, which is what RFC 7162 section 3.2.5.2 specifies.
func writeQResyncChanged(ctx context.Context, c *conn, changed imap.UIDSet) error {
	if changed.IsEmpty() || changed.Dynamic() {
		return nil
	}
	items := applyCondStoreFetchItems(c, []imap.FetchItem{imap.FetchItemFlags, imap.FetchItemUID})
	writer := newFetchWriter(func(_ context.Context, data *imap.FetchMessageData) error {
		mapped, err := mapFetchResponse(c.state.selected, data, true)
		if err != nil {
			return err
		}
		if err := writeFetchLikeResponse(c, mapped); err != nil {
			return err
		}
		return c.encoder.Flush()
	})
	err := c.state.selected.mailbox.Fetch(ctx, writer, changed, &FetchOptions{Items: items})
	writer.core.close()
	return err
}
