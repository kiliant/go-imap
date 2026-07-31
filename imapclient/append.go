package imapclient

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// AppendOptions configures APPEND and intentionally leaves room for MULTIAPPEND
// and CATENATE. Construct with keyed fields only.
type AppendOptions struct {
	Flags        []imap.Flag
	InternalDate *time.Time
	_            struct{}
}

// AppendData is data returned by APPEND. UIDValidity and UID are zero until
// UIDPLUS APPENDUID response-code parsing is enabled.
//
// Construct with keyed fields only; fields may be added in a future release.
type AppendData struct {
	UIDValidity uint32
	UID         imap.UID
	_           struct{}
}

// AppendCommand is an in-flight APPEND command.
type AppendCommand struct {
	*Command
	data      *AppendData
	appendErr error
}

// Wait waits for APPEND and returns its response data.
func (cmd *AppendCommand) Wait(ctx context.Context) (*AppendData, error) {
	if cmd == nil || cmd.Command == nil {
		return nil, fmt.Errorf("imapclient: nil append command")
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

// Append appends exactly size bytes from message to mailbox. The message is
// streamed directly to the connection; it is never buffered in memory. ctx
// controls the synchronous literal continuation and streaming phase; callers
// then use [AppendCommand.Wait] for the tagged completion.
//
// A nil options pointer is valid and appends with no flags and no internal
// date. A non-nil options pointer supplies those optional APPEND arguments.
func (c *Client) Append(ctx context.Context, mailbox string, options *AppendOptions, size int64, message io.Reader) *AppendCommand {
	data := &AppendData{}
	if ctx == nil {
		return &AppendCommand{Command: rejectedCommand(c, "APPEND", "APPEND requires a non-nil context"), data: data}
	}
	if err := ctx.Err(); err != nil {
		return &AppendCommand{Command: failedCommand("APPEND", err), data: data, appendErr: err}
	}
	if mailbox == "" || message == nil || size < 0 {
		return &AppendCommand{Command: rejectedCommand(c, "APPEND", "APPEND requires a mailbox, reader, and non-negative size"), data: data}
	}
	o := AppendOptions{}
	if options != nil {
		o = *options
	}

	// beginCommand holds Client.mu while the literal is sent. Closing the raw
	// transport is the only way a cancelled context can interrupt a blocked
	// network write without waiting for that lock; beginCommand then observes
	// the write failure and poisons the session in the normal way.
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

	c.literalMu.Lock()
	continued := make(chan struct{})
	clear := c.setContinuation(func(string) error {
		select {
		case continued <- struct{}{}:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	caps := c.Capabilities()
	var appendErr error
	cmd := c.beginCommand("APPEND", stateAuthenticated|stateSelected, func(enc *imapwire.Encoder) {
		defer clear()
		defer enc.SetWaitContinuation(nil)
		enc.SetLiteralPlus(caps["LITERAL+"])
		enc.SetLiteralMinus(caps["LITERAL-"])
		enc.SetWaitContinuation(func() error {
			select {
			case <-continued:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		enc.SP().Mailbox(mailbox)
		if len(o.Flags) != 0 {
			enc.SP().List(len(o.Flags), func(i int) { enc.Flag(string(o.Flags[i])) })
		}
		if o.InternalDate != nil {
			enc.SP().DateTime(*o.InternalDate)
		}
		enc.SP()
		literal, err := enc.Literal(size, false)
		if err != nil {
			appendErr = err
			return
		}
		if _, err := io.Copy(literal, appendContextReader{ctx: ctx, reader: io.LimitReader(message, size)}); err != nil {
			appendErr = err
		}
		if err := literal.Close(); err != nil && appendErr == nil {
			appendErr = err
		}
	}, nil)
	if cmd.tag == "" {
		clear()
	}
	c.literalMu.Unlock()
	close(stopCancel)
	<-cancelDone
	select {
	case err := <-cancelled:
		if appendErr == nil {
			appendErr = err
		}
	default:
	}
	return &AppendCommand{Command: cmd, data: data, appendErr: appendErr}
}

type appendContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r appendContextReader) Read(p []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	default:
		return r.reader.Read(p)
	}
}
