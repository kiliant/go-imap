package imapserver

import (
	"maps"
	"slices"
	"testing"

	"github.com/kiliant/go-imap"
)

// TestStatusReplyNarrowsToRequestedItems covers the guarantee written on
// Session.Status: the framework writes what the client asked for and nothing
// else.
//
// It was documented before it was true. A backend that fills its whole
// StatusData — the obvious way to write one, and what memory does — made the
// server volunteer RECENT, UNSEEN and SIZE to a client that asked only for
// MESSAGES. RFC 3501 section 6.3.10 defines the response as the answer to the
// request, and a client cannot tell a volunteered item from one it forgot it
// asked for.
func TestStatusReplyNarrowsToRequestedItems(t *testing.T) {
	data := &imap.StatusData{
		Mailbox: "INBOX",
		Values: map[imap.StatusItemKeyword]any{
			imap.StatusItemMessages: uint64(0),
			imap.StatusItemRecent:   uint64(7),
			imap.StatusItemUnseen:   uint64(42),
			imap.StatusItemSize:     uint64(12345),
		},
	}
	narrowed := statusReply(data, []imap.StatusItem{imap.StatusItemMessages})

	got := slices.Sorted(maps.Keys(narrowed.Values))
	want := []imap.StatusItemKeyword{imap.StatusItemMessages}
	if !slices.Equal(got, want) {
		t.Errorf("narrowed STATUS items = %v, want %v", got, want)
	}
	// A present zero must survive: it is the value, not an absence.
	if value, ok := narrowed.Number(imap.StatusItemMessages); !ok || value != 0 {
		t.Errorf("MESSAGES = %d, present %v; want a present zero", value, ok)
	}
	// The backend's own value is untouched — it may be shared or cached.
	if len(data.Values) != 4 {
		t.Errorf("backend StatusData was modified: %v", data.Values)
	}
}

// TestStatusReplyPassesUnmodelledItems guards the open-ended half of rule 1.
// StatusItem is an interface so a future RFC can add an item carrying
// arguments; narrowing must not be the thing that drops it.
func TestStatusReplyPassesUnmodelledItems(t *testing.T) {
	future := imap.StatusItemKeyword("ANNOTATION-STORAGE")
	data := &imap.StatusData{
		Mailbox: "INBOX",
		Values:  map[imap.StatusItemKeyword]any{future: uint64(9)},
	}
	narrowed := statusReply(data, []imap.StatusItem{future})
	if value, ok := narrowed.Number(future); !ok || value != 9 {
		t.Errorf("an item this library does not model was dropped: %v", narrowed.Values)
	}
}
