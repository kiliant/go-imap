package imapclient

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/kiliant/go-imap"
)

// StatusSize returns the STATUS=SIZE value from a STATUS response: the total
// size of the mailbox in octets. STATUS=SIZE, RFC 8438 section 3.
//
// The second result reports whether the server sent the item at all, which is
// distinct from a mailbox whose size is zero. RFC 8438 requires clients to
// accept 63-bit values, so the size is int64 rather than anything narrower.
func StatusSize(data *StatusData) (int64, bool) {
	value, ok := statusUint64(data, imap.StatusItemSize)
	if !ok || value > uint64(MaxModSeq) {
		return 0, false
	}
	return int64(value), true
}

// StatusAppendLimit returns the APPENDLIMIT value from a STATUS response: the
// largest message, in octets, that the server will accept into this mailbox.
// APPENDLIMIT, RFC 7889 section 3.1.
//
// The results are the limit, whether the mailbox is unlimited, and whether the
// server sent the item at all. RFC 7889 section 5 gives the value as
// "number / nil": NIL means this mailbox has no limit, which is emphatically
// not the same as a limit of zero — RFC 7889 section 5 defines a limit of zero
// as "the server will not accept any APPEND commands at all" for that mailbox.
func StatusAppendLimit(data *StatusData) (limit int64, unlimited bool, ok bool) {
	if data == nil || data.Values == nil {
		return 0, false, false
	}
	raw, present := data.Values[imap.StatusItemAppendLimit]
	if !present {
		return 0, false, false
	}
	switch v := raw.(type) {
	case uint64:
		if v > uint64(MaxModSeq) {
			return 0, false, false
		}
		return int64(v), false, true
	case string:
		if strings.EqualFold(v, "NIL") {
			return 0, true, true
		}
	}
	return 0, false, false
}

func statusUint64(data *StatusData, item imap.StatusItemKeyword) (uint64, bool) {
	if data == nil || data.Values == nil {
		return 0, false
	}
	value, ok := data.Values[item].(uint64)
	return value, ok
}

// MailboxSize issues STATUS (SIZE) for mailbox and returns its total size in
// octets. STATUS=SIZE, RFC 8438.
//
// It returns an [imap.Error] wrapping [ErrCapabilityNotAdvertised] without
// contacting the server when STATUS=SIZE is not advertised. There is no
// emulation: the base-protocol equivalent is a FETCH of RFC822.SIZE for every
// message in the mailbox, which is exactly the round trip this extension
// exists to avoid, and which would silently turn one command into thousands.
func (c *Client) MailboxSize(ctx context.Context, mailbox string) (int64, error) {
	if !c.Supports("STATUS=SIZE") {
		return 0, capabilityError("STATUS (SIZE)", "STATUS=SIZE")
	}
	data, err := c.Status(mailbox, &StatusOptions{Items: []imap.StatusItem{imap.StatusItemSize}}).Wait(ctx)
	if err != nil {
		return 0, err
	}
	size, ok := StatusSize(data)
	if !ok {
		return 0, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "server did not return a SIZE status item"}
	}
	return size, nil
}

// AppendLimitData is the result of an APPENDLIMIT lookup. APPENDLIMIT, RFC
// 7889.
//
// Construct with keyed fields only; fields may be added in a future release.
type AppendLimitData struct {
	// Limit is the largest message the server will accept, in octets. It is
	// meaningless when Unlimited is set. A limit of zero means the server
	// accepts no APPEND at all here (RFC 7889 section 5).
	Limit int64

	// Unlimited reports that the server declared no limit for this mailbox, by
	// returning NIL for the STATUS item.
	Unlimited bool

	// ServerWide reports that the value came from the server's
	// "APPENDLIMIT=<n>" capability rather than from a STATUS command, and
	// therefore applies to every mailbox. RFC 7889 section 5 defines that
	// capability form as "the fixed maximum message size in octets that the
	// server will accept".
	ServerWide bool

	_ struct{}
}

// AppendLimit reports the largest message the server will accept into mailbox.
// APPENDLIMIT, RFC 7889.
//
// A server may advertise the limit in either of two ways, and this method
// covers both. "APPENDLIMIT=<n>" declares one fixed limit for every mailbox and
// is answered from the capability list without a round trip. A bare
// "APPENDLIMIT" declares per-mailbox limits, which are read with
// STATUS (APPENDLIMIT).
//
// When neither is advertised it returns an [imap.Error] wrapping
// [ErrCapabilityNotAdvertised]. There is no fallback: the base protocol has no
// way to ask, and guessing a limit would either reject messages the server
// would have taken or let an APPEND fail after the whole message has been
// streamed.
func (c *Client) AppendLimit(ctx context.Context, mailbox string) (*AppendLimitData, error) {
	if values := c.CapabilityValues("APPENDLIMIT"); len(values) > 0 {
		limit, err := strconv.ParseInt(values[0], 10, 64)
		if err != nil || limit < 0 {
			return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: fmt.Sprintf("invalid APPENDLIMIT capability value %q", values[0])}
		}
		return &AppendLimitData{Limit: limit, ServerWide: true}, nil
	}
	if !c.Supports("APPENDLIMIT") {
		return nil, capabilityError("STATUS (APPENDLIMIT)", "APPENDLIMIT")
	}
	data, err := c.Status(mailbox, &StatusOptions{Items: []imap.StatusItem{imap.StatusItemAppendLimit}}).Wait(ctx)
	if err != nil {
		return nil, err
	}
	limit, unlimited, ok := StatusAppendLimit(data)
	if !ok {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "server did not return an APPENDLIMIT status item"}
	}
	return &AppendLimitData{Limit: limit, Unlimited: unlimited}, nil
}

// MailboxHighestModSeq issues STATUS (HIGHESTMODSEQ) for mailbox. CONDSTORE,
// RFC 7162 section 3.1.7.
//
// This is a CONDSTORE enabling command in its own right (RFC 7162 section 3.1),
// so a successful call also switches the session into CONDSTORE mode for the
// rest of the connection; see [Client.CondStoreEnabled].
//
// A zero result is not an error and not an absent value: RFC 7162 section 3.1.7
// requires a server that cannot store mod-sequences persistently for a mailbox
// to answer zero, which is the STATUS-command equivalent of the NOMODSEQ
// response code. Such a mailbox cannot be resynchronised incrementally.
//
// This value together with the mailbox's UIDVALIDITY is the synchronisation
// anchor to cache; neither half is usable without the other.
func (c *Client) MailboxHighestModSeq(ctx context.Context, mailbox string) (uint64, error) {
	if !c.condStoreAvailable() {
		return 0, capabilityError("STATUS (HIGHESTMODSEQ)", "CONDSTORE")
	}
	data, err := c.Status(mailbox, &StatusOptions{Items: []imap.StatusItem{imap.StatusItemHighestModSeq}}).Wait(ctx)
	if err != nil {
		return 0, err
	}
	if _, ok := statusUint64(data, imap.StatusItemHighestModSeq); !ok {
		return 0, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "server did not return a HIGHESTMODSEQ status item"}
	}
	c.markCondStoreEnabled()
	return data.HighestModSeq, nil
}
