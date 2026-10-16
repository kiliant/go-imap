package imapserver

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strconv"
	"testing"

	"github.com/kiliant/go-imap"
)

func uidBatchesSelection(count int) *selectedState {
	s := &selectedState{uids: make([]imap.UID, count)}
	for i := range s.uids {
		// Gaps and an absent UID 1 exercise boundaries beyond contiguous IDs.
		s.uids[i] = imap.UID(2*i + 7)
	}
	return s
}

func TestUIDBatchesBoundaries(t *testing.T) {
	for _, tt := range []struct {
		name    string
		count   int
		size    uint32
		indices imap.UIDBatchIndexRange
		want    []imap.UIDRange
	}{
		{name: "empty", size: 500},
		{name: "divisible", count: 1000, size: 500, want: []imap.UIDRange{{Start: 2005, Stop: 1007}, {Start: 1005, Stop: 1}}},
		{name: "remainder", count: 1001, size: 500, want: []imap.UIDRange{{Start: 2007, Stop: 1009}, {Start: 1007, Stop: 9}, {Start: 7, Stop: 1}}},
		{name: "equal", count: 500, size: 500, want: []imap.UIDRange{{Start: 1005, Stop: 1}}},
		{name: "larger", count: 3, size: math.MaxUint32, want: []imap.UIDRange{{Start: 11, Stop: 1}}},
		{name: "page", count: 1001, size: 500, indices: imap.UIDBatchIndexRange{First: 2, Last: 2}, want: []imap.UIDRange{{Start: 1007, Stop: 9}}},
		{name: "clamped", count: 1001, size: 500, indices: imap.UIDBatchIndexRange{First: 3, Last: 4}, want: []imap.UIDRange{{Start: 7, Stop: 1}}},
		{name: "later", count: 3, size: 500, indices: imap.UIDBatchIndexRange{First: 2, Last: 2}},
		{name: "hostile", count: 3, size: 500, indices: imap.UIDBatchIndexRange{First: math.MaxUint32, Last: math.MaxUint32}},
		{name: "100000", count: 100000, size: 100000, indices: imap.UIDBatchIndexRange{First: 1, Last: 1}, want: []imap.UIDRange{{Start: 200005, Stop: 1}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := uidBatchesSelection(tt.count)
			s.savedSearch = imap.UIDSet{{Start: 7, Stop: 11}}
			data, err := s.makeUIDBatches("A1", uidBatchesArgs{size: tt.size, indices: tt.indices})
			if err != nil {
				t.Fatal(err)
			}
			var got []imap.UIDRange
			for _, b := range data.Batches {
				got = append(got, b.Range)
			}
			if data.Tag != "A1" || !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %#v, want %v", data, tt.want)
			}
			if !reflect.DeepEqual(s.savedSearch, imap.UIDSet{{Start: 7, Stop: 11}}) {
				t.Fatal("changed SEARCHRES")
			}
		})
	}
}

func TestUIDBatchesPolicyErrors(t *testing.T) {
	for _, tt := range []struct {
		args uidBatchesArgs
		kind imap.ErrorType
		code imap.ResponseCode
	}{
		{uidBatchesArgs{size: 499}, imap.ErrorTypeNo, imap.CodeTooFew},
		{uidBatchesArgs{size: 500, indices: imap.UIDBatchIndexRange{First: 2, Last: 1}}, imap.ErrorTypeBad, imap.CodeClientBug},
		{uidBatchesArgs{size: 500, indices: imap.UIDBatchIndexRange{Last: 1}}, imap.ErrorTypeBad, imap.CodeClientBug},
		{uidBatchesArgs{size: 500, indices: imap.UIDBatchIndexRange{First: 1, Last: 201}}, imap.ErrorTypeNo, imap.CodeTooMany},
		{uidBatchesArgs{size: math.MaxUint32, indices: imap.UIDBatchIndexRange{First: 1, Last: math.MaxUint32}}, imap.ErrorTypeNo, imap.CodeTooMany},
	} {
		s := uidBatchesSelection(3)
		_, err := s.makeUIDBatches("A1", tt.args)
		var protocol *imap.Error
		if !errors.As(err, &protocol) || protocol.Type != tt.kind || protocol.Code != tt.code {
			t.Fatalf("%+v: %v", tt.args, err)
		}
		if s.uidBatches != nil {
			t.Fatal("failed request changed resend history")
		}
	}
}

func TestUIDBatchesRepeat(t *testing.T) {
	for _, size := range []uint32{500, 501} {
		for _, change := range []string{"add", "expunge", "mixed"} {
			t.Run(change+"/"+strconv.FormatUint(uint64(size), 10), func(t *testing.T) {
				s := uidBatchesSelection(1001)
				args := uidBatchesArgs{size: size}
				if _, err := s.makeUIDBatches("A1", args); err != nil {
					t.Fatal(err)
				}
				threshold := int((size + 1) / 2)
				for i := 0; i < threshold; i++ {
					_, err := s.makeUIDBatches("A2", args)
					var protocol *imap.Error
					if !errors.As(err, &protocol) || protocol.Code != imap.CodeLimit {
						t.Fatalf("repeat after %d changes: %v", i, err)
					}
					if change != "expunge" {
						s.uids = append(s.uids, s.uids[len(s.uids)-1]+1)
					}
					if change != "add" {
						s.uids = s.uids[1:]
					}
				}
				if _, err := s.makeUIDBatches("A3", args); err != nil {
					t.Fatalf("repeat at threshold: %v", err)
				}
				// A successful repeat resets its baseline.
				if _, err := s.makeUIDBatches("A4", args); err == nil {
					t.Fatal("repeat baseline was not reset")
				}
			})
		}
	}
	// Empty selections must not panic while accounting for their highest UID.
	s := uidBatchesSelection(0)
	args := uidBatchesArgs{size: 500}
	if _, err := s.makeUIDBatches("A1", args); err != nil {
		t.Fatal(err)
	}
	s.uids = uidBatchesSelection(250).uids
	if _, err := s.makeUIDBatches("A2", args); err != nil {
		t.Fatal(err)
	}
	args.indices = imap.UIDBatchIndexRange{First: 2, Last: 2}
	if _, err := s.makeUIDBatches("A3", args); err != nil {
		t.Fatal("paging refused", err)
	}
	args.size = 501
	if _, err := s.makeUIDBatches("A4", args); err != nil {
		t.Fatal("size change refused", err)
	}
}

func TestUIDBatchesFrameworkCapability(t *testing.T) {
	// No backend and no CapabilitySupport witness are needed.
	server := New(nil, nil)
	state := &sessionState{state: stateAuthenticated}
	caps := deriveCapabilitiesContext(context.Background(), state, server)
	for _, capability := range caps {
		if capability == "UIDBATCHES" {
			return
		}
	}
	t.Fatalf("UIDBATCHES absent from %v", caps)
}
