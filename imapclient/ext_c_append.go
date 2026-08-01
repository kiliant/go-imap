package imapclient

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// AppendMessage is one message in a MULTIAPPEND or a CATENATE source list.
// MULTIAPPEND, RFC 3502; CATENATE, RFC 4469.
//
// Construct with keyed fields only; fields may be added in a future release.
type AppendMessage struct {
	// Flags are set on this message. Nil or empty means no flags.
	Flags []imap.Flag

	// InternalDate sets the message's internal date. Nil omits it.
	InternalDate *time.Time

	// Size is the number of octets that will be read from Literal. Required
	// when Literal is set; ignored for CATENATE URL/TEXT parts that carry
	// their own size.
	Size int64

	// Literal is the raw message. Required for MULTIAPPEND message slots that
	// are not using CATENATE.
	Literal io.Reader

	// Catenate, when non-empty, assembles the message on the server from the
	// listed parts instead of sending Literal. CATENATE, RFC 4469. Requires
	// the CATENATE capability. URL parts that need access tokens depend on
	// URLAUTH (owned by T11); this client sends the URL string as given.
	Catenate []CatenatePart

	// Binary sends the message payload as literal8 (~{n}). Use it when the
	// octets may contain NUL, or when UTF8=ACCEPT/APPEND requires literal8
	// for a UTF-8 message. BINARY / UTF8, RFC 3516 / RFC 9755.
	Binary bool

	_ struct{}
}

// CatenatePart is one element of a CATENATE list: either TEXT or URL.
// CATENATE, RFC 4469 section 5.
//
// Exactly one of Text or URL must be set. Construct with keyed fields only;
// fields may be added in a future release.
type CatenatePart struct {
	// Text is a literal message fragment. Size octets are read from Literal.
	Text *CatenateText

	// URL is an IMAP URL (RFC 5092) referring to a message or part already on
	// the server. Authenticated URLs may need URLAUTH (T11).
	URL string

	_ struct{}
}

// CatenateText is the TEXT part of a CATENATE list.
//
// Construct with keyed fields only; fields may be added in a future release.
type CatenateText struct {
	Size    int64
	Literal io.Reader
	Binary  bool
	_       struct{}
}

// MultiAppendData is the result of MULTIAPPEND / CATENATE APPEND.
//
// Construct with keyed fields only; fields may be added in a future release.
type MultiAppendData struct {
	// UIDValidity and UIDs come from APPENDUID when the server sends one.
	// With MULTIAPPEND the UID set may contain several UIDs; UIDs holds them
	// all. A single-UID APPEND still fills UIDs with one element.
	UIDValidity uint32
	UIDs        imap.UIDSet

	_ struct{}
}

// MultiAppendCommand is an in-flight MULTIAPPEND / CATENATE APPEND.
type MultiAppendCommand struct {
	*Command
	data      *MultiAppendData
	appendErr error
}

// Wait waits for APPEND completion and returns the APPENDUID set when present.
func (cmd *MultiAppendCommand) Wait(ctx context.Context) (*MultiAppendData, error) {
	if cmd == nil || cmd.Command == nil {
		return nil, fmt.Errorf("imapclient: nil multiappend command")
	}
	if err := cmd.Command.Wait(ctx); err != nil {
		if cmd.appendErr != nil {
			return nil, cmd.appendErr
		}
		return nil, err
	}
	if cmd.appendErr != nil {
		return nil, cmd.appendErr
	}
	return cmd.data, nil
}

// MultiAppend appends one or more messages in a single APPEND command.
// MULTIAPPEND, RFC 3502.
//
// A single-element messages slice is a plain APPEND and does not require the
// MULTIAPPEND capability. Two or more messages require MULTIAPPEND. Each
// message may optionally use CATENATE when that capability is advertised.
//
// ctx controls the synchronous literal phase; the returned command's Wait
// waits for the tagged completion.
func (c *Client) MultiAppend(ctx context.Context, mailbox string, messages []AppendMessage) *MultiAppendCommand {
	data := &MultiAppendData{}
	if ctx == nil {
		return &MultiAppendCommand{Command: rejectedCommand(c, "APPEND", "APPEND requires a non-nil context"), data: data}
	}
	if err := ctx.Err(); err != nil {
		return &MultiAppendCommand{Command: failedCommand("APPEND", err), data: data, appendErr: err}
	}
	if mailbox == "" || len(messages) == 0 {
		return &MultiAppendCommand{Command: rejectedCommand(c, "APPEND", "APPEND requires a mailbox and at least one message"), data: data}
	}
	if len(messages) > 1 && !c.Supports("MULTIAPPEND") {
		return &MultiAppendCommand{Command: failedCommand("APPEND", capabilityError("MULTIAPPEND", "MULTIAPPEND")), data: data}
	}
	for i := range messages {
		if err := validateAppendMessage(&messages[i]); err != nil {
			return &MultiAppendCommand{Command: rejectedCommand(c, "APPEND", err.Error()), data: data}
		}
		if len(messages[i].Catenate) > 0 && !c.Supports("CATENATE") {
			return &MultiAppendCommand{Command: failedCommand("APPEND", capabilityError("CATENATE", "CATENATE")), data: data}
		}
	}

	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	stopCancel := make(chan struct{})
	cancelled := make(chan error, 1)
	cancelDone := make(chan struct{})
	go func() {
		defer close(cancelDone)
		select {
		case <-ctx.Done():
			cancelled <- ctx.Err()
			if conn != nil {
				_ = conn.Close()
			}
		case <-stopCancel:
		}
	}()

	var appendErr error
	var streamed int
	cmd := c.beginCommandWithCompletion("APPEND", stateAuthenticated|stateSelected, func(enc *imapwire.Encoder) {
		enc.SP().Mailbox(mailbox)
		for i := range messages {
			enc.SP()
			if err := writeAppendMessage(enc, &messages[i], ctx); err != nil {
				appendErr = err
				// Earlier message(s) may already be on the wire. Closing the
				// transport forces issue's Flush to fail and poison the session
				// — a truncated MULTIAPPEND must not leave a synchronised
				// half-command behind (RFC 3502 is all-or-nothing).
				if streamed > 0 && conn != nil {
					_ = conn.Close()
				}
				return
			}
			streamed++
		}
	}, nil, func(success bool, code, args string) {
		if !success || !strings.EqualFold(code, string(imap.CodeAppendUID)) {
			return
		}
		parsed, err := parseAppendUID(args)
		if err != nil {
			return
		}
		data.UIDValidity = parsed.UIDValidity
		data.UIDs = parsed.DestinationUIDs
	})
	close(stopCancel)
	<-cancelDone
	select {
	case err := <-cancelled:
		appendErr = err
	default:
	}
	return &MultiAppendCommand{Command: cmd, data: data, appendErr: appendErr}
}

// CatenateAppend appends one message assembled on the server from parts.
// CATENATE, RFC 4469.
//
// It is a convenience wrapper around [Client.MultiAppend] with a single
// message whose Catenate field is set. A nil options pointer is valid and
// appends with no flags and no internal date. URL parts that require URLAUTH
// tokens are the caller's responsibility until T11 lands; this method only
// puts the URL string on the wire.
func (c *Client) CatenateAppend(ctx context.Context, mailbox string, parts []CatenatePart, options *AppendOptions) *MultiAppendCommand {
	msg := AppendMessage{Catenate: parts}
	if options != nil {
		msg.Flags = options.Flags
		msg.InternalDate = options.InternalDate
	}
	return c.MultiAppend(ctx, mailbox, []AppendMessage{msg})
}

func validateAppendMessage(m *AppendMessage) error {
	if len(m.Catenate) > 0 {
		if m.Literal != nil {
			return fmt.Errorf("APPEND message cannot set both Literal and Catenate")
		}
		for i, p := range m.Catenate {
			hasText := p.Text != nil
			hasURL := p.URL != ""
			if hasText == hasURL {
				return fmt.Errorf("CATENATE part %d must set exactly one of Text or URL", i)
			}
			if hasText {
				if p.Text.Literal == nil || p.Text.Size < 0 {
					return fmt.Errorf("CATENATE TEXT part %d requires a reader and non-negative size", i)
				}
			}
		}
		return nil
	}
	if m.Literal == nil || m.Size < 0 {
		return fmt.Errorf("APPEND message requires a reader and non-negative size")
	}
	return nil
}

func writeAppendMessage(enc *imapwire.Encoder, m *AppendMessage, ctx context.Context) error {
	if len(m.Flags) != 0 {
		enc.List(len(m.Flags), func(i int) { enc.Flag(string(m.Flags[i])) })
		enc.SP()
	}
	if m.InternalDate != nil {
		enc.DateTime(*m.InternalDate)
		enc.SP()
	}
	if len(m.Catenate) > 0 {
		enc.Atom("CATENATE").SP().Special('(')
		for i, p := range m.Catenate {
			if i > 0 {
				enc.SP()
			}
			if p.URL != "" {
				enc.Atom("URL").SP().Astring(p.URL)
				continue
			}
			enc.Atom("TEXT").SP()
			if err := writeBinaryLiteral(enc, p.Text.Size, p.Text.Binary, p.Text.Literal, ctx); err != nil {
				return err
			}
		}
		enc.Special(')')
		return nil
	}
	return writeBinaryLiteral(enc, m.Size, m.Binary, m.Literal, ctx)
}
