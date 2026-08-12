package imapclient

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapcodec"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// FetchCommand is an in-flight FETCH or UID FETCH command.
//
// Next yields each matching untagged FETCH response. A response containing a
// body section is yielded as soon as its literal has been announced: consume
// or discard the section before calling Next again. This deliberate
// back-pressure keeps large messages out of memory and makes abandoning a
// section a clear protocol error instead of silently desynchronising the
// connection.
type FetchCommand struct {
	*Command
	responses chan *imap.FetchMessageData

	// stop is closed once the command has completed, and is never sent on.
	// The response channel itself must never be closed: closing it would race
	// with the reader goroutine's send whenever a caller stops consuming a
	// command that the connection then completes.
	stop chan struct{}
}

// fetchLiteralReader couples the decoder's literal interlock to the FETCH
// collector. The collector must not parse the rest of the response until the
// caller consumed this reader.
type fetchLiteralReader struct {
	literal   *imapwire.LiteralReader
	remaining int64
	done      chan struct{}
	once      sync.Once
}

func newFetchLiteralReader(literal *imapwire.LiteralReader) *fetchLiteralReader {
	r := &fetchLiteralReader{literal: literal, remaining: literal.Size(), done: make(chan struct{})}
	if r.remaining == 0 {
		r.finish()
	}
	return r
}

func (r *fetchLiteralReader) finish() { r.once.Do(func() { close(r.done) }) }

func (r *fetchLiteralReader) Read(p []byte) (int, error) {
	n, err := r.literal.Read(p)
	r.remaining -= int64(n)
	if r.remaining == 0 {
		r.finish()
	}
	return n, err
}

func (r *fetchLiteralReader) Close() error {
	err := r.literal.Discard()
	r.remaining = 0
	r.finish()
	return err
}

// Next waits for the next FETCH response claimed by this command. It returns
// io.EOF once the command completed and every response was delivered.
func (cmd *FetchCommand) Next(ctx context.Context) (*imap.FetchMessageData, error) {
	if cmd == nil || cmd.Command == nil {
		return nil, fmt.Errorf("imapclient: nil fetch command")
	}
	select {
	case data := <-cmd.responses:
		return data, nil
	case <-cmd.stop:
		select {
		case data := <-cmd.responses:
			return data, nil
		default:
		}
		if err := cmd.Command.err; err != nil {
			return nil, err
		}
		return nil, io.EOF
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// FetchOptions configures a plain FETCH. A nil pointer selects the defaults.
//
// It carries no fields today. FETCH modifiers that already exist have their own
// entry points — [Client.FetchSync] for CONDSTORE/QRESYNC CHANGEDSINCE and
// [Client.FetchPartial] for the PARTIAL range — because this method had no
// options struct to extend. New modifiers belong here.
//
// Construct with keyed fields only; fields may be added in a future release.
type FetchOptions struct {
	_ struct{}
}

// Fetch issues FETCH for a sequence-number set. To fetch by UID, use
// [Client.FetchUID]; the distinct methods make mixing the two address spaces
// impossible at the call site. A nil options pointer selects the defaults.
func (c *Client) Fetch(set imap.SeqSet, options *FetchOptions, items ...imap.FetchItem) *FetchCommand {
	return c.fetch("FETCH", set.String(), func(n imap.SeqNum) bool { return set.Contains(n) }, items)
}

// FetchUID issues UID FETCH for a UID set. FETCH responses still carry the
// server's sequence number; include [imap.FetchItemUID] when the UID is needed
// in the returned data. A nil options pointer selects the defaults.
func (c *Client) FetchUID(set imap.UIDSet, options *FetchOptions, items ...imap.FetchItem) *FetchCommand {
	return c.fetch("UID FETCH", set.String(), func(imap.SeqNum) bool { return true }, items)
}

func (c *Client) fetch(name, set string, matches func(imap.SeqNum) bool, items []imap.FetchItem) *FetchCommand {
	fc := &FetchCommand{responses: make(chan *imap.FetchMessageData), stop: make(chan struct{})}
	if set == "" || len(items) == 0 {
		fc.Command = rejectedCommand(c, name, "FETCH requires a non-empty set and at least one item")
		close(fc.stop)
		return fc
	}
	if err := validateFetchItems(items); err != nil {
		fc.Command = rejectedCommand(c, name, err.Error())
		close(fc.stop)
		return fc
	}
	fc.Command = c.beginCommand(name, stateSelected, func(enc *imapwire.Encoder) {
		enc.SP()
		writeNumSet(enc, set)
		enc.SP().List(len(items), func(i int) { writeFetchItem(enc, items[i]) })
	}, func(resp *untaggedResponse) (bool, error) {
		if !resp.hasNum || resp.name != "FETCH" || !matches(imap.SeqNum(resp.number)) {
			return false, nil
		}
		return true, readFetchResponse(resp, fc.deliver)
	})
	go func() {
		<-fc.Command.done
		close(fc.stop)
	}()
	return fc
}

func (fc *FetchCommand) deliver(data *imap.FetchMessageData) {
	select {
	case fc.responses <- data:
	case <-fc.stop:
	}
}

func validateFetchItems(items []imap.FetchItem) error {
	for _, item := range items {
		switch v := item.(type) {
		case nil:
			return fmt.Errorf("nil FETCH item")
		case *imap.FetchItemBodySection:
			if v == nil {
				return fmt.Errorf("nil BODY section item")
			}
			if len(v.HeaderFields) != 0 && len(v.HeaderFieldsNot) != 0 {
				return fmt.Errorf("BODY section cannot use both HeaderFields and HeaderFieldsNot")
			}
			if err := validateSection(v.Part, v.Partial); err != nil {
				return err
			}
		case *imap.FetchItemBinarySection:
			if v == nil {
				return fmt.Errorf("nil BINARY section item")
			}
			if err := validateSection(v.Part, v.Partial); err != nil {
				return err
			}
		case *imap.FetchItemBinarySectionSize:
			if v == nil {
				return fmt.Errorf("nil BINARY.SIZE item")
			}
			if err := validateSection(v.Part, nil); err != nil {
				return err
			}
		case *imap.FetchItemBodyStructure:
			if v == nil {
				return fmt.Errorf("nil BODY item")
			}
		case *imap.FetchItemPreview:
			if v == nil {
				return fmt.Errorf("nil PREVIEW item")
			}
		}
	}
	return nil
}

func validateSection(part []int, partial *imap.SectionPartial) error {
	for _, n := range part {
		if n <= 0 {
			return fmt.Errorf("BODY part numbers must be positive")
		}
	}
	if partial != nil && (partial.Offset < 0 || partial.Size <= 0 || partial.Offset > int64(^uint32(0)) || partial.Size > int64(^uint32(0))) {
		return fmt.Errorf("invalid BODY partial range")
	}
	return nil
}

func rejectedCommand(c *Client, name, text string) *Command {
	cmd := &Command{client: c, name: name, done: make(chan struct{})}
	cmd.complete(&imap.Error{Type: imap.ErrorTypeProtocol, Text: text})
	return cmd
}

func writeFetchItem(enc *imapwire.Encoder, item imap.FetchItem) {
	imapcodec.WriteFetchItem(enc, item)
}

func readFetchResponse(resp *untaggedResponse, emit func(*imap.FetchMessageData)) error {
	return imapcodec.ReadFetchResponse(resp.dec, imap.SeqNum(resp.number), func(lr *imapwire.LiteralReader) (io.Reader, func()) {
		stream := newFetchLiteralReader(lr)
		return stream, func() { <-stream.done }
	}, emit)
}

func readFetchObjectID(dec *imapwire.Decoder) (string, error) {
	return imapcodec.ReadFetchObjectID(dec)
}

func formatSectionKey(prefix string, section *imapwire.BodySection) string {
	return imapcodec.FormatSectionKey(prefix, section)
}
