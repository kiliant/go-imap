package imapclient

import (
	"context"
	"fmt"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapcodec"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// UIDBatchesData is a client-owned UIDBATCHES result.
// Construct with keyed fields only; fields may be added in a future release.
type UIDBatchesData struct {
	imap.UIDBatchesData
	_ struct{}
}

// UIDBatchesOptions configures UIDBatches. Nil requests every batch.
// Construct with keyed fields only; fields may be added in a future release.
type UIDBatchesOptions struct {
	Indices imap.UIDBatchIndexRange
	_       struct{}
}

// UIDBatches retrieves newest-first UID ranges for the selected mailbox
// (RFC 10022). It requires UIDBATCHES and supports UIDONLY. It never changes
// the saved SEARCHRES result and offers no fallback.
//
// size must be at least 500. A supplied index range is inclusive, low to high,
// and may cover at most 100,000 messages (number of batches times size).
// With MESSAGELIMIT, callers should request no more than MessageLimit.
// Batches may be smaller than requested and may include nonexistent UIDs.
//
// Callers must not resend until selecting a different mailbox or until more
// than size/2 messages have arrived or been expunged. The client does not
// track those counts; servers may refuse a repeat with NO [LIMIT].
func (c *Client) UIDBatches(ctx context.Context, size uint32, options *UIDBatchesOptions) (*UIDBatchesData, error) {
	if ctx == nil {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "UIDBATCHES requires a non-nil context"}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var indices imap.UIDBatchIndexRange
	if options != nil {
		indices = options.Indices
	}
	if err := validateUIDBatches(size, indices); err != nil {
		return nil, err
	}
	if !c.Supports("UIDBATCHES") {
		return nil, capabilityError("UIDBATCHES", "UIDBATCHES")
	}
	var data *imap.UIDBatchesData
	cmd := c.issue("UIDBATCHES", commandOptions{
		allowed:         stateSelected,
		write:           func(enc *imapwire.Encoder) { _ = imapcodec.WriteUIDBatchesCommand(enc, size, indices) },
		collectorForTag: func(tag string) commandCollector { return uidBatchesCollector(tag, &data) },
	})
	if err := cmd.Wait(ctx); err != nil {
		return nil, err
	}
	if data == nil {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Tag: cmd.Tag(), Text: "UIDBATCHES completed without its required response"}
	}
	return &UIDBatchesData{UIDBatchesData: *data}, nil
}

func validateUIDBatches(size uint32, indices imap.UIDBatchIndexRange) error {
	code, text := imap.ResponseCode(""), ""
	switch {
	case size < 500:
		code, text = imap.CodeTooFew, "UIDBATCHES size must be at least 500"
	case (indices.First == 0) != (indices.Last == 0) || indices.First > indices.Last:
		code, text = imap.CodeClientBug, "UIDBATCHES requires both indices in ascending order"
	case indices.First != 0 && (uint64(indices.Last)-uint64(indices.First)+1)*uint64(size) > 100000:
		code, text = imap.CodeTooMany, "UIDBATCHES range exceeds 100000 messages"
	}
	if text != "" {
		return &imap.Error{Type: imap.ErrorTypeProtocol, Code: code, Text: text}
	}
	return nil
}

func uidBatchesCollector(tag string, data **imap.UIDBatchesData) commandCollector {
	return func(resp *untaggedResponse) (bool, error) {
		if resp.hasNum || resp.cond != nil || resp.name != "UIDBATCHES" {
			return false, nil
		}
		if err := resp.decodeData(); err != nil {
			return true, err
		}
		parsed := resp.data.uidbatches
		if parsed.Tag != tag {
			return false, nil
		}
		if *data != nil {
			return true, fmt.Errorf("more than one UIDBATCHES response for %s", tag)
		}
		*data = parsed
		return true, nil
	}
}
