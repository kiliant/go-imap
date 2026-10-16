package imapserver

import (
	"context"
	"slices"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapcodec"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// UIDBATCHES (RFC 10022) partitions the framework's selection UID slice, so
// every backend supports it without an interface or capability witness.
//
// Resend accounting intentionally tracks only the last successful request.
// Different sizes/ranges and reselections (even of the same mailbox) are
// allowed. Alternating requests can therefore bypass the repeat limit; this
// is acceptable for inexpensive index arithmetic over the existing UID slice.
// The threshold is N/2 or more, accommodating section 3.1.1's explicit MAY.
// Counts infer removals from the surviving UIDs above the previous highest UID;
// additions that were themselves removed need not be credited.

type uidBatchesArgs struct {
	size    uint32
	indices imap.UIDBatchIndexRange
}

type uidBatchesRequest struct {
	args    uidBatchesArgs
	count   uint64
	highest imap.UID
}

func init() {
	registerCapabilities(capabilityDescriptor{
		Name: "UIDBATCHES", States: stateMaskAuthenticated | stateMaskSelected,
	})
	registerCommand("UIDBATCHES", stateMaskSelected, false, parseUIDBatches, handleUIDBatches)
}

func parseUIDBatches(dec *imapwire.Decoder) (any, int64, error) {
	size, indices, err := imapcodec.ReadUIDBatchesCommand(dec)
	return uidBatchesArgs{size: size, indices: indices}, 16, err
}

func handleUIDBatches(_ context.Context, c *conn, command *queuedCommand) error {
	args := command.args.(uidBatchesArgs)
	data, err := c.state.selected.makeUIDBatches(command.tag, args)
	if err != nil {
		return writeBackendError(c, command.tag, command.name, err)
	}
	c.encoder.BeginResponse(imapwire.ResponseUntagged, "").Atom("UIDBATCHES")
	if err := imapcodec.WriteUIDBatchesResponse(c.encoder, data); err != nil {
		return err
	}
	return c.writeTagged(command.tag, "OK", "UIDBATCHES completed")
}

func (s *selectedState) makeUIDBatches(tag string, args uidBatchesArgs) (*imap.UIDBatchesData, error) {
	if args.size < 500 {
		return nil, &imap.Error{Type: imap.ErrorTypeNo, Code: imap.CodeTooFew, Text: "minimum batch size is 500"}
	}
	first, last := uint64(args.indices.First), uint64(args.indices.Last)
	if first > last || (first == 0) != (last == 0) {
		return nil, &imap.Error{Type: imap.ErrorTypeBad, Code: imap.CodeClientBug, Text: "batch indices must be positive and ordered"}
	}
	size := uint64(args.size)
	if first != 0 && (last-first+1)*size > 100000 {
		return nil, &imap.Error{Type: imap.ErrorTypeNo, Code: imap.CodeTooMany, Text: "batch range exceeds 100000 messages"}
	}
	count := uint64(len(s.uids))
	if previous := s.uidBatches; previous != nil && previous.args == args {
		at, found := slices.BinarySearch(s.uids, previous.highest)
		if found {
			at++
		}
		added := count - uint64(at)
		removed := previous.count + added - count
		if added*2 < size && removed*2 < size {
			return nil, &imap.Error{Type: imap.ErrorTypeNo, Code: imap.CodeLimit, Text: "mailbox has not changed enough to repeat UIDBATCHES"}
		}
	}
	total := (count + size - 1) / size
	if first == 0 {
		first, last = 1, total
	} else if last > total {
		last = total
	}
	data := &imap.UIDBatchesData{Tag: tag}
	// Clamp to the number of existing batches before iterating or indexing.
	// Even MaxUint32:MaxUint32 is a valid (empty) window in a small mailbox.
	for i := first; i <= last; i++ {
		high := count - (i-1)*size
		low := uint64(0)
		if high > size {
			low = high - size
		}
		stop := imap.UID(1)
		if low > 0 {
			stop = s.uids[low]
		}
		data.Batches = append(data.Batches, imap.UIDBatch{Range: imap.UIDRange{Start: s.uids[high-1], Stop: stop}})
	}
	request := &uidBatchesRequest{args: args, count: count}
	if count > 0 {
		request.highest = s.uids[count-1]
	}
	s.uidBatches = request
	return data, nil
}
