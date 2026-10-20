package imapserver

import (
	"context"
	"fmt"

	"github.com/kiliant/go-imap"
)

// OBJECTID (RFC 8474), SAVEDATE (RFC 8514), STATUS=SIZE (RFC 8438),
// APPENDLIMIT (RFC 7889) and PREVIEW (RFC 8970).
//
// Backends witness support for extension items carried by the open FetchItem
// and StatusItem types. The framework gates requests on those witnesses.
// OBJECTID additionally emits MAILBOXID on CREATE and SELECT/EXAMINE by asking
// the existing Session.Status contract for the immutable mailbox identifier.
// A SELECT whose ID lookup fails is abandoned before new selection responses.
// The framework does not generally validate optional FETCH data: some RFCs
// permit absent values, such as a message with no SAVEDATE.

func init() {
	registerCapabilities(
		// OBJECTID supplies EMAILID and THREADID on FETCH and MAILBOXID on
		// STATUS. RFC 8474.
		capabilityDescriptor{
			Name:            "OBJECTID",
			States:          stateMaskAuthenticated | stateMaskSelected,
			RequiresBackend: backendSupportsCapability("OBJECTID"),
		},
		// SAVEDATE distinguishes when a message arrived in this mailbox from
		// the internal date the message itself carries. RFC 8514.
		capabilityDescriptor{
			Name:            "SAVEDATE",
			States:          stateMaskAuthenticated | stateMaskSelected,
			RequiresBackend: backendSupportsCapability("SAVEDATE"),
		},
		// STATUS=SIZE adds the SIZE status item. RFC 8438.
		capabilityDescriptor{
			Name:            "STATUS=SIZE",
			States:          stateMaskAuthenticated | stateMaskSelected,
			RequiresBackend: backendSupportsCapability("STATUS=SIZE"),
		},
		// APPENDLIMIT advertises the largest message the server accepts. The
		// per-mailbox form is a STATUS item; the server-wide form is the
		// capability token itself, which a backend expresses by witnessing
		// APPENDLIMIT and answering the STATUS item. RFC 7889.
		capabilityDescriptor{
			Name:            "APPENDLIMIT",
			States:          stateMaskAuthenticated | stateMaskSelected,
			RequiresBackend: backendSupportsCapability("APPENDLIMIT"),
		},
		// PREVIEW supplies a short textual preview of a message. RFC 8970.
		capabilityDescriptor{
			Name:            "PREVIEW",
			States:          stateMaskAuthenticated | stateMaskSelected,
			RequiresBackend: backendSupportsCapability("PREVIEW"),
		},
	)
}

// mailboxIDResponse queries the existing OBJECTID promise without adding a new
// backend interface. Object identifiers are 1..255 ASCII letters/digits/-/_.
func mailboxIDResponse(ctx context.Context, c *conn, mailbox string) (string, error) {
	data, err := c.state.session.Status(ctx, mailbox, &StatusOptions{Items: []imap.StatusItem{imap.StatusItemMailboxID}})
	if err != nil {
		return "", err
	}
	var id string
	if data != nil {
		id, _ = data.Values[imap.StatusItemMailboxID].(string)
	}
	if len(id) == 0 || len(id) > 255 {
		return "", fmt.Errorf("imapserver: OBJECTID backend returned an invalid MAILBOXID")
	}
	for _, ch := range []byte(id) {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_') {
			return "", fmt.Errorf("imapserver: OBJECTID backend returned an invalid MAILBOXID")
		}
	}
	return "(" + id + ")", nil
}
