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

// printMessage prints the summary line of uid, then the message itself.
//
// The two come from separate commands. FetchCommand.Next hands a response over
// as soon as its body literal starts, and the items a server sends after the
// literal (RFC 3501 leaves the order to the server) are added to its Items
// only once the body has been read. A summary taken from the body's response
// could therefore miss UID, FLAGS or ENVELOPE, and reading them would race
// with the client adding them. A response without a body is complete when it
// is handed over.
func printMessage(ctx context.Context, client *imapclient.Client, uid imap.UID, out, errOut io.Writer) error {
	fmt.Fprintf(errOut, "imapcli: fetching UID %d (BODY.PEEK[])...\n", uid)
	summary, err := fetchSummary(ctx, client, uid)
	if err != nil {
		return err
	}
	if summary == nil {
		return fmt.Errorf("no message with UID %d", uid)
	}
	if err := writeMessageSummary(out, summary); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(out); err != nil {
		return err
	}
	return printMessageBody(ctx, client, uid, out)
}

// fetchSummary returns the UID, FLAGS and ENVELOPE of uid, merged across every
// response that carries them, or nil when the mailbox holds no such message.
func fetchSummary(ctx context.Context, client *imapclient.Client, uid imap.UID) (*imap.FetchMessageData, error) {
	cmd := client.FetchUID(imap.UIDSetNum(uid), nil, imap.FetchItemUID, imap.FetchItemFlags, imap.FetchItemEnvelope)
	var summary *imap.FetchMessageData
	for {
		data, err := cmd.Next(ctx)
		if err == io.EOF {
			return summary, nil
		}
		if err != nil {
			return nil, err
		}
		if got, ok := fetchUID(data); !ok || got != uid {
			continue // an unsolicited FETCH for another message
		}
		if summary == nil {
			summary = &imap.FetchMessageData{SeqNum: data.SeqNum, Items: make(map[imap.FetchDataKey][]imap.FetchData)}
		}
		for key, values := range data.Items {
			summary.Items[key] = append(summary.Items[key], values...)
		}
	}
}

// printMessageBody streams BODY[] of uid to w. A response's Items are read
// only to find the body, before its literal is consumed: until then the client
// adds nothing to them.
func printMessageBody(ctx context.Context, client *imapclient.Client, uid imap.UID, w io.Writer) error {
	cmd := client.FetchUID(imap.UIDSetNum(uid), nil, &imap.FetchItemBodySection{Peek: true})
	printed := false
	for {
		data, err := cmd.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		body := wholeMessageBody(data)
		if body == nil || body.Literal == nil {
			continue
		}
		if printed {
			_ = closeLiteral(body.Literal) // a repeated body
			continue
		}
		if err := writeMessageBody(w, body); err != nil {
			return err
		}
		printed = true
	}
	if !printed {
		return fmt.Errorf("server returned no BODY section")
	}
	_, err := fmt.Fprintln(w)
	return err
}

// writeMessageBody copies body's literal to w and closes it.
func writeMessageBody(w io.Writer, body *imap.FetchDataBodySection) error {
	_, err := io.Copy(w, body.Literal)
	if closeErr := closeLiteral(body.Literal); err == nil {
		err = closeErr
	}
	return err
}

func writeMailboxStatus(w io.Writer, status *imapclient.MailboxStatus) error {
	mode := "read-write"
	if status.ReadOnly {
		mode = "read-only"
	}
	// Unseen is the UNSEEN response code: the sequence number of the first
	// unseen message, not a count and not a UID. Zero means none was reported.
	unseen := ""
	if status.Unseen != 0 {
		unseen = fmt.Sprintf("  First unseen: seq %d", status.Unseen)
	}
	_, err := fmt.Fprintf(w, "Mailbox: %s (%s)\nMessages: %d  Recent: %d%s\nUIDNEXT: %d  UIDVALIDITY: %d\n\n",
		status.Mailbox, mode, status.NumMessages, status.NumRecent, unseen, status.UIDNext, status.UIDValidity)
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
