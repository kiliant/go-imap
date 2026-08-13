package imapserver

import (
	"fmt"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapcodec"
)

// UIDONLY (RFC 9586): once enabled, the connection stops using message sequence
// numbers entirely.
//
// This has no backend surface at all, because the framework already owns the
// UID/sequence-number map — backends have only ever seen UIDs. What UIDONLY
// changes is which of the two number spaces reaches the client, which is
// entirely a response-shaping decision.
//
// The saving is real: a client that never sees a sequence number does not have
// to track EXPUNGE renumbering, which is the most error-prone part of writing an
// IMAP client. That is also why the extension is all-or-nothing — a server that
// enabled it and then emitted one sequence number would be worse than one that
// never offered it, since the client has by then discarded the machinery needed
// to interpret it.

func init() {
	registerCapabilities(
		capabilityDescriptor{
			Name:   "UIDONLY",
			States: stateMaskAuthenticated | stateMaskSelected,
			Enable: func(state *sessionState) bool { return state.enable("UIDONLY") },
		},
	)
}

// uidOnlyEnabled reports whether sequence numbers are forbidden on this
// connection.
func uidOnlyEnabled(c *conn) bool {
	return c.state.enabledCapability("UIDONLY")
}

// requireUIDCommand refuses a message-set command that names messages by
// sequence number once UIDONLY is enabled.
//
// RFC 9586 section 3.1 requires a BAD rather than a silent reinterpretation. The
// client asked about message 3; answering about UID 3 would act on a different
// message, which is worse than refusing.
func requireUIDCommand(c *conn, command *queuedCommand) error {
	if !uidOnlyEnabled(c) || commandUsesUIDs(command) {
		return nil
	}
	return fmt.Errorf("%s requires the UID form once UIDONLY is enabled", command.name)
}

// writeFetchLikeResponse writes one FETCH response in the shape the session's
// number space requires: UIDFETCH under UIDONLY, ordinary FETCH otherwise.
//
// Under UIDONLY the UID item is dropped from the item list, because the UID is
// already the response's subject and RFC 9586 section 3.2 makes repeating it the
// one redundant item in the grammar.
func writeFetchLikeResponse(c *conn, data *imap.FetchMessageData) error {
	if !uidOnlyEnabled(c) {
		return imapcodec.WriteFetchResponse(c.encoder, data, fetchLiteralSize)
	}
	uid, ok := extractFetchUID(data)
	if !ok {
		return fmt.Errorf("imapserver: UIDONLY response has no UID")
	}
	trimmed := *data
	trimmed.Items = make(map[imap.FetchDataKey][]imap.FetchData, len(data.Items))
	for key, values := range data.Items {
		if imap.FetchDataKey(imap.FetchItemUID) == key {
			continue
		}
		trimmed.Items[key] = values
	}
	return imapcodec.WriteUIDFetchResponse(c.encoder, uid, &trimmed, fetchLiteralSize)
}

// uidOnlyRequiresUIDResponses reports whether EXPUNGE must be reported as
// VANISHED.
//
// RFC 9586 section 3.3: an untagged EXPUNGE carries a sequence number, so it
// cannot be sent at all once UIDONLY is enabled. VANISHED carries UIDs and is
// the only removal report left, which is why UIDONLY implies QRESYNC's response
// shape whether or not QRESYNC itself was enabled.
func uidOnlyRequiresUIDResponses(c *conn) bool {
	return uidOnlyEnabled(c)
}
