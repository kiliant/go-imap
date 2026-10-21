package imapserver

import (
	"context"
	"fmt"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// The LIST return options of LIST-MYRIGHTS (RFC 8440) and LIST-METADATA
// (RFC 9590).
//
// RFC 9738's MESSAGELIMIT= and SAVELIMIT= were removed before v1: they limit
// the messages one command processes, which the framework must enforce, and
// the earlier advertisement-only interface described different semantics.
//
// The two LIST return options are answered the same way LIST-STATUS is: the
// framework issues the per-mailbox query after Session.List has returned, so no
// backend learns that LIST grew an option. What differs is only which optional
// interface supplies the answer.

// listReturnMyRights and listReturnMetadata are the two return options this
// file adds. RFC 8440 section 3, RFC 9590 section 3.
const (
	listReturnMyRights = "MYRIGHTS"
	listReturnMetadata = "METADATA"
)

// writeListMyRights answers RETURN (MYRIGHTS), issuing one untagged MYRIGHTS
// response per mailbox the LIST returned.
//
// A mailbox whose rights cannot be read is skipped rather than failing the
// command: RFC 8440 section 3 expects LIST to succeed, and a mailbox the user
// cannot see the rights of is a normal condition, not an error.
func writeListMyRights(ctx context.Context, c *conn, args *listArgs, mailboxes []string) error {
	if !listWants(args, listReturnMyRights) {
		return nil
	}
	session, ok := c.state.session.(ACLSession)
	if !ok {
		return nil
	}
	for _, mailbox := range mailboxes {
		rights, err := session.MyRights(ctx, mailbox, nil)
		if err != nil {
			continue
		}
		c.encoder.BeginResponse(imapwire.ResponseUntagged, "").Atom("MYRIGHTS").SP().
			Mailbox(mailbox).SP().Astring(string(rights)).CRLF()
		if err := c.encoder.Flush(); err != nil {
			return err
		}
	}
	return nil
}

// writeListMetadata answers RETURN (METADATA (...)), issuing one untagged
// METADATA response per mailbox that has any of the requested entries.
func writeListMetadata(ctx context.Context, c *conn, args *listArgs, mailboxes []string) error {
	if !listWants(args, listReturnMetadata) || len(args.metadataEntries) == 0 {
		return nil
	}
	session, ok := c.state.session.(MetadataSession)
	if !ok {
		return nil
	}
	for _, mailbox := range mailboxes {
		data, err := session.GetMetadata(ctx, mailbox, args.metadataEntries, nil)
		if err != nil || data == nil || len(data.Entries) == 0 {
			continue
		}
		c.encoder.BeginResponse(imapwire.ResponseUntagged, "").Atom("METADATA").SP().Mailbox(mailbox).SP().
			List(len(data.Entries), func(i int) {
				entry := data.Entries[i]
				c.encoder.Astring(string(entry.Name)).SP()
				if entry.Value == nil {
					c.encoder.NIL()
				} else {
					c.encoder.String(*entry.Value)
				}
			}).CRLF()
		if err := c.encoder.Flush(); err != nil {
			return err
		}
	}
	return nil
}

func listWants(args *listArgs, option string) bool {
	for _, requested := range args.returnOptions {
		if requested == option {
			return true
		}
	}
	return false
}

// parseListMetadataEntries reads the entry list that RETURN (METADATA ...)
// carries. RFC 9590 section 3.
func parseListMetadataEntries(decoder *imapwire.Decoder, args *listArgs) error {
	if !decoder.ExpectSP() {
		return decoder.Err()
	}
	readEntry := func() error {
		var entry string
		if !decoder.ExpectAstring(&entry) {
			return decoder.Err()
		}
		args.metadataEntries = append(args.metadataEntries, imap.MetadataEntryName(entry))
		return nil
	}
	if decoder.PeekSpecial('(') {
		return decoder.ExpectList(readEntry)
	}
	return readEntry()
}

// validateListExtensionReturnOptions accepts the two return options this file
// adds, which applyListOptions would otherwise reject as unknown.
func validateListExtensionReturnOptions(c *conn, option string) error {
	advertised := advertisedCapabilities(c)
	switch option {
	case listReturnMyRights:
		if !advertised["LIST-MYRIGHTS"] {
			return fmt.Errorf("LIST return option MYRIGHTS requires LIST-MYRIGHTS")
		}
		return nil
	case listReturnMetadata:
		if !advertised["LIST-METADATA"] {
			return fmt.Errorf("LIST return option METADATA requires LIST-METADATA")
		}
		return nil
	default:
		return fmt.Errorf("unsupported LIST return option %q", option)
	}
}
