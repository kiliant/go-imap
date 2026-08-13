package imapserver

import (
	"context"
	"fmt"
	"io"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

type appendArgs struct {
	mailbox string
	// first is the first message's prefix and the start of its payload. The
	// rest of the command is read by the handler between literals, because a
	// literal's length is not on the wire until the previous one is consumed.
	// See ext_c_append.go.
	first appendMessage
}

func parseAppend(decoder *imapwire.Decoder) (any, int64, error) {
	if !decoder.ExpectSP() {
		return nil, 0, decoder.Err()
	}
	args := &appendArgs{}
	if !decoder.ExpectMailbox(&args.mailbox) || !decoder.ExpectSP() {
		return nil, 0, decoder.Err()
	}
	if err := parseAppendMessagePrefix(decoder, &args.first); err != nil {
		return nil, 0, err
	}
	if err := parseAppendPayloadStart(decoder, &args.first); err != nil {
		return nil, 0, err
	}
	return args, int64(len(args.mailbox) + len(args.first.flags)*16), nil
}

// handleAppend stores one message, or several under MULTIAPPEND.
//
// # Atomicity
//
// RFC 3502 section 6.3.11 says a failing MULTIAPPEND should leave no messages
// behind. The framework cannot deliver that over a Session.Append that stores one
// message at a time: it would have to undo earlier appends, and a backend that
// crashed between them would leave them anyway. So it does not pretend — it stops
// at the first failure and reports it, having stored what already succeeded. A
// backend wanting true atomicity needs an interface receiving every message at
// once, which is an additive change if a caller ever needs one.
func handleAppend(ctx context.Context, c *conn, command *queuedCommand) error {
	args, _ := command.args.(*appendArgs)
	if args == nil {
		return c.writeBad(command.tag, "invalid APPEND arguments")
	}
	origin := nextCommandOrigin()
	message := args.first
	var appended []*imap.AppendData
	var failure error

	for {
		payload, err := c.collectAppendPayload(ctx, &message)
		if err != nil {
			// The wire is out of step; there is nothing left to drain against.
			return c.writeBad(command.tag, "invalid APPEND payload")
		}
		if failure == nil {
			failure = validateAppendShape(c, len(appended)+1, len(message.catenate) != 0)
		}
		if failure == nil {
			data, backendErr := c.state.session.Append(ctx, args.mailbox, payload.reader, &AppendOptions{
				MutationOptions: MutationOptions{Origin: origin},
				Flags:           message.flags,
				InternalDate:    message.internalDate,
			})
			if backendErr != nil {
				failure = backendErr
			} else {
				appended = append(appended, data)
			}
		}
		// The payload is drained whatever happened, because the rest of the
		// command cannot be parsed until this message's bytes are off the wire.
		payload.drain()
		payload.close()

		next, err := readAppendContinuation(ctx, c, false)
		if err != nil {
			return c.writeBad(command.tag, "invalid APPEND continuation")
		}
		if next.step == appendStepDone {
			break
		}
		message = next.message
	}

	if failure != nil {
		return writeBackendError(c, command.tag, "APPEND", failure)
	}
	if err := c.drainUpdates(updateAccounting{origin: origin}); err != nil {
		return err
	}
	if uidArgs, ok := appendUIDArgs(appended); ok {
		return writeTaggedCondition(c, command.tag, "OK", imap.CodeAppendUID, uidArgs, "APPEND completed")
	}
	return c.writeTagged(command.tag, "OK", "APPEND completed")
}

// collectAppendPayload assembles one message's bytes, reading the remaining
// CATENATE parts from the wire as it goes.
//
// A plain append streams its single literal straight through. A catenated one is
// assembled part by part: each literal must be fully read before the next part
// can be parsed, so the parts are buffered in order and concatenated. That
// buffering is the price of the grammar rather than a choice — the next part's
// length is not knowable until this one is consumed.
func (c *conn) collectAppendPayload(ctx context.Context, message *appendMessage) (*appendPayload, error) {
	if len(message.catenate) == 0 {
		literal := &appendLiteral{reader: message.literal, remaining: message.literal.Size()}
		return &appendPayload{reader: literal, literal: message.literal, pending: literal}, nil
	}
	session, _ := c.state.session.(CatenateSession)
	payload := &appendPayload{}
	parts := message.catenate
	for {
		for _, part := range parts {
			if part.literal != nil {
				staged, err := io.ReadAll(&appendLiteral{reader: part.literal, remaining: part.literal.Size()})
				if err != nil {
					return nil, err
				}
				payload.parts = append(payload.parts, staged)
				continue
			}
			if session == nil {
				return nil, fmt.Errorf("imapserver: CATENATE is not implemented by this backend")
			}
			resolved, err := session.ResolveCatenateURL(ctx, part.url, nil)
			if err != nil || resolved == nil {
				return nil, fmt.Errorf("imapserver: CATENATE URL did not resolve")
			}
			staged, err := io.ReadAll(resolved)
			_ = resolved.Close()
			if err != nil {
				return nil, err
			}
			payload.parts = append(payload.parts, staged)
		}
		next, err := readAppendContinuation(ctx, c, true)
		if err != nil {
			return nil, err
		}
		if next.step == appendStepCatenateEnd {
			break
		}
		parts = next.message.catenate
	}
	readers := make([]io.Reader, 0, len(payload.parts))
	for _, part := range payload.parts {
		readers = append(readers, bytesReader(part))
	}
	payload.reader = io.MultiReader(readers...)
	return payload, nil
}

func appendUIDArgs(appended []*imap.AppendData) (string, bool) {
	if len(appended) == 0 {
		return "", false
	}
	validity := uint32(0)
	var uids imap.UIDSet
	for _, data := range appended {
		if data == nil || !data.HasUID || data.UIDValidity == 0 || data.UID == 0 {
			return "", false
		}
		if validity == 0 {
			validity = data.UIDValidity
		} else if validity != data.UIDValidity {
			return "", false
		}
		uids.AddNum(data.UID)
	}
	return fmt.Sprintf("%d %s", validity, uids.Normalized().String()), true
}

type appendLiteral struct {
	reader    io.Reader
	remaining int64
}

func (r *appendLiteral) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.reader.Read(p)
	r.remaining -= int64(n)
	if r.remaining == 0 && err == io.EOF {
		err = nil
	}
	return n, err
}
