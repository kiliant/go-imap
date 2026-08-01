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

// AppendOptions configures APPEND and intentionally leaves room for MULTIAPPEND
// and CATENATE. Construct with keyed fields only.
type AppendOptions struct {
	Flags        []imap.Flag
	InternalDate *time.Time
	_            struct{}
}

// AppendData is data returned by APPEND. UIDValidity and UID are filled from
// a tagged APPENDUID response code when the server sends one (UIDPLUS, RFC
// 4315 section 3). They stay zero when the server omits the code — for
// example when the destination is not selectable or has UIDNOTSTICKY status.
// MULTIAPPEND may return a UID set; this type reports a single UID and leaves
// UID zero when more than one destination UID is assigned.
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

	// The literal payload is streamed straight to the socket, so a cancelled
	// context has to interrupt a blocked network write. Closing the transport
	// is the only way to do that; beginCommand then observes the write failure
	// and poisons the session in the normal way. Waiting for the continuation
	// request itself is already interruptible, because the session is closed
	// and the command completed by the same close.
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
	cmd := c.beginCommandWithCompletion("APPEND", stateAuthenticated|stateSelected, func(enc *imapwire.Encoder) {
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
	}, nil, func(success bool, code, args string) {
		if !success || !strings.EqualFold(code, string(imap.CodeAppendUID)) {
			return
		}
		parsed, err := parseAppendUID(args)
		if err != nil {
			return
		}
		data.UIDValidity = parsed.UIDValidity
		if countUIDs(parsed.DestinationUIDs) == 1 {
			for _, r := range parsed.DestinationUIDs {
				data.UID = r.Start
				break
			}
		}
	})
	close(stopCancel)
	<-cancelDone
	select {
	case err := <-cancelled:
		// Cancellation takes precedence over whatever the interrupted write
		// reported: closing the transport is how the cancellation was
		// delivered, so the write error is its consequence, not its cause.
		appendErr = err
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
