package imapclient

import (
	"context"
	"fmt"
	"strings"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// NotifyEventName is a NOTIFY event name. It is string-backed and open-ended:
// RFC 5465 defines MessageNew, MessageExpunge, FlagChange, MailboxName,
// SubscriptionChange, MailboxMetadataChange and ServerMetadataChange; later
// RFCs may add more.
type NotifyEventName string

// Notify event names from RFC 5465 section 5.
const (
	NotifyEventMessageNew            NotifyEventName = "MessageNew"
	NotifyEventMessageExpunge        NotifyEventName = "MessageExpunge"
	NotifyEventFlagChange            NotifyEventName = "FlagChange"
	NotifyEventMailboxName           NotifyEventName = "MailboxName"
	NotifyEventSubscriptionChange    NotifyEventName = "SubscriptionChange"
	NotifyEventMailboxMetadataChange NotifyEventName = "MailboxMetadataChange"
	NotifyEventServerMetadataChange  NotifyEventName = "ServerMetadataChange"
)

// NotifyMailboxSpecifier names which mailboxes a NOTIFY filter applies to.
type NotifyMailboxSpecifier string

const (
	// NotifySelected covers the currently selected mailbox.
	NotifySelected NotifyMailboxSpecifier = "SELECTED"
	// NotifySelectedDelayed is SELECTED with delayed MessageNew until the
	// client becomes idle or issues a command (RFC 5465).
	NotifySelectedDelayed NotifyMailboxSpecifier = "SELECTED-DELAYED"
	// NotifyPersonal covers all personal-namespace mailboxes.
	NotifyPersonal NotifyMailboxSpecifier = "PERSONAL"
	// NotifySubscribed covers all subscribed mailboxes.
	NotifySubscribed NotifyMailboxSpecifier = "SUBSCRIBED"
	// NotifySubtree is used with [NotifyFilter.Mailboxes] patterns.
	NotifySubtree NotifyMailboxSpecifier = "SUBTREE"
	// NotifyMailboxes is used with an explicit mailbox list in
	// [NotifyFilter.Mailboxes].
	NotifyMailboxes NotifyMailboxSpecifier = "MAILBOXES"
	// NotifyInboxes covers mailboxes the server considers inboxes.
	NotifyInboxes NotifyMailboxSpecifier = "INBOXES"
)

// NotifyFilter is one mailbox set and its events in a NOTIFY command.
// RFC 5465 section 4.
//
// Construct with keyed fields only; fields may be added in a future release.
type NotifyFilter struct {
	// Specifier selects the mailbox set. When it is SUBTREE or MAILBOXES,
	// Mailboxes must be non-empty.
	Specifier NotifyMailboxSpecifier

	// Mailboxes are the names or patterns for SUBTREE / MAILBOXES.
	Mailboxes []string

	// Events are the event names to watch. At least one is required unless
	// None is set.
	Events []NotifyEventName

	// None requests NOTIFY NONE for this filter (disables notifications for
	// the specifier). When set, Events must be empty.
	None bool

	// MessageNewFetchItems, when non-empty, attaches a FETCH item list to a
	// MessageNew event (RFC 5465 MessageNew (fetch-att)).
	MessageNewFetchItems []imap.FetchItem

	_ struct{}
}

// NotifyOptions configures NOTIFY. A nil pointer selects the defaults
// (no STATUS, no NONE — filters must be non-empty).
//
// Construct with keyed fields only; fields may be added in a future release.
type NotifyOptions struct {
	// Status, when true, asks for STATUS notifications for non-selected
	// mailboxes (NOTIFY STATUS ...). RFC 5465 section 4.
	Status bool

	// None cancels all notifications (NOTIFY NONE). When set, filters must
	// be empty and Status must be false.
	None bool

	_ struct{}
}

// Notify registers event notifications. NOTIFY, RFC 5465.
//
// filters is the ordered list of mailbox/event filters. Pass a nil or empty
// filters slice with options.None set to issue NOTIFY NONE and cancel prior
// watches. A nil options pointer selects the defaults; either filters or
// NONE is required.
//
// After a successful NOTIFY, message-level events for the selected mailbox
// arrive through [UnilateralDataHandler] (Exists, Expunge, Fetch, Vanished) —
// the same path IDLE uses. Do not install a second notification mechanism.
//
// STATUS and MailboxName events for non-selected mailboxes are currently
// discarded by the connection-level handler unless a future
// UnilateralDataHandler field claims them; see the T11 escalation note.
func (c *Client) Notify(ctx context.Context, filters []NotifyFilter, options *NotifyOptions) error {
	if ctx == nil {
		return &imap.Error{Type: imap.ErrorTypeProtocol, Text: "NOTIFY requires a non-nil context"}
	}
	if !c.Supports("NOTIFY") {
		return capabilityError("NOTIFY", "NOTIFY")
	}
	if err := validateNotify(filters, options); err != nil {
		return err
	}
	cmd := c.beginCommand("NOTIFY", stateAuthenticated|stateSelected, func(enc *imapwire.Encoder) {
		writeNotify(enc, filters, options)
	}, nil)
	return cmd.Wait(ctx)
}

func validateNotify(filters []NotifyFilter, options *NotifyOptions) error {
	none := options != nil && options.None
	status := options != nil && options.Status
	if none {
		if status || len(filters) != 0 {
			return &imap.Error{Type: imap.ErrorTypeProtocol, Text: "NOTIFY NONE cannot combine with STATUS or filters"}
		}
		return nil
	}
	if len(filters) == 0 {
		return &imap.Error{Type: imap.ErrorTypeProtocol, Text: "NOTIFY requires at least one filter or NONE"}
	}
	for i, filter := range filters {
		if err := validateNotifyFilter(filter); err != nil {
			return &imap.Error{Type: imap.ErrorTypeProtocol, Text: fmt.Sprintf("NOTIFY filter %d: %s", i, err.Error())}
		}
	}
	return nil
}

func validateNotifyFilter(filter NotifyFilter) error {
	spec := strings.ToUpper(string(filter.Specifier))
	switch NotifyMailboxSpecifier(spec) {
	case NotifySelected, NotifySelectedDelayed, NotifyPersonal, NotifySubscribed, NotifyInboxes:
		if len(filter.Mailboxes) != 0 {
			return fmt.Errorf("specifier %s does not take mailbox names", spec)
		}
	case NotifySubtree, NotifyMailboxes:
		if len(filter.Mailboxes) == 0 {
			return fmt.Errorf("specifier %s requires mailbox names", spec)
		}
	default:
		if spec == "" {
			return fmt.Errorf("missing mailbox specifier")
		}
		// Open-ended: a future specifier is accepted if it looks like an atom.
		if strings.ContainsAny(spec, " (){%*\\\"") {
			return fmt.Errorf("invalid mailbox specifier %q", filter.Specifier)
		}
	}
	if filter.None {
		if len(filter.Events) != 0 || len(filter.MessageNewFetchItems) != 0 {
			return fmt.Errorf("NONE cannot combine with events")
		}
		return nil
	}
	if len(filter.Events) == 0 {
		return fmt.Errorf("at least one event is required")
	}
	for _, event := range filter.Events {
		if strings.TrimSpace(string(event)) == "" {
			return fmt.Errorf("empty event name")
		}
	}
	if len(filter.MessageNewFetchItems) != 0 {
		hasNew := false
		for _, event := range filter.Events {
			if strings.EqualFold(string(event), string(NotifyEventMessageNew)) {
				hasNew = true
				break
			}
		}
		if !hasNew {
			return fmt.Errorf("MessageNew fetch items require a MessageNew event")
		}
		if err := validateFetchItems(filter.MessageNewFetchItems); err != nil {
			return err
		}
	}
	return nil
}

func writeNotify(enc *imapwire.Encoder, filters []NotifyFilter, options *NotifyOptions) {
	enc.SP()
	if options != nil && options.None {
		enc.Atom("NONE")
		return
	}
	if options != nil && options.Status {
		enc.Atom("STATUS").SP()
	}
	enc.Atom("SET").SP().List(len(filters), func(i int) {
		writeNotifyFilter(enc, filters[i])
	})
}

func writeNotifyFilter(enc *imapwire.Encoder, filter NotifyFilter) {
	spec := strings.ToUpper(string(filter.Specifier))
	enc.Atom(spec)
	if len(filter.Mailboxes) != 0 {
		enc.SP().List(len(filter.Mailboxes), func(i int) { enc.Mailbox(filter.Mailboxes[i]) })
	}
	enc.SP()
	if filter.None {
		enc.Atom("NONE")
		return
	}
	enc.List(len(filter.Events), func(i int) {
		name := string(filter.Events[i])
		enc.Atom(name)
		if strings.EqualFold(name, string(NotifyEventMessageNew)) && len(filter.MessageNewFetchItems) != 0 {
			enc.SP().List(len(filter.MessageNewFetchItems), func(j int) {
				writeFetchItem(enc, filter.MessageNewFetchItems[j])
			})
		}
	})
}
