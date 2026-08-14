package imap

// NOTIFY vocabulary (RFC 5465), shared by the client and the server.
//
// These names live here rather than in either package because both sides speak
// them and they must agree on the spelling. They did not: imapclient canonicalised
// events as "MessageNew" while imapserver upper-cased them to "MESSAGENEW", and
// both declared a NotifyMailboxSpecifier with the same constants. A backend
// author comparing against the wrong spelling would have matched nothing, with no
// compiler error and no failing test.
//
// The mixed-case spelling is the one RFC 5465 section 5 registers and the one the
// frozen imapclient constants already carried, so it is the one kept. Event and
// specifier names are atoms, and therefore case-insensitive on the wire: a server
// must fold what it reads to compare it, and imapserver does.

// NotifyEventName is a NOTIFY event name. It is string-backed and open-ended:
// RFC 5465 section 5 registers the seven below and later extensions add more, so
// a new event is a new value here rather than a new type.
//
// Compare case-insensitively, or fold to the canonical spelling first. A
// consumer that does not recognise an event must say so rather than ignore it —
// see [NotifyMailboxSpecifier] for why.
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

// NotifyMailboxSpecifier names which mailboxes a NOTIFY registration applies to.
// Open-ended for the same reason as [NotifyEventName].
//
// A server that does not recognise a specifier must refuse the registration
// rather than silently watching nothing: the client has been told its request
// was accepted, and will read the resulting silence as "nothing has changed".
// RFC 5465 section 6 provides the BADEVENT response code for exactly this.
type NotifyMailboxSpecifier string

const (
	// NotifySelected covers the currently selected mailbox.
	NotifySelected NotifyMailboxSpecifier = "SELECTED"
	// NotifySelectedDelayed is SELECTED with MessageNew delayed until the client
	// becomes idle or issues a command.
	NotifySelectedDelayed NotifyMailboxSpecifier = "SELECTED-DELAYED"
	// NotifyPersonal covers all personal-namespace mailboxes.
	NotifyPersonal NotifyMailboxSpecifier = "PERSONAL"
	// NotifySubscribed covers all subscribed mailboxes.
	NotifySubscribed NotifyMailboxSpecifier = "SUBSCRIBED"
	// NotifySubtree covers a subtree, and is used with an explicit mailbox list.
	NotifySubtree NotifyMailboxSpecifier = "SUBTREE"
	// NotifyMailboxes covers an explicit list of mailboxes.
	NotifyMailboxes NotifyMailboxSpecifier = "MAILBOXES"
	// NotifyInboxes covers mailboxes the server considers inboxes.
	NotifyInboxes NotifyMailboxSpecifier = "INBOXES"
)

// NotifyEventNames returns every NOTIFY event name this library knows, in the
// order RFC 5465 section 5 registers them.
//
// It exists so consumers stop keeping their own copies. The server folds wire
// input to these spellings, the client mirrors them, and a reference backend
// decides what it can serve by consulting them — three places that previously
// each held a hand-written list, which is how the event and specifier
// vocabularies drifted apart in the first place. A later RFC registering an
// event adds it here and every consumer sees it.
//
// This is a registry, not a validity test. [NotifyEventName] is open by design,
// so a name absent from this list is legal and may simply be newer than this
// release. Reject an event because you cannot serve it — which is a statement
// about your backend — never because it is missing here, or upgrading the
// library will silently start accepting events nothing implements.
//
// The returned slice is a copy; callers may modify it.
func NotifyEventNames() []NotifyEventName {
	return []NotifyEventName{
		NotifyEventMessageNew,
		NotifyEventMessageExpunge,
		NotifyEventFlagChange,
		NotifyEventMailboxName,
		NotifyEventSubscriptionChange,
		NotifyEventMailboxMetadataChange,
		NotifyEventServerMetadataChange,
	}
}

// NotifyMailboxSpecifiers returns every NOTIFY mailbox specifier this library
// knows. See [NotifyEventNames] for why this is a function rather than a list
// each consumer keeps, and for why it is a registry rather than a validity test.
//
// The returned slice is a copy; callers may modify it.
func NotifyMailboxSpecifiers() []NotifyMailboxSpecifier {
	return []NotifyMailboxSpecifier{
		NotifySelected,
		NotifySelectedDelayed,
		NotifyPersonal,
		NotifySubscribed,
		NotifySubtree,
		NotifyMailboxes,
		NotifyInboxes,
	}
}
