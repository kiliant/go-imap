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
		name string
		data []*imap.FetchMessageData
		want uint64
	}{
		{"valid-full-width", []*imap.FetchMessageData{message(1, math.MaxInt64), message(2, 9)}, math.MaxInt64},
		{"missing-message", []*imap.FetchMessageData{message(1, 3)}, 0},
		{"duplicate-message", []*imap.FetchMessageData{message(1, 3), message(1, 4)}, 0},
		{"unexpected-message", []*imap.FetchMessageData{message(1, 3), message(3, 4)}, 0},
		{"zero-clock", []*imap.FetchMessageData{message(1, 0), message(2, 4)}, 0},
		{"overflow-clock", []*imap.FetchMessageData{message(1, math.MaxInt64+1), message(2, 4)}, 0},
		{"nil-message", []*imap.FetchMessageData{nil}, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := &conn{state: sessionState{selected: &selectedState{mailbox: &searchClockMailbox{data: tt.data}}}}
			got, err := searchResponseModSeq(context.Background(), c, &searchArgs{}, &SearchResult{UIDs: []imap.UID{1, 2}}, []imap.UID{1, 2})
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
