package imapclient

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// ReplaceOptions configures REPLACE and UID REPLACE. A nil pointer is valid and
// replaces with no flags and no internal date.
//
// Construct with keyed fields only; fields may be added in a future release.
type ReplaceOptions struct {
	// Flags are set on the replacement message.
	Flags []imap.Flag

	// InternalDate sets the replacement message's internal date.
	InternalDate *time.Time

	// AllowNonAtomicFallback permits the emulated three-command sequence
	// described on [Client.Replace] when the server does not advertise
	// REPLACE. It is off by default because the emulation is not atomic.
	AllowNonAtomicFallback bool

	_ struct{}
}

// ReplaceData is the result of a REPLACE. REPLACE, RFC 8508.
//
// Construct with keyed fields only; fields may be added in a future release.
type ReplaceData struct {
	// UIDValidity and UID identify the replacement message, from the APPENDUID
	// response code. RFC 8508 section 4.3 advises servers that also support
	// UIDPLUS to send APPENDUID in an untagged OK before the EXPUNGE, and that
	// is the form this client reads.
	//
	// Both are zero when the server sent no APPENDUID, and both are always
	// zero on the emulated path; see [Client.Replace].
	UIDValidity uint32
	UID         imap.UID

	// Emulated reports that the result came from the non-atomic fallback
	// rather than from a real REPLACE command.
	Emulated bool

	_ struct{}
}

// Replace atomically replaces the message with the given sequence number in the
// selected mailbox by size bytes read from message, stored into mailbox.
// REPLACE, RFC 8508 section 3.2.
//
// The replacement is appended to mailbox, which need not be the selected
// mailbox, and the original is removed from the selected mailbox. The message
// is streamed to the connection and never buffered in memory. REPLACE is only
// valid in the selected state, unlike APPEND (RFC 8508 section 3.5).
//
// # Emulated fallback
//
// If the server does not advertise REPLACE and
// [ReplaceOptions.AllowNonAtomicFallback] is set, this performs the equivalent
// sequence given in RFC 8508 section 3.4:
//
//	APPEND mailbox <message>
//	UID STORE <original> +FLAGS.SILENT (\Deleted)
//	UID EXPUNGE <original>
//
// That emulation is **not atomic**. Each step is a separate command, so another
// session can observe the intermediate states, and a failure or a dropped
// connection between steps leaves either both messages present or — never — the
// original alone: the append is deliberately performed first, so the worst
// outcome is a duplicate rather than a loss. The client cannot report the new
// message's UID on this path, so [ReplaceData.UIDValidity] and
// [ReplaceData.UID] stay zero.
//
// The emulation also requires UIDPLUS, for UID EXPUNGE. Without it the only
// available expunge is the plain EXPUNGE command, which removes every message
// flagged \Deleted in the mailbox rather than just this one — silent data loss,
// so the fallback refuses instead.
//
// Without AllowNonAtomicFallback, an absent REPLACE capability yields an
// [imap.Error] wrapping [ErrCapabilityNotAdvertised] and nothing is sent.
func (c *Client) Replace(ctx context.Context, seqNum imap.SeqNum, mailbox string, options *ReplaceOptions, size int64, message io.Reader) (*ReplaceData, error) {
	return c.replace(ctx, false, uint32(seqNum), mailbox, options, size, message)
}

// ReplaceUID is [Client.Replace] addressing the original message by UID.
// REPLACE, RFC 8508 section 3.3.
func (c *Client) ReplaceUID(ctx context.Context, uid imap.UID, mailbox string, options *ReplaceOptions, size int64, message io.Reader) (*ReplaceData, error) {
	return c.replace(ctx, true, uint32(uid), mailbox, options, size, message)
}

func (c *Client) replace(ctx context.Context, uid bool, number uint32, mailbox string, options *ReplaceOptions, size int64, message io.Reader) (*ReplaceData, error) {
	name := "REPLACE"
	if uid {
		name = "UID REPLACE"
	}
	if ctx == nil {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "REPLACE requires a non-nil context"}
	}
	if number == 0 || mailbox == "" || message == nil || size < 0 {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "REPLACE requires a non-zero message identifier, a mailbox, a reader, and a non-negative size"}
	}
	o := ReplaceOptions{}
	if options != nil {
		o = *options
	}
	if !c.Supports("REPLACE") {
		if !o.AllowNonAtomicFallback {
			return nil, capabilityError(name, "REPLACE")
		}
		return c.replaceEmulated(ctx, uid, number, mailbox, &o, size, message)
	}

	data := &ReplaceData{}
	var replaceErr error
	cmd := c.beginCommand(name, stateSelected, func(enc *imapwire.Encoder) {
		enc.SP().Number(number).SP().Mailbox(mailbox)
		if len(o.Flags) != 0 {
			enc.SP().List(len(o.Flags), func(i int) { enc.Flag(string(o.Flags[i])) })
		}
		if o.InternalDate != nil {
			enc.SP().DateTime(*o.InternalDate)
		}
		enc.SP()
		literal, err := enc.Literal(size, false)
		if err != nil {
			replaceErr = err
			return
		}
		if _, err := io.Copy(literal, appendContextReader{ctx: ctx, reader: io.LimitReader(message, size)}); err != nil {
			replaceErr = err
		}
		if err := literal.Close(); err != nil && replaceErr == nil {
			replaceErr = err
		}
	}, func(resp *untaggedResponse) (bool, error) {
		if resp.cond == nil || resp.name != "OK" || !strings.EqualFold(resp.cond.Text.Code, string(imap.CodeAppendUID)) {
			return false, nil
		}
		validity, newUID, err := parseReplaceAppendUID(resp.cond.Text.Args)
		if err != nil {
			return true, err
		}
		data.UIDValidity, data.UID = validity, newUID
		return true, nil
	})
	if replaceErr != nil {
		_ = cmd.Wait(ctx)
		return nil, replaceErr
	}
	if err := cmd.Wait(ctx); err != nil {
		return nil, err
	}
	return data, nil
}

// replaceEmulated implements the RFC 8508 section 3.4 equivalence for servers
// without REPLACE. See [Client.Replace] for its atomicity caveats.
func (c *Client) replaceEmulated(ctx context.Context, uid bool, number uint32, mailbox string, o *ReplaceOptions, size int64, message io.Reader) (*ReplaceData, error) {
	if !c.Supports("UIDPLUS") {
		return nil, capabilityError("emulated REPLACE", "UIDPLUS")
	}
	if c.State() != StateSelected {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "REPLACE is not valid in " + string(c.State()) + " state"}
	}

	// Resolve the original message's UID first: the expunge must name a UID, so
	// that it removes exactly this message and not every \Deleted message in
	// the mailbox.
	originalUID := imap.UID(number)
	if !uid {
		resolved, err := c.replaceResolveUID(ctx, imap.SeqNum(number))
		if err != nil {
			return nil, err
		}
		originalUID = resolved
	}

	// Append before deleting. If the session dies between the two steps, the
	// mailbox holds a duplicate, which a user can resolve; the reverse order
	// would lose the message outright.
	appendOpts := &AppendOptions{Flags: o.Flags, InternalDate: o.InternalDate}
	if _, err := c.Append(ctx, mailbox, appendOpts, size, message).Wait(ctx); err != nil {
		return nil, err
	}
	set := imap.UIDSetNum(originalUID)
	store := c.StoreUID(set, []imap.Flag{imap.FlagDeleted}, &StoreOptions{Op: StoreFlagsAdd, Silent: true})
	if err := store.Wait(ctx); err != nil {
		return nil, err
	}
	if err := c.replaceExpungeUID(set).Wait(ctx); err != nil {
		return nil, err
	}
	return &ReplaceData{Emulated: true}, nil
}

// replaceResolveUID maps a sequence number to a UID for the emulated path.
func (c *Client) replaceResolveUID(ctx context.Context, seqNum imap.SeqNum) (imap.UID, error) {
	cmd := c.Fetch(imap.SeqSetNum(seqNum), imap.FetchItemUID)
	var found imap.UID
	for {
		data, err := cmd.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, err
		}
		for _, value := range data.Items[imap.FetchDataKey("UID")] {
			if v, ok := value.(imap.FetchDataUID); ok {
				found = imap.UID(v)
			}
		}
	}
	if err := cmd.Wait(ctx); err != nil {
		return 0, err
	}
	if found == 0 {
		return 0, &imap.Error{Type: imap.ErrorTypeProtocol, Text: fmt.Sprintf("no message with sequence number %d", seqNum)}
	}
	return found, nil
}

// replaceExpungeUID issues UID EXPUNGE for the emulated path. It is written
// here rather than shared so that this extension does not depend on the
// internals of another extension's file.
func (c *Client) replaceExpungeUID(set imap.UIDSet) *Command {
	return c.beginCommand("UID EXPUNGE", stateSelected, func(enc *imapwire.Encoder) {
		enc.SP()
		writeNumSet(enc, set.String())
	}, nil)
}

// parseReplaceAppendUID parses an APPENDUID response code:
//
//	resp-text-code =/ "APPENDUID" SP nz-number SP append-uid
//
// RFC 4315 section 3.
func parseReplaceAppendUID(args string) (uint32, imap.UID, error) {
	fields := strings.Fields(strings.TrimSpace(args))
	if len(fields) != 2 {
		return 0, 0, &imap.Error{Type: imap.ErrorTypeProtocol, Text: fmt.Sprintf("invalid APPENDUID response code %q", args)}
	}
	validity, err := strconv.ParseUint(fields[0], 10, 32)
	if err != nil || validity == 0 {
		return 0, 0, &imap.Error{Type: imap.ErrorTypeProtocol, Text: fmt.Sprintf("invalid APPENDUID UIDVALIDITY %q", fields[0])}
	}
	uid, err := strconv.ParseUint(fields[1], 10, 32)
	if err != nil || uid == 0 {
		return 0, 0, &imap.Error{Type: imap.ErrorTypeProtocol, Text: fmt.Sprintf("invalid APPENDUID UID %q", fields[1])}
	}
	return uint32(validity), imap.UID(uid), nil
}
