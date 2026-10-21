package imapserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

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
	staged, err := c.stageAppend(ctx, args.first, false, nil)
	defer staged.close()
	if err != nil {
		return writeAppendDecodeError(c, command.tag, err, "invalid APPEND payload")
	}
	messages, failure := staged.messages, staged.failure
	origin := nextCommandOrigin()
	if failure != nil {
		return writeBackendError(c, command.tag, "APPEND", appendDestinationRefusal(ctx, c, args.mailbox, failure))
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

// stagedAppend owns the temporary file backing every message until the backend
// call finishes. No mutation runs until the whole command has been consumed.
type stagedAppend struct {
	file     *os.File
	messages []AppendMessage
	failure  error
}

func (s *stagedAppend) close() {
	if s.file != nil {
		_ = s.file.Close()
		_ = os.Remove(s.file.Name())
	}
}

func (c *conn) stageAppend(ctx context.Context, first appendMessage, single bool, refusal error) (*stagedAppend, error) {
	spool, err := os.CreateTemp("", "go-imap-append-*")
	staged := &stagedAppend{file: spool}
	message := first
	var messages []AppendMessage
	failure := refusal
	if failure == nil {
		failure = err
	}
	var offset int64
	count := 0
	for {
		count++
		if failure == nil && single && count > 1 {
			failure = &imap.Error{Type: imap.ErrorTypeBad, Text: "REPLACE requires exactly one message"}
		}
		if failure == nil {
			failure = validateAppendShape(c, count, len(message.catenate) != 0)
		}
		// After a refusal, later messages are only drained: API-STABILITY
		// promises no further URL resolution, and the budget is the command's.
		payload, collectErr := c.collectAppendPayload(ctx, &message, c.server.options.Limits.MaxLiteralBytes-offset, failure != nil)
		if collectErr != nil {
			var protocolErr *imap.Error
			var wireErr *imapwire.Error
			// A literal policy error may wrap an imap.Error, but it ended
			// continuation parsing: the reader has already recovered the line.
			// Only fully consumed payload refusals can continue draining here.
			if errors.As(collectErr, &wireErr) || !errors.As(collectErr, &protocolErr) {
				return staged, collectErr
			}
			if failure == nil {
				failure = collectErr
			}
		} else {
			if failure == nil {
				size, copyErr := io.Copy(spool, io.LimitReader(payload.reader, probingLimit(c.server.options.Limits.MaxLiteralBytes-offset)))
				if copyErr != nil {
					if readTimedOut(copyErr) {
						return staged, copyErr
					}
					failure = copyErr
				} else if size > c.server.options.Limits.MaxLiteralBytes-offset {
					failure = resolvedContentTooBig()
				} else if size == 0 {
					failure = &imap.Error{Type: imap.ErrorTypeNo, Code: imap.CodeCannot, Text: "empty APPEND message"}
				} else {
					staged := AppendMessage{Literal: io.NewSectionReader(spool, offset, size), Flags: message.flags, InternalDate: message.internalDate}
					if message.literal != nil {
						staged.Size, staged.HasSize = message.literal.Size(), true
					}
					if !c.state.enabledCapability("UTF8=ACCEPT") && c.state.revision != revisionIMAP4rev2 {
						failure = validateAppendHeaders(io.NewSectionReader(spool, offset, size))
					}
					messages = append(messages, staged)
					offset += size
				}
			}
			if err := payload.drain(); err != nil {
				payload.close()
				return staged, err
			}
			payload.close()
		}
		next, err := readAppendContinuation(ctx, c, false)
		if err != nil {
			return staged, err
		}
		if next.step == appendStepDone {
			break
		}
		message = next.message
	}
	staged.messages, staged.failure = messages, failure
	return staged, nil
}

// validateAppendHeaders scans only the header section with bounded memory. A
// body may contain 8-bit bytes without negotiating UTF8=ACCEPT.
func validateAppendHeaders(reader io.Reader) error {
	var buffer [4096]byte
	blank := true
	for {
		n, err := reader.Read(buffer[:])
		for _, b := range buffer[:n] {
			if b >= 0x80 {
				return &imap.Error{Type: imap.ErrorTypeNo, Code: imap.CodeCannot, Text: "8-bit message headers require ENABLE UTF8=ACCEPT or IMAP4rev2"}
			}
			if b == '\n' {
				if blank {
					return nil
				}
				blank = true
			} else if b != '\r' {
				blank = false
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func appendFamilyCommand(command *queuedCommand) bool {
	if command.name == "APPEND" || command.name == "REPLACE" {
		return true
	}
	args, ok := command.args.(*uidCommandArgs)
	return ok && args != nil && args.descriptor != nil && args.descriptor.name == "REPLACE"
}

// appendLiteralTooBig identifies a literal size policy failure without turning
// syntax, line-length or nesting errors into upload-size refusals.
func appendLiteralTooBig(err error) bool {
	var wireErr *imapwire.Error
	if errors.As(err, &wireErr) && wireErr.Op == "literal" && errors.Is(err, imapwire.ErrLimitExceeded) {
		return true
	}
	var protocolErr *imap.Error
	return errors.As(err, &protocolErr) && protocolErr.Code == imap.CodeTooBig
}

func writeAppendDecodeError(c *conn, tag string, err error, text string) error {
	if readTimedOut(err) {
		return c.writeReadTimeout(err)
	}
	var protocolErr *imap.Error
	if errors.As(err, &protocolErr) && protocolErr.Type == imap.ErrorTypeBad {
		return writeBackendError(c, tag, "APPEND", err)
	}
	if appendLiteralTooBig(err) {
		return writeBackendError(c, tag, "APPEND", resolvedContentTooBig())
	}
	return c.writeBad(tag, text)
}

// collectAppendPayload assembles one message's bytes, reading the remaining
// CATENATE parts from the wire as it goes.
//
// Plain literals remain streams. CATENATE parts are staged to a bounded
// temporary file, so their byte counts and later syntax can be checked before
// any backend append. On refusal, later literals are drained without resolving
// more URLs, keeping the next command synchronized.
// collectAppendPayload reads one message's payload. budget is what remains of
// the command's aggregate byte limit. When refused, the command has already
// failed: CATENATE literals are drained and no URL is resolved.
func (c *conn) collectAppendPayload(ctx context.Context, message *appendMessage, budget int64, refused bool) (*appendPayload, error) {
	if len(message.catenate) == 0 {
		// Defence in depth behind decodeFailure. A parse that neither collected
		// a CATENATE part nor a literal must already have failed; if some future
		// production ever lets one through, the connection owes the client a BAD
		// rather than the process a nil dereference.
		if message.literal == nil {
			return nil, fmt.Errorf("APPEND message carries no payload")
		}
		if message.literal.Binary() && !advertisedCapabilities(c)["BINARY"] {
			if err := message.literal.Discard(); err != nil {
				return nil, err
			}
			return nil, &imap.Error{Type: imap.ErrorTypeBad, Text: "literal8 APPEND requires BINARY"}
		}
		literal := &appendLiteral{reader: message.literal, remaining: message.literal.Size()}
		return &appendPayload{reader: literal, literal: message.literal, pending: literal}, nil
	}
	session, _ := c.state.session.(CatenateSession)
	if !advertisedCapabilities(c)["CATENATE"] {
		session = nil
	}
	payload := &appendPayload{}
	var failure error
	var file *os.File
	if refused {
		// Its text is never sent: the command's earlier failure is reported.
		failure = &imap.Error{Type: imap.ErrorTypeNo, Text: "APPEND already refused"}
	} else {
		var err error
		file, err = os.CreateTemp("", "go-imap-catenate-*")
		if err != nil {
			failure = &imap.Error{Type: imap.ErrorTypeNo, Code: imap.CodeServerBug, Text: "cannot stage CATENATE content"}
		} else {
			payload.cleanup = func() { _ = file.Close(); _ = os.Remove(file.Name()) }
		}
	}
	success := false
	defer func() {
		if !success {
			payload.close()
		}
	}()
	parts := message.catenate
	var total int64
	for {
		for _, part := range parts {
			if part.literal != nil {
				if failure == nil && part.literal.Binary() {
					failure = &imap.Error{Type: imap.ErrorTypeBad, Text: "CATENATE TEXT requires an ordinary literal"}
				}
				if failure == nil && part.literal.Size() > budget-total {
					failure = resolvedContentTooBig()
				}
				if failure == nil {
					n, err := io.Copy(file, &appendLiteral{reader: part.literal, remaining: part.literal.Size()})
					if err != nil {
						return nil, err
					}
					total += n
					if total > budget {
						failure = resolvedContentTooBig()
					}
				} else {
					if err := part.literal.Discard(); err != nil {
						return nil, err
					}
				}
				continue
			}
			if failure != nil {
				continue
			}
			if session == nil {
				failure = &imap.Error{Type: imap.ErrorTypeNo, Code: imap.CodeCannot, Text: "CATENATE is not available"}
				continue
			}
			if err := requireURLCapabilities(c, part.url); err != nil {
				failure = catenateURLRefusal(part.url, err.Error())
				continue
			}
			resolved, err := session.ResolveCatenateURL(ctx, part.url, nil)
			if err != nil || resolved == nil {
				if resolved != nil {
					_ = resolved.Close()
				}
				failure = catenateURLRefusal(part.url, "CATENATE URL did not resolve")
				continue
			}
			n, readErr := io.Copy(file, io.LimitReader(&literalContextReader{ctx: ctx, reader: resolved}, probingLimit(budget-total)))
			_ = resolved.Close()
			total += n
			if readErr != nil {
				failure = &imap.Error{Type: imap.ErrorTypeNo, Code: imap.CodeUnavailable, Text: "CATENATE URL could not be read"}
			}
			if total > budget {
				failure = resolvedContentTooBig()
			}
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
	payload.reader = io.NewSectionReader(file, 0, total)
	success = true
	return payload, nil
}

// appendDestinationRefusal puts TRYCREATE ahead of a refusal the framework
// reached while staging, before the backend saw the destination. RFC 3501
// section 6.3.11 requires TRYCREATE whenever the destination does not exist,
// and a tagged response carries one code. Status with no items asks the
// session whether the mailbox exists; any answer other than NONEXISTENT or
// TRYCREATE leaves the original refusal in place.
func appendDestinationRefusal(ctx context.Context, c *conn, mailbox string, failure error) error {
	var refusal *imap.Error
	if !errors.As(failure, &refusal) || refusal.Type != imap.ErrorTypeNo || refusal.Code == imap.CodeTryCreate || c.state.session == nil {
		return failure
	}
	_, err := c.state.session.Status(ctx, mailbox, &StatusOptions{})
	var missing *imap.Error
	if errors.As(err, &missing) && (missing.Code == imap.CodeNonExistent || missing.Code == imap.CodeTryCreate) {
		return &imap.Error{Type: imap.ErrorTypeNo, Code: imap.CodeTryCreate, Text: "destination mailbox does not exist"}
	}
	return failure
}

// catenateURLRefusal refuses one CATENATE URL. BADURL must echo the URL, and
// url-resp-text cannot carry "]", CR, LF or NUL (RFC 4469 section 5). The code
// is optional (section 3), so such a URL gets a plain NO. Left to the response
// encoder, it would become SERVERBUG and report the client's URL to OnError as
// a backend failure.
func catenateURLRefusal(url, text string) error {
	if strings.ContainsAny(url, "]\x00\r\n") {
		return &imap.Error{Type: imap.ErrorTypeNo, Text: text}
	}
	return &imap.Error{Type: imap.ErrorTypeNo, Code: imap.CodeBadURL, CodeArgs: url, Text: text}
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
