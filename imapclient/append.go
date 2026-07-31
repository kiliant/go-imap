package imapclient

import (
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

// Append appends exactly size bytes from message to mailbox. The message is
// streamed directly to the connection; it is never buffered in memory.
func (c *Client) Append(mailbox string, options *AppendOptions, size int64, message io.Reader) *Command {
	if mailbox == "" || message == nil || size < 0 {
		return rejectedCommand(c, "APPEND", "APPEND requires a mailbox, reader, and non-negative size")
	}
	o := AppendOptions{}
	if options != nil {
		o = *options
	}
	c.literalMu.Lock()
	defer c.literalMu.Unlock()
	continued := make(chan struct{})
	clear := c.setContinuation(func(string) error { continued <- struct{}{}; return nil })
	caps := c.Capabilities()
	cmd := c.beginCommand("APPEND", stateAuthenticated|stateSelected, func(enc *imapwire.Encoder) {
		defer clear()
		defer enc.SetWaitContinuation(nil)
		enc.SetLiteralPlus(caps["LITERAL+"])
		enc.SetLiteralMinus(caps["LITERAL-"])
		enc.SetWaitContinuation(func() error { <-continued; return nil })
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
			return
		}
		if _, err := io.Copy(literal, io.LimitReader(message, size)); err != nil {
			return
		}
		_ = literal.Close()
	}, nil)
	if cmd.tag == "" {
		clear()
	}
	return cmd
}
