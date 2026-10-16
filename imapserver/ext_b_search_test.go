package imapserver

import (
	"context"
	"math"
	"testing"

	"github.com/kiliant/go-imap"
)

type searchClockMailbox struct {
	SelectedMailbox
	data []*imap.FetchMessageData
}

func (m *searchClockMailbox) Fetch(ctx context.Context, writer *FetchWriter, uids imap.UIDSet, options *FetchOptions) error {
	for _, data := range m.data {
		if err := writer.WriteMessage(ctx, data); err != nil {
			return err
		}
	}
	return nil
}

func TestSearchClockBackendFallback(t *testing.T) {
	message := func(uid imap.UID, clock uint64) *imap.FetchMessageData {
		return &imap.FetchMessageData{Items: map[imap.FetchDataKey][]imap.FetchData{
			"UID": {imap.FetchDataUID(uid)}, "MODSEQ": {imap.FetchDataModSeq(clock)},
		}}
	}
	for _, tt := range []struct {
		name      string
		data      []*imap.FetchMessageData
		aggregate uint64
		want      uint64
	}{
		{"valid-full-width", []*imap.FetchMessageData{message(1, math.MaxInt64), message(2, 9)}, 0, math.MaxInt64},
		// Expunged between Search and the clock Fetch: a race, not a fault.
		{"concurrently-expunged", []*imap.FetchMessageData{message(1, 3)}, 0, 3},
		{"all-expunged-with-aggregate", nil, 7, 7},
		{"all-expunged-without-aggregate", nil, 0, 0},
		{"duplicate-message", []*imap.FetchMessageData{message(1, 3), message(1, 4)}, 0, 0},
		{"unexpected-message", []*imap.FetchMessageData{message(1, 3), message(3, 4)}, 0, 0},
		{"zero-clock", []*imap.FetchMessageData{message(1, 0), message(2, 4)}, 0, 0},
		{"overflow-clock", []*imap.FetchMessageData{message(1, math.MaxInt64+1), message(2, 4)}, 0, 0},
		{"nil-message", []*imap.FetchMessageData{nil}, 0, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := &conn{state: sessionState{selected: &selectedState{mailbox: &searchClockMailbox{data: tt.data}}}}
			// A result that is not the client's whole view forces the Fetch path,
			// as a search racing an unannounced removal does.
			result := &SearchResult{UIDs: []imap.UID{1, 2, 3}, ModSeq: tt.aggregate}
			got, err := searchResponseModSeq(context.Background(), c, &searchArgs{}, result, []imap.UID{1, 2})
			if tt.want == 0 {
				if err == nil {
					t.Fatal("invalid backend response accepted")
				}
			} else if err != nil || got != tt.want {
				t.Fatalf("fallback = %d, %v; want %d", got, err, tt.want)
			}
		})
	}
}
