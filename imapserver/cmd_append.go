package imapserver

import (
	"context"
	"errors"
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
			// A semantic refusal the backend or this layer raised — an
			// unresolvable CATENATE URL, a payload the backend rejects — carries
			// its own response code and left the wire at a part boundary, so it
			// is reported as itself.
			var protocolErr *imap.Error
			if errors.As(err, &protocolErr) {
				// collectAppendPayload read every part, but the command
				// terminator is consumed here, and skipping it leaves the
				// trailing CRLF — and any further messages — to be re-parsed as
				// commands. That is what produced a spurious untagged BAD after
				// each refusal.
				for {
					next, drainErr := readAppendContinuation(ctx, c, false)
					if drainErr != nil {
						return c.writeBad(command.tag, "invalid APPEND payload")
					}
					if next.step == appendStepDone {
						break
					}
					discard, collectErr := c.collectAppendPayload(ctx, &next.message)
					if collectErr != nil {
						// A later message failed too. One refusal is reported;
						// the wire still has to be emptied.
						continue
					}
					discard.drain()
					discard.close()
				}
				return writeBackendError(c, command.tag, "APPEND", err)
			}
			// Anything else means the wire is out of step, and there is nothing
			// left to drain against.
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
		// Defence in depth behind decodeFailure. A parse that neither collected
		// a CATENATE part nor a literal must already have failed; if some future
		// production ever lets one through, the connection owes the client a BAD
		// rather than the process a nil dereference.
		if message.literal == nil {
			return nil, fmt.Errorf("APPEND message carries no payload")
		}
		literal := &appendLiteral{reader: message.literal, remaining: message.literal.Size()}
		return &appendPayload{reader: literal, literal: message.literal, pending: literal}, nil
	}
	session, _ := c.state.session.(CatenateSession)
	payload := &appendPayload{}
	parts := message.catenate
	// The first failure is remembered and the command is read to its end anyway.
	// Every remaining part must come off the wire before a response is written,
	// or the leftovers are parsed as commands.
	var failure error
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
				if failure == nil {
					failure = fmt.Errorf("imapserver: CATENATE is not implemented by this backend")
				}
				continue
			}
			resolved, err := session.ResolveCatenateURL(ctx, part.url, nil)
			if err != nil || resolved == nil {
				// RFC 4469 section 3 answers an unresolvable URL with
				// NO [BADURL <url>], naming the offending URL so a client
				// catenating several parts learns which one failed.
				//
				// Recorded rather than returned: the rest of the command is still
				// on the wire, and abandoning it there leaves the remainder to be
				// re-parsed as commands. That produced a spurious untagged
				// "* BAD invalid command syntax" after every refusal — harmless
				// only because the decoder is literal-aware, and invisible to a
				// test that stops reading at the tagged response.
				if failure == nil {
					failure = &imap.Error{
						Type:     imap.ErrorTypeNo,
						Code:     imap.CodeBadURL,
						CodeArgs: part.url,
						Text:     "CATENATE URL did not resolve",
					}
				}
				continue
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
	if failure != nil {
		return nil, failure
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
