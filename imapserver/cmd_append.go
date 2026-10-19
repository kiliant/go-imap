package imapserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

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

// handleAppend stages the complete command before handing it to the backend.
// The first literal cannot be committed until the terminator is read: only then
// do we know whether the client sent another message, and whether every literal
// and CATENATE part was valid. A single temporary file bounds framework memory
// independently of ordinary literal size and is removed on every return path.
func handleAppend(ctx context.Context, c *conn, command *queuedCommand) error {
	args, _ := command.args.(*appendArgs)
	if args == nil {
		return c.writeBad(command.tag, "invalid APPEND arguments")
	}
	spool, err := os.CreateTemp("", "go-imap-append-*")
	if spool != nil {
		defer func() { _ = spool.Close(); _ = os.Remove(spool.Name()) }()
	}
	origin := nextCommandOrigin()
	message := args.first
	var messages []AppendMessage
	failure := err
	var offset int64
	count := 0
	for {
		count++
		if failure == nil {
			failure = validateAppendShape(c, count, len(message.catenate) != 0)
		}
		payload, collectErr := c.collectAppendPayload(ctx, &message)
		if collectErr != nil {
			var protocolErr *imap.Error
			if !errors.As(collectErr, &protocolErr) {
				return c.writeBad(command.tag, "invalid APPEND payload")
			}
			if failure == nil {
				failure = collectErr
			}
		} else {
			if failure == nil {
				size, copyErr := io.Copy(spool, payload.reader)
				if copyErr != nil {
					failure = copyErr
				} else if size == 0 {
					failure = &imap.Error{Type: imap.ErrorTypeNo, Code: imap.CodeCannot, Text: "empty APPEND message"}
				} else {
					staged := AppendMessage{Literal: io.NewSectionReader(spool, offset, size), Flags: message.flags, InternalDate: message.internalDate}
					if message.literal != nil {
						staged.Size, staged.HasSize = message.literal.Size(), true
					}
					messages = append(messages, staged)
					offset += size
				}
			}
			payload.drain()
			payload.close()
		}
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
	if err := ctx.Err(); err != nil {
		return writeBackendError(c, command.tag, "APPEND", err)
	}
	var uidArgs string
	var hasUIDs bool
	if len(messages) == 1 {
		message := messages[0]
		data, err := c.state.session.Append(ctx, args.mailbox, message.Literal, &AppendOptions{
			MutationOptions: MutationOptions{Origin: origin}, Flags: message.Flags, InternalDate: message.InternalDate,
			Size: message.Size, HasSize: message.HasSize,
		})
		if err != nil {
			return writeBackendError(c, command.tag, "APPEND", err)
		}
		uidArgs, hasUIDs = appendUIDArgs([]*imap.AppendData{data})
	} else {
		batch, ok := c.state.session.(MultiAppendSession)
		if !ok {
			return c.writeBad(command.tag, "MULTIAPPEND is not available")
		}
		data, err := batch.MultiAppend(ctx, args.mailbox, messages, &MultiAppendOptions{MutationOptions: MutationOptions{Origin: origin}})
		if err != nil {
			return writeBackendError(c, command.tag, "APPEND", err)
		}
		if data != nil && data.HasUIDs && data.UIDValidity != 0 && !data.UIDs.IsEmpty() && !data.UIDs.Dynamic() {
			uidArgs, hasUIDs = fmt.Sprintf("%d %s", data.UIDValidity, data.UIDs.String()), true
		}
	}
	if err := c.drainUpdates(updateAccounting{origin: origin}); err != nil {
		return err
	}
	if hasUIDs {
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
	if !advertisedCapabilities(c)["CATENATE"] {
		session = nil
	}
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
					failure = &imap.Error{Type: imap.ErrorTypeNo, Code: imap.CodeCannot, Text: "CATENATE is not available"}
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
