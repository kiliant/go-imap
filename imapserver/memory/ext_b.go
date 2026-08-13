package memory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
)

// Group B capability support: CONDSTORE and QRESYNC (RFC 7162), plus the
// message-attribute extensions SAVEDATE (RFC 8514) and STATUS=SIZE (RFC 8438).
//
// # What this backend cannot honestly promise
//
// QRESYNC exists to serve a client that was offline across a server restart. It
// answers "what vanished since modification sequence N" from a record of
// removals kept after the messages themselves are gone — and this backend keeps
// that record in memory, so a restart loses it, along with every message.
//
// Within one process lifetime the implementation here is complete and the
// semantics are exact. Across a restart it is not merely lossy, it is the one
// case QRESYNC was designed for. A durable backend must persist expungedRecord
// and highestModSeq alongside the messages; this one is a reference
// implementation and a test fixture, not a store.

// bumpModSeqLocked advances the mailbox's modification sequence and returns the
// new value, which the caller assigns to whatever it just changed.
//
// The caller must hold the account lock.
func bumpModSeqLocked(m *mailbox) uint64 {
	m.highestModSeq++
	return m.highestModSeq
}

// recordVanishedLocked remembers a removal so QRESYNC can report it after the
// message is gone.
//
// The caller must hold the account lock.
func recordVanishedLocked(m *mailbox, uid imap.UID) {
	m.expunged = append(m.expunged, expungedRecord{uid: uid, modSeq: bumpModSeqLocked(m)})
}

// StoreCondStore implements [imapserver.CondStoreMailbox].
//
// RFC 7162 section 3.1.3: a message whose modification sequence exceeds
// UnchangedSince is left untouched and reported through MODIFIED, while the
// rest of the command proceeds. The whole scan runs under one lock so the
// comparison and the store are atomic with respect to another session.
func (s *selected) StoreCondStore(ctx context.Context, writer *imapserver.FetchWriter, uids imap.UIDSet, store *imapserver.StoreFlags, options *imapserver.StoreOptions) (*imapserver.CondStoreResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.readOnly {
		return nil, noError(imap.CodeReadOnly, "mailbox is read-only")
	}
	if store == nil {
		return nil, noError(imap.CodeClientBug, "nil STORE flags")
	}
	s.session.account.mu.Lock()
	if s.closed {
		s.session.account.mu.Unlock()
		return nil, noError(imap.CodeClosed, "mailbox is no longer selected")
	}
	var origin imapserver.ChangeToken
	silent, unchangedSince := false, uint64(0)
	if options != nil {
		origin, silent, unchangedSince = options.Origin, options.Silent, options.UnchangedSince
	}
	var changes []imapserver.Update
	var results []*imap.FetchMessageData
	var modified []imap.UID
	for i, msg := range s.mailbox.messages {
		if !uids.Contains(msg.uid) {
			continue
		}
		if unchangedSince != 0 && msg.modSeq > unchangedSince {
			modified = append(modified, msg.uid)
			continue
		}
		flags, err := applyStoreFlags(msg.flags, store)
		if err != nil {
			s.session.account.mu.Unlock()
			return nil, err
		}
		msg.flags = flags
		msg.modSeq = bumpModSeqLocked(s.mailbox)
		changes = append(changes, &imapserver.UpdateFlags{UID: msg.uid, Flags: cloneFlags(flags), ModSeq: msg.modSeq})
		// RFC 7162 section 3.1.3: a conditional store reports the new
		// modification sequence even for .SILENT, because the client needs it
		// to issue the next conditional command.
		if !silent || unchangedSince != 0 {
			results = append(results, flagsFetchData(imap.SeqNum(i+1), msg))
		}
	}
	if len(changes) != 0 {
		publishLocked(s.mailbox, advanceLocked(s.mailbox, origin, changes))
	}
	highest := s.mailbox.highestModSeq
	s.session.account.mu.Unlock()
	for _, data := range results {
		if err := writer.WriteMessage(ctx, data); err != nil {
			return nil, err
		}
	}
	return &imapserver.CondStoreResult{
		Modified:      imap.UIDSetNum(modified...),
		HighestModSeq: highest,
	}, nil
}

// Resync implements [imapserver.QResyncMailbox].
//
// Vanished is answered from the retained removal record; Changed from the
// messages still present whose modification sequence is newer than the client's.
func (s *selected) Resync(ctx context.Context, params *imapserver.QResyncSelect) (*imapserver.QResyncResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if params == nil {
		return &imapserver.QResyncResult{}, nil
	}
	s.session.account.mu.Lock()
	defer s.session.account.mu.Unlock()
	if s.closed {
		return nil, noError(imap.CodeClosed, "mailbox is no longer selected")
	}
	var vanished, changed []imap.UID
	for _, record := range s.mailbox.expunged {
		if record.modSeq <= params.ModSeq {
			continue
		}
		// A client that named the UIDs it knows about is told only about those.
		// RFC 7162 section 3.2.5.
		if !params.KnownUIDs.IsEmpty() && !params.KnownUIDs.Contains(record.uid) {
			continue
		}
		vanished = append(vanished, record.uid)
	}
	for _, msg := range s.mailbox.messages {
		if msg.modSeq <= params.ModSeq {
			continue
		}
		if !params.KnownUIDs.IsEmpty() && !params.KnownUIDs.Contains(msg.uid) {
			continue
		}
		changed = append(changed, msg.uid)
	}
	slices.Sort(vanished)
	slices.Sort(changed)
	return &imapserver.QResyncResult{
		Vanished: imap.UIDSetNum(slices.Compact(vanished)...),
		Changed:  imap.UIDSetNum(slices.Compact(changed)...),
	}, nil
}

var (
	_ imapserver.CondStoreMailbox = (*selected)(nil)
	_ imapserver.QResyncMailbox   = (*selected)(nil)
)

// appendLimit is the largest message this backend accepts, reported through the
// APPENDLIMIT status item. RFC 7889 section 4.
const appendLimit = 32 << 20

// saveDateOf reports when a message was placed in its mailbox.
//
// RFC 8514 section 3 permits NIL for a message whose arrival time is unknown,
// which is why FetchDataSaveDate carries a pointer: a zero time.Time would be
// indistinguishable from a genuine date the backend does happen to know.
func saveDateOf(msg *message) *time.Time {
	if msg.saveDate.IsZero() {
		return nil
	}
	saved := msg.saveDate
	return &saved
}

// emailID is the OBJECTID identifier for a message.
//
// RFC 8474 section 3 requires it to be immutable and unique within the account,
// and to survive a COPY — the same message body copied elsewhere keeps its
// identifier. Deriving it from the message bytes gives all of that without a
// counter to persist, which suits a backend that persists nothing.
func emailID(msg *message) string {
	sum := sha256.Sum256(msg.raw)
	return "M" + hex.EncodeToString(sum[:12])
}

// mailboxID is the OBJECTID identifier for a mailbox. It is derived from the
// UIDVALIDITY, which this backend already never reuses.
func mailboxID(m *mailbox) string {
	return "B" + strconv.FormatUint(uint64(m.uidValidity), 16)
}

// previewOf returns a short textual preview of a message.
//
// RFC 8970 section 4 leaves the algorithm to the server and only requires that
// the result be short and derived from the text the user would read. This takes
// the leading run of the body, which is what a mail client shows in a list.
func previewOf(msg *message) string {
	const previewLimit = 200
	body := msg.raw
	if at := bytes.Index(body, []byte("\r\n\r\n")); at >= 0 {
		body = body[at+4:]
	}
	preview := strings.Join(strings.Fields(string(body)), " ")
	if len(preview) > previewLimit {
		preview = preview[:previewLimit]
	}
	return preview
}
