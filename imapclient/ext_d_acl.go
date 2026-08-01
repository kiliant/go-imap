package imapclient

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// ACLRights is an ACL rights string. It is deliberately open-ended: RFC 4314
// defines the base letters and RIGHTS= advertisements name further sets, so a
// closed enum would break on the next rights extension.
type ACLRights string

// ACLEntry is one identifier/rights pair from GETACL. RFC 4314.
//
// Construct with keyed fields only; fields may be added in a future release.
type ACLEntry struct {
	Identifier string
	Rights     ACLRights
	_          struct{}
}

// ACLData is the result of GETACL.
//
// Construct with keyed fields only; fields may be added in a future release.
type ACLData struct {
	Mailbox string
	Entries []ACLEntry
	_       struct{}
}

// ListRightsData is the result of LISTRIGHTS. RFC 4314 section 3.4.
//
// Construct with keyed fields only; fields may be added in a future release.
type ListRightsData struct {
	Mailbox    string
	Identifier string
	Required   ACLRights
	Optional   []ACLRights
	_          struct{}
}

// MyRightsData is the result of MYRIGHTS. RFC 4314 section 3.5.
//
// Construct with keyed fields only; fields may be added in a future release.
type MyRightsData struct {
	Mailbox string
	Rights  ACLRights
	_       struct{}
}

// SetACLOptions configures SETACL. A nil pointer selects the defaults.
//
// Construct with keyed fields only; fields may be added in a future release.
type SetACLOptions struct {
	_ struct{}
}

// GetACL returns the access control list for mailbox. ACL, RFC 4314.
func (c *Client) GetACL(ctx context.Context, mailbox string, options *GetACLOptions) (*ACLData, error) {
	_ = options
	if ctx == nil {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "GETACL requires a non-nil context"}
	}
	if mailbox == "" {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "GETACL requires a mailbox"}
	}
	if !c.Supports("ACL") {
		return nil, capabilityError("GETACL", "ACL")
	}
	data := &ACLData{}
	var got bool
	cmd := c.beginCommand("GETACL", stateAuthenticated|stateSelected, func(enc *imapwire.Encoder) {
		enc.SP().Mailbox(mailbox)
	}, func(resp *untaggedResponse) (bool, error) {
		if resp.hasNum || resp.cond != nil || resp.name != "ACL" {
			return false, nil
		}
		parsed, err := readACLResponse(resp.dec)
		if err != nil {
			return true, err
		}
		*data = *parsed
		got = true
		return true, nil
	})
	if err := cmd.Wait(ctx); err != nil {
		return nil, err
	}
	if !got {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "GETACL completed without an ACL response"}
	}
	return data, nil
}

// GetACLOptions configures GETACL. A nil pointer selects the defaults.
//
// Construct with keyed fields only; fields may be added in a future release.
type GetACLOptions struct {
	_ struct{}
}

// SetACL changes the rights for identifier on mailbox. SETACL, RFC 4314.
//
// An empty rights string removes all rights for the identifier, which is
// distinct from DELETEACL only in that the identifier may remain listed with
// an empty rights string on some servers; prefer [Client.DeleteACL] to remove
// the entry. A nil options pointer selects the defaults.
func (c *Client) SetACL(ctx context.Context, mailbox, identifier string, rights ACLRights, options *SetACLOptions) error {
	_ = options
	if ctx == nil {
		return &imap.Error{Type: imap.ErrorTypeProtocol, Text: "SETACL requires a non-nil context"}
	}
	if mailbox == "" || identifier == "" {
		return &imap.Error{Type: imap.ErrorTypeProtocol, Text: "SETACL requires a mailbox and identifier"}
	}
	if !c.Supports("ACL") {
		return capabilityError("SETACL", "ACL")
	}
	cmd := c.beginCommand("SETACL", stateAuthenticated|stateSelected, func(enc *imapwire.Encoder) {
		enc.SP().Mailbox(mailbox).SP().Astring(identifier).SP().Astring(string(rights))
	}, nil)
	return cmd.Wait(ctx)
}

// DeleteACL removes any ACL entries for identifier on mailbox.
// DELETEACL, RFC 4314.
func (c *Client) DeleteACL(ctx context.Context, mailbox, identifier string, options *DeleteACLOptions) error {
	_ = options
	if ctx == nil {
		return &imap.Error{Type: imap.ErrorTypeProtocol, Text: "DELETEACL requires a non-nil context"}
	}
	if mailbox == "" || identifier == "" {
		return &imap.Error{Type: imap.ErrorTypeProtocol, Text: "DELETEACL requires a mailbox and identifier"}
	}
	if !c.Supports("ACL") {
		return capabilityError("DELETEACL", "ACL")
	}
	cmd := c.beginCommand("DELETEACL", stateAuthenticated|stateSelected, func(enc *imapwire.Encoder) {
		enc.SP().Mailbox(mailbox).SP().Astring(identifier)
	}, nil)
	return cmd.Wait(ctx)
}

// DeleteACLOptions configures DELETEACL. A nil pointer selects the defaults.
//
// Construct with keyed fields only; fields may be added in a future release.
type DeleteACLOptions struct {
	_ struct{}
}

// ListRights returns the rights the server will always grant identifier on
// mailbox, together with the rights that may additionally be granted.
// LISTRIGHTS, RFC 4314 section 3.4.
func (c *Client) ListRights(ctx context.Context, mailbox, identifier string, options *ListRightsOptions) (*ListRightsData, error) {
	_ = options
	if ctx == nil {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "LISTRIGHTS requires a non-nil context"}
	}
	if mailbox == "" || identifier == "" {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "LISTRIGHTS requires a mailbox and identifier"}
	}
	if !c.Supports("ACL") {
		return nil, capabilityError("LISTRIGHTS", "ACL")
	}
	data := &ListRightsData{}
	var got bool
	cmd := c.beginCommand("LISTRIGHTS", stateAuthenticated|stateSelected, func(enc *imapwire.Encoder) {
		enc.SP().Mailbox(mailbox).SP().Astring(identifier)
	}, func(resp *untaggedResponse) (bool, error) {
		if resp.hasNum || resp.cond != nil || resp.name != "LISTRIGHTS" {
			return false, nil
		}
		parsed, err := readListRightsResponse(resp.dec)
		if err != nil {
			return true, err
		}
		*data = *parsed
		got = true
		return true, nil
	})
	if err := cmd.Wait(ctx); err != nil {
		return nil, err
	}
	if !got {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "LISTRIGHTS completed without a LISTRIGHTS response"}
	}
	return data, nil
}

// ListRightsOptions configures LISTRIGHTS. A nil pointer selects the defaults.
//
// Construct with keyed fields only; fields may be added in a future release.
type ListRightsOptions struct {
	_ struct{}
}

// MyRights returns the current user's rights on mailbox. MYRIGHTS, RFC 4314.
func (c *Client) MyRights(ctx context.Context, mailbox string, options *MyRightsOptions) (*MyRightsData, error) {
	_ = options
	if ctx == nil {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "MYRIGHTS requires a non-nil context"}
	}
	if mailbox == "" {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "MYRIGHTS requires a mailbox"}
	}
	if !c.Supports("ACL") {
		return nil, capabilityError("MYRIGHTS", "ACL")
	}
	data := &MyRightsData{}
	var got bool
	cmd := c.beginCommand("MYRIGHTS", stateAuthenticated|stateSelected, func(enc *imapwire.Encoder) {
		enc.SP().Mailbox(mailbox)
	}, func(resp *untaggedResponse) (bool, error) {
		if resp.hasNum || resp.cond != nil || resp.name != "MYRIGHTS" {
			return false, nil
		}
		parsed, err := readMyRightsResponse(resp.dec)
		if err != nil {
			return true, err
		}
		*data = *parsed
		got = true
		return true, nil
	})
	if err := cmd.Wait(ctx); err != nil {
		return nil, err
	}
	if !got {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "MYRIGHTS completed without a MYRIGHTS response"}
	}
	return data, nil
}

// MyRightsOptions configures MYRIGHTS. A nil pointer selects the defaults.
//
// Construct with keyed fields only; fields may be added in a future release.
type MyRightsOptions struct {
	_ struct{}
}

// RightsSets returns the RIGHTS= capability values the server advertises.
// RIGHTS=, RFC 4314. The returned slice is owned by the caller.
func (c *Client) RightsSets() []string {
	return c.CapabilityValues("RIGHTS")
}

// ListReturnMyRights configures LIST RETURN (MYRIGHTS). LIST-MYRIGHTS, RFC 8440.
//
// Do not place this type in [ListOptions.ReturnOptions]: [Client.List] and
// [Client.ListMailboxes] reject types they do not own. Pass it to
// [Client.ListMailboxesExt] or use [Client.ListMailboxesWithMyRights].
//
// Construct with keyed fields only; fields may be added in a future release.
type ListReturnMyRights struct {
	// Handler receives one [MyRightsData] per LIST response that carried
	// MYRIGHTS extended data. A nil Handler still requests the return option
	// but discards the values. Called on the reader goroutine; must not block.
	Handler func(*MyRightsData)
	_       struct{}
}

const listReturnMyRightsKeyword ListReturnOptionKeyword = "MYRIGHTS"

// ListMailboxesWithMyRights lists mailboxes and collects LIST-MYRIGHTS data.
func (c *Client) ListMailboxesWithMyRights(ctx context.Context, reference, pattern string, options *ListOptions) ([]*ListData, []*MyRightsData, error) {
	if ctx == nil {
		return nil, nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "LIST requires a non-nil context"}
	}
	if !c.Supports("LIST-MYRIGHTS") {
		return nil, nil, capabilityError("LIST RETURN (MYRIGHTS)", "LIST-MYRIGHTS")
	}
	var rights []*MyRightsData
	data, err := c.ListMailboxesExt(ctx, reference, pattern, options, &ListExtOptions{
		MyRights: &ListReturnMyRights{Handler: func(d *MyRightsData) {
			rights = append(rights, d)
		}},
	})
	return data, rights, err
}

func readACLResponse(dec *imapwire.Decoder) (*ACLData, error) {
	if !dec.ExpectSP() {
		return nil, dec.Err()
	}
	var mailbox string
	if !dec.ExpectMailbox(&mailbox) {
		return nil, dec.Err()
	}
	data := &ACLData{Mailbox: mailbox}
	for dec.SP() {
		var id, rights string
		if !dec.ExpectAstring(&id) || !dec.ExpectSP() || !dec.ExpectAstring(&rights) {
			return nil, dec.Err()
		}
		data.Entries = append(data.Entries, ACLEntry{Identifier: id, Rights: ACLRights(rights)})
	}
	if !dec.ExpectCRLF() {
		return nil, dec.Err()
	}
	return data, nil
}

func readListRightsResponse(dec *imapwire.Decoder) (*ListRightsData, error) {
	if !dec.ExpectSP() {
		return nil, dec.Err()
	}
	var mailbox, identifier, required string
	if !dec.ExpectMailbox(&mailbox) || !dec.ExpectSP() || !dec.ExpectAstring(&identifier) || !dec.ExpectSP() || !dec.ExpectAstring(&required) {
		return nil, dec.Err()
	}
	data := &ListRightsData{Mailbox: mailbox, Identifier: identifier, Required: ACLRights(required)}
	for dec.SP() {
		var optional string
		if !dec.ExpectAstring(&optional) {
			return nil, dec.Err()
		}
		data.Optional = append(data.Optional, ACLRights(optional))
	}
	if !dec.ExpectCRLF() {
		return nil, dec.Err()
	}
	return data, nil
}

func readMyRightsResponse(dec *imapwire.Decoder) (*MyRightsData, error) {
	if !dec.ExpectSP() {
		return nil, dec.Err()
	}
	var mailbox, rights string
	if !dec.ExpectMailbox(&mailbox) || !dec.ExpectSP() || !dec.ExpectAstring(&rights) {
		return nil, dec.Err()
	}
	if !dec.ExpectCRLF() {
		return nil, dec.Err()
	}
	return &MyRightsData{Mailbox: mailbox, Rights: ACLRights(rights)}, nil
}

// listWithExtendedReturns issues LIST with structured return options that
// this package owns (MYRIGHTS, METADATA). Keyword-only returns still go
// through the ordinary LIST path.
func (c *Client) listWithExtendedReturns(ctx context.Context, reference, pattern string, options *ListOptions) ([]*ListData, error) {
	return c.ListMailboxesExt(ctx, reference, pattern, options, nil)
}

// ListExtOptions carries Group D LIST return options that are not
// [ListReturnOption] values (so [Client.List] / [Client.ListMailboxes] cannot
// reject them by accident). A nil pointer means no extended returns.
//
// Construct with keyed fields only; fields may be added in a future release.
type ListExtOptions struct {
	MyRights *ListReturnMyRights
	Metadata *ListReturnMetadata
	_        struct{}
}

// ListMailboxesExt lists mailboxes with optional LIST-MYRIGHTS / LIST-METADATA
// return data. Other selection and return options still go through options.
// A [*ListReturnStatus] in options is honoured: with LIST-STATUS (or enabled
// rev2) it shares the LIST command; otherwise the N+1 STATUS emulation from
// [Client.ListMailboxes] runs after the extended listing.
func (c *Client) ListMailboxesExt(ctx context.Context, reference, pattern string, options *ListOptions, ext *ListExtOptions) ([]*ListData, error) {
	if ctx == nil {
		return nil, &imap.Error{Type: imap.ErrorTypeProtocol, Text: "LIST requires a non-nil context"}
	}
	var myRights *ListReturnMyRights
	var metadata *ListReturnMetadata
	if ext != nil {
		myRights = ext.MyRights
		metadata = ext.Metadata
	}
	if myRights == nil && metadata == nil {
		return c.ListMailboxes(ctx, reference, pattern, options)
	}
	if myRights != nil && !c.Supports("LIST-MYRIGHTS") {
		return nil, capabilityError("LIST RETURN (MYRIGHTS)", "LIST-MYRIGHTS")
	}
	if metadata != nil {
		if !c.Supports("LIST-METADATA") {
			return nil, capabilityError("LIST RETURN (METADATA)", "LIST-METADATA")
		}
		if err := validateListReturnMetadata(metadata); err != nil {
			return nil, err
		}
	}
	if !c.supportsAny("LIST-EXTENDED") && !c.rev2Enabled() {
		return nil, capabilityError("LIST extended return options", "LIST-EXTENDED")
	}

	status, rest, err := splitListStatusOption(options)
	if err != nil {
		return nil, err
	}
	emulateStatus := status != nil && !c.supportsAny("LIST-STATUS") && !c.rev2Enabled()
	cmdOpts := options
	if emulateStatus {
		cmdOpts = rest
	}
	data, err := c.listExtendedReturnCommand(reference, pattern, cmdOpts, myRights, metadata).Wait(ctx)
	if err != nil {
		return nil, err
	}
	if !emulateStatus || status.Handler == nil {
		return data, nil
	}
	for _, item := range data {
		if imap.ContainsAttr(item.Attrs, imap.MailboxAttrNoSelect) || imap.ContainsAttr(item.Attrs, imap.MailboxAttrNonExistent) {
			continue
		}
		st, err := c.Status(item.Mailbox, &StatusOptions{Items: status.Items}).Wait(ctx)
		if err != nil {
			var ierr *imap.Error
			if errors.As(err, &ierr) && ierr.Type == imap.ErrorTypeNo {
				continue
			}
			return nil, err
		}
		status.Handler(st)
	}
	return data, nil
}

func (c *Client) listExtendedReturnCommand(reference, pattern string, options *ListOptions, myRights *ListReturnMyRights, metadata *ListReturnMetadata) *ListCommand {
	patterns := []string{pattern}
	var selection []string
	var returns []string
	var status *ListReturnStatus
	if options != nil {
		patterns = append(patterns, options.Patterns...)
		for _, option := range options.SelectionOptions {
			keyword, ok := option.(ListSelectOptionKeyword)
			if !ok {
				return &ListCommand{Command: failedCommand("LIST", fmt.Errorf("imapclient: unsupported LIST selection option %T", option))}
			}
			selection = append(selection, string(keyword))
		}
		for _, option := range options.ReturnOptions {
			switch o := option.(type) {
			case ListReturnOptionKeyword:
				returns = append(returns, string(o))
			case *ListReturnStatus:
				if status != nil {
					return &ListCommand{Command: failedCommand("LIST", &imap.Error{Type: imap.ErrorTypeProtocol, Text: "LIST accepts at most one STATUS return option"})}
				}
				status = o
			default:
				return &ListCommand{Command: failedCommand("LIST", fmt.Errorf("imapclient: unsupported LIST return option %T", option))}
			}
		}
	}
	if myRights != nil {
		returns = appendUniqueReturn(returns, string(listReturnMyRightsKeyword))
	}
	if metadata != nil {
		if err := validateListReturnMetadata(metadata); err != nil {
			return &ListCommand{Command: failedCommand("LIST", err)}
		}
		// METADATA carries an argument list and is written separately; drop a
		// bare METADATA keyword so we do not emit METADATA twice.
		filtered := returns[:0]
		for _, keyword := range returns {
			if strings.EqualFold(keyword, "METADATA") {
				continue
			}
			filtered = append(filtered, keyword)
		}
		returns = filtered
	}
	var statusItemsList []imap.StatusItemKeyword
	if status != nil {
		items, err := statusItems(&StatusOptions{Items: status.Items})
		if err != nil {
			return &ListCommand{Command: failedCommand("LIST", &imap.Error{Type: imap.ErrorTypeProtocol, Text: err.Error()})}
		}
		if len(status.Items) == 0 && c.rev2Enabled() {
			items = withoutStatusItem(items, imap.StatusItemRecent)
		}
		statusItemsList = items
	}

	data := make([]*ListData, 0)
	limit := c.maxUntaggedResponses()
	lists := listCollector("LIST", &data, limit)
	myRightsCol := listMyRightsCollector(myRights, limit)
	metadataCol := listMetadataCollector(metadata, limit)
	var statusCol commandCollector
	if status != nil {
		statusCol = listStatusCollector(status.Handler, limit)
	}
	cmd := c.beginCommand("LIST", stateAuthenticated|stateSelected, func(enc *imapwire.Encoder) {
		if len(selection) != 0 {
			enc.SP().List(len(selection), func(i int) { enc.Atom(selection[i]) })
		}
		enc.SP().Mailbox(reference).SP()
		if len(patterns) == 1 {
			enc.ListMailbox(patterns[0])
		} else {
			enc.List(len(patterns), func(i int) { enc.ListMailbox(patterns[i]) })
		}
		enc.SP().Atom("RETURN").SP().Special('(')
		first := true
		writeAtom := func(s string) {
			if !first {
				enc.SP()
			}
			first = false
			enc.Atom(s)
		}
		for _, keyword := range returns {
			writeAtom(keyword)
		}
		if status != nil {
			writeAtom("STATUS")
			enc.SP().List(len(statusItemsList), func(i int) { enc.Atom(string(statusItemsList[i])) })
		}
		if metadata != nil {
			writeAtom("METADATA")
			enc.SP().List(len(metadata.Entries), func(i int) { enc.Astring(string(metadata.Entries[i])) })
		}
		enc.Special(')')
	}, func(resp *untaggedResponse) (bool, error) {
		if claimed, err := lists(resp); claimed || err != nil {
			return claimed, err
		}
		if claimed, err := myRightsCol(resp); claimed || err != nil {
			return claimed, err
		}
		if claimed, err := metadataCol(resp); claimed || err != nil {
			return claimed, err
		}
		if statusCol != nil {
			return statusCol(resp)
		}
		return false, nil
	})
	return &ListCommand{Command: cmd, data: &data}
}

// listMyRightsCollector claims the untagged MYRIGHTS responses that RFC 8440
// interleaves with LIST (the same pattern as LIST-STATUS).
func listMyRightsCollector(myRights *ListReturnMyRights, limit int) commandCollector {
	count := 0
	return func(resp *untaggedResponse) (bool, error) {
		if myRights == nil || resp.hasNum || resp.cond != nil || resp.name != "MYRIGHTS" {
			return false, nil
		}
		if err := countUntaggedResponse(&count, limit, "MYRIGHTS"); err != nil {
			return true, err
		}
		parsed, err := readMyRightsResponse(resp.dec)
		if err != nil {
			return true, err
		}
		if myRights.Handler != nil {
			myRights.Handler(parsed)
		}
		return true, nil
	}
}

// listMetadataCollector claims the untagged METADATA responses that RFC 9590
// interleaves with LIST.
func listMetadataCollector(metadata *ListReturnMetadata, limit int) commandCollector {
	count := 0
	return func(resp *untaggedResponse) (bool, error) {
		if metadata == nil || resp.hasNum || resp.cond != nil || resp.name != "METADATA" {
			return false, nil
		}
		if err := countUntaggedResponse(&count, limit, "METADATA"); err != nil {
			return true, err
		}
		parsed, err := readMetadataResponse(resp.dec)
		if err != nil {
			return true, err
		}
		if metadata.Handler != nil {
			metadata.Handler(parsed)
		}
		return true, nil
	}
}

func appendUniqueReturn(returns []string, keyword string) []string {
	for _, existing := range returns {
		if strings.EqualFold(existing, keyword) {
			return returns
		}
	}
	return append(returns, keyword)
}
