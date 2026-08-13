package imapserver

import (
	"context"
	"fmt"
	"strconv"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// The LIST return options of LIST-MYRIGHTS (RFC 8440) and LIST-METADATA
// (RFC 9590), and the advertisement-only limits of MESSAGELIMIT= and SAVELIMIT=
// (RFC 9738).
//
// The two LIST return options are answered the same way LIST-STATUS is: the
// framework issues the per-mailbox query after Session.List has returned, so no
// backend learns that LIST grew an option. What differs is only which optional
// interface supplies the answer.

// Limits advertised as capability values. RFC 9738 section 3 spells them
// "MESSAGELIMIT=<n>" and "SAVELIMIT=<n>", where the value is part of the token.
const (
	messageLimitPrefix = "MESSAGELIMIT="
	saveLimitPrefix    = "SAVELIMIT="
)

// MessageLimitSession is the optional RFC 9738 support: the largest number of
// messages the server will accept into one mailbox, and the largest it will
// keep. A backend implements it when it enforces such a limit.
//
// Zero means unlimited for either value, which is how a server with no limit
// declines to advertise one.
type MessageLimitSession interface {
	MessageLimits(ctx context.Context, options *MessageLimitOptions) (messageLimit, saveLimit uint32, err error)
}

// MessageLimitOptions configures the limit query. A nil pointer selects the
// defaults.
// Construct with keyed fields only; fields may be added in a future release.
type MessageLimitOptions struct{ _ struct{} }

func init() {
	registerCapabilities(
		// The advertised token carries the value, so the descriptor's Name is a
		// prefix completed at derivation time. deriveCapabilities has no notion
		// of a dynamic name, so the value is appended by the Available hook
		// writing it into the session's cache and the name being rewritten in
		// capabilityValueOverrides.
		capabilityDescriptor{
			Name:            "MESSAGELIMIT",
			States:          stateMaskAuthenticated | stateMaskSelected,
			RequiresBackend: sessionImplements[MessageLimitSession](),
		},
		capabilityDescriptor{
			Name:            "SAVELIMIT",
			States:          stateMaskAuthenticated | stateMaskSelected,
			RequiresBackend: sessionImplements[MessageLimitSession](),
		},
	)
}

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

// capabilityValueOverrides rewrites the parameterised capability tokens whose
// advertised value comes from the backend.
//
// RFC 9738 puts the limit inside the token, so "MESSAGELIMIT" alone is not a
// legal advertisement. The descriptor table decides *whether* to advertise;
// this decides what the token says.
func capabilityValueOverrides(state *sessionState, names []string) []string {
	messageLimit, saveLimit, ok := sessionMessageLimits(state)
	if !ok {
		return names
	}
	rewritten := make([]string, 0, len(names))
	for _, name := range names {
		switch name {
		case "MESSAGELIMIT":
			if messageLimit == 0 {
				continue
			}
			rewritten = append(rewritten, messageLimitPrefix+strconv.FormatUint(uint64(messageLimit), 10))
		case "SAVELIMIT":
			if saveLimit == 0 {
				continue
			}
			rewritten = append(rewritten, saveLimitPrefix+strconv.FormatUint(uint64(saveLimit), 10))
		default:
			rewritten = append(rewritten, name)
		}
	}
	return rewritten
}

// resolveMessageLimits asks the backend for its RFC 9738 limits once, at
// authentication.
//
// It deliberately does not happen during capability derivation. Every extension
// command now calls requireCapability, which derives capabilities, so a backend
// call from there would put an uncancellable round trip behind LIST, GETQUOTA
// and everything else — with context.Background(), since derivation has no
// context to pass. Resolving once against the authentication context keeps the
// "context first on every blocking call" rule intact.
func resolveMessageLimits(ctx context.Context, state *sessionState) {
	if state == nil || state.session == nil {
		return
	}
	session, ok := state.session.(MessageLimitSession)
	if !ok {
		return
	}
	messageLimit, saveLimit, err := session.MessageLimits(ctx, nil)
	if err != nil {
		return
	}
	state.messageLimit, state.saveLimit = messageLimit, saveLimit
}

func sessionMessageLimits(state *sessionState) (uint32, uint32, bool) {
	if state == nil || state.session == nil {
		return 0, 0, false
	}
	if _, ok := state.session.(MessageLimitSession); !ok {
		return 0, 0, false
	}
	return state.messageLimit, state.saveLimit, true
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
