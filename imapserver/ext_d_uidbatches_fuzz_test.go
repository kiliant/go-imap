package imapserver

import (
	"testing"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// Exercise command parsing and the selected-state path with independently
// generated sizes and indices, including values that do not fit a signed int.
func FuzzUIDBatchesServer(f *testing.F) {
	f.Add(" 500\r\n", uint32(500), uint32(0), uint32(0), uint16(1001))
	f.Add(" 500 4294967295:4294967295\r\n", uint32(500), ^uint32(0), ^uint32(0), uint16(3))
	f.Add(" 4294967295 1:4294967295\r\n", ^uint32(0), uint32(1), ^uint32(0), uint16(0))
	f.Fuzz(func(t *testing.T, wire string, size, first, last uint32, messages uint16) {
		_, _, _ = parseUIDBatches(imapwire.NewDecoderString(wire, nil))
		s := uidBatchesSelection(int(messages % 4096))
		args := uidBatchesArgs{size: size, indices: imap.UIDBatchIndexRange{First: first, Last: last}}
		data, err := s.makeUIDBatches("A1", args)
		if err != nil {
			return
		}
		seen := make(map[imap.UID]bool)
		previous := ^imap.UID(0)
		for _, batch := range data.Batches {
			r := batch.Range
			if r.Start == 0 || r.Stop == 0 || r.Stop > r.Start || r.Start > previous {
				t.Fatalf("invalid range %v after %v", r, previous)
			}
			previous = r.Stop
			count := uint32(0)
			for _, uid := range s.uids {
				if uid >= r.Stop && uid <= r.Start {
					if seen[uid] {
						t.Fatalf("overlapping UID %v", uid)
					}
					seen[uid] = true
					count++
				}
			}
			if count == 0 || count > size {
				t.Fatalf("range has %d messages for size %d", count, size)
			}
		}
		if first == 0 && len(seen) != len(s.uids) {
			t.Fatalf("covered %d of %d messages", len(seen), len(s.uids))
		}
		if _, err := s.makeUIDBatches("A2", args); err == nil {
			t.Fatal("identical repeat succeeded")
		}
	})
}
