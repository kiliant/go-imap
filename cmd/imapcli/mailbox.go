package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapclient"
)

// mailboxOpts configures a read-only mailbox view.
type mailboxOpts struct {
	Name   string
	Limit  uint // 0 = all messages in the selected window
	Offset uint // skip this many newest messages before applying Limit
	UID    uint // non-zero: print this UID instead of listing
}

func openMailbox(ctx context.Context, client *imapclient.Client, opts mailboxOpts, out, errOut io.Writer) error {
	fmt.Fprintf(errOut, "imapcli: examining %s (read-only)...\n", opts.Name)
	status, err := client.Examine(opts.Name, nil).Wait(ctx)
	if err != nil {
		return err
	}
	if err := writeMailboxStatus(out, status); err != nil {
		return err
	}

	if opts.UID != 0 {
		return printMessage(ctx, client, imap.UID(opts.UID), out, errOut)
	}
	return listMessages(ctx, client, status.NumMessages, opts.Limit, opts.Offset, out, errOut)
}

func listMessages(ctx context.Context, client *imapclient.Client, numMessages uint32, limit, offset uint, out, errOut io.Writer) error {
	start, stop, ok := messageWindow(numMessages, limit, offset)
	if !ok {
		return nil
	}
	set := imap.SeqSetRange(start, stop)
	count := uint32(stop) - uint32(start) + 1
	fmt.Fprintf(errOut, "imapcli: fetching %d message summary(ies) (seq %d:%d)...\n", count, start, stop)

	if _, err := fmt.Fprintf(out, "%6s  %-16s  %-28s  %s\n", "UID", "DATE", "FROM", "SUBJECT"); err != nil {
		return err
	}
	cmd := client.Fetch(set, nil, imap.FetchItemUID, imap.FetchItemFlags, imap.FetchItemEnvelope)
	for {
		data, err := cmd.Next(ctx)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := writeMessageSummary(out, data); err != nil {
			return err
		}
	}
}

// messageWindow selects a sequence range over the newest messages.
// limit 0 means "no cap" (all messages after offset). offset skips that many
// newest messages. ok is false when the window is empty.
func messageWindow(numMessages uint32, limit, offset uint) (start, stop imap.SeqNum, ok bool) {
	if numMessages == 0 || uint32(offset) >= numMessages {
		return 0, 0, false
	}
	stopN := numMessages - uint32(offset)
	startN := uint32(1)
	if limit > 0 && uint32(limit) < stopN {
		startN = stopN - uint32(limit) + 1
	}
	return imap.SeqNum(startN), imap.SeqNum(stopN), true
}

func printMessage(ctx context.Context, client *imapclient.Client, uid imap.UID, out, errOut io.Writer) error {
	fmt.Fprintf(errOut, "imapcli: fetching UID %d (BODY.PEEK[])...\n", uid)
	cmd := client.FetchUID(imap.UIDSetNum(uid), nil,
		imap.FetchItemUID,
		imap.FetchItemFlags,
		imap.FetchItemEnvelope,
		&imap.FetchItemBodySection{Peek: true},
	)
	var seen bool
	for {
		data, err := cmd.Next(ctx)
		if err == io.EOF {
			if !seen {
				return fmt.Errorf("no message with UID %d", uid)
			}
			return nil
		}
		if err != nil {
			return err
		}
		seen = true
		if err := writeMessageSummary(out, data); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(out); err != nil {
			return err
		}
		if err := writeMessageBody(out, data); err != nil {
			return err
		}
	}
}

func writeMessageBody(w io.Writer, data *imap.FetchMessageData) error {
	var body *imap.FetchDataBodySection
	for _, values := range data.Items {
		for _, v := range values {
			if s, ok := v.(*imap.FetchDataBodySection); ok {
				body = s
			}
		}
	}
	if body == nil || body.Literal == nil {
		return fmt.Errorf("server returned no BODY section")
	}
	_, err := io.Copy(w, body.Literal)
	if c, ok := body.Literal.(io.Closer); ok {
		closeErr := c.Close()
		if err == nil {
			err = closeErr
		}
	}
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	return nil
}

func writeMailboxStatus(w io.Writer, status *imapclient.MailboxStatus) error {
	mode := "read-write"
	if status.ReadOnly {
		mode = "read-only"
	}
	_, err := fmt.Fprintf(w, "Mailbox: %s (%s)\nMessages: %d  Recent: %d  Unseen: %d\nUIDNEXT: %d  UIDVALIDITY: %d\n\n",
		status.Mailbox, mode, status.NumMessages, status.NumRecent, status.Unseen, status.UIDNext, status.UIDValidity)
	return err
}

func writeMessageSummary(w io.Writer, data *imap.FetchMessageData) error {
	var (
		uid     imap.UID
		flags   []imap.Flag
		subject string
		from    string
		date    string
	)
	for _, values := range data.Items {
		for _, v := range values {
			switch v := v.(type) {
			case imap.FetchDataUID:
				uid = imap.UID(v)
			case imap.FetchDataFlags:
				flags = []imap.Flag(v)
			case *imap.FetchDataEnvelope:
				if v.Envelope != nil {
					subject = v.Envelope.Subject
					from = formatAddresses(v.Envelope.From)
					if !v.Envelope.Date.IsZero() {
						date = v.Envelope.Date.Local().Format("2006-01-02 15:04")
					}
				}
			}
		}
	}
	flagNote := ""
	if len(flags) > 0 {
		parts := make([]string, 0, len(flags))
		for _, f := range flags {
			parts = append(parts, string(f))
		}
		flagNote = " [" + strings.Join(parts, " ") + "]"
	}
	_, err := fmt.Fprintf(w, "%6d  %-16s  %-28s  %s%s\n", uid, date, truncate(from, 28), subject, flagNote)
	return err
}

func formatAddresses(addrs []imap.Address) string {
	if len(addrs) == 0 {
		return ""
	}
	a := addrs[0]
	if a.IsGroupStart() || a.IsGroupEnd() {
		return a.Mailbox
	}
	if a.Name != "" {
		return a.Name
	}
	return a.Addr()
}

// truncate shortens s to at most n runes. It counts runes rather than bytes so
// a cut never lands inside a multi-byte character such as an umlaut.
func truncate(s string, n int) string {
	r := []rune(s)
	if n <= 0 || len(r) <= n {
		return s
	}
	if n <= 3 {
		return string(r[:n])
	}
	return string(r[:n-3]) + "..."
}
