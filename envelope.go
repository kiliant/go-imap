package imap

import (
	"strings"
	"time"
)

// Address is a single address from a message header, as IMAP reports it in an
// ENVELOPE. RFC 3501 section 7.4.2 (the address production in section 9),
// RFC 9051 section 7.5.2.
//
// IMAP transmits an address as four fields; this type keeps three of them,
// because the fourth (the source route of RFC 822) was deprecated by RFC 2822
// and is not used by any deployed server.
//
// An address list may contain group syntax markers. A start-of-group marker has
// an empty Host and a non-empty Mailbox holding the group name; an end-of-group
// marker has both empty. Use [Address.IsGroupStart] and [Address.IsGroupEnd]
// rather than testing the fields.
//
// Construct with keyed fields only; fields may be added in a future release.
type Address struct {
	// Name is the display name, already decoded from RFC 2047 encoded-words
	// by whatever produced this value. It may be empty.
	Name string

	// Mailbox is the local part of the address, or the group name for a
	// start-of-group marker.
	Mailbox string

	// Host is the domain part of the address, empty for a group marker.
	Host string
}

// IsGroupStart reports whether the address is the start-of-group marker of
// RFC 3501 section 7.4.2, in which case Mailbox holds the group name.
func (a *Address) IsGroupStart() bool { return a.Host == "" && a.Mailbox != "" }

// IsGroupEnd reports whether the address is the end-of-group marker of
// RFC 3501 section 7.4.2.
func (a *Address) IsGroupEnd() bool { return a.Host == "" && a.Mailbox == "" }

// Addr returns the address in "mailbox@host" form, or "" for a group marker.
func (a *Address) Addr() string {
	if a.Host == "" {
		return ""
	}
	return a.Mailbox + "@" + a.Host
}

// String formats the address in RFC 5322 form, quoting the display name when
// it needs it.
func (a *Address) String() string {
	addr := a.Addr()
	switch {
	case a.Name == "" && addr == "":
		return ""
	case a.Name == "":
		return "<" + addr + ">"
	case addr == "":
		return a.Name
	case strings.ContainsAny(a.Name, `"\`) || strings.ContainsAny(a.Name, "()<>@,;:[]"):
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(a.Name) + `" <` + addr + ">"
	default:
		return a.Name + " <" + addr + ">"
	}
}

// Envelope is the parsed message envelope a server returns for the ENVELOPE
// fetch item. RFC 3501 section 7.4.2, RFC 9051 section 7.5.2.
//
// The envelope is computed by the server from the message's RFC 5322 header. A
// field the message does not carry is the zero value; note in particular that
// servers differ on whether a missing Sender or Reply-To is reported as NIL or
// defaulted to From, so a caller that needs the RFC 5322 defaulting rules
// should apply them itself.
//
// Text fields have their RFC 2047 encoded-words decoded; see [DecodeHeader].
//
// Construct with keyed fields only; fields may be added in a future release.
type Envelope struct {
	// Date is the Date header, parsed. It is the zero time if the header is
	// absent or unparseable.
	Date time.Time

	// Subject is the Subject header, decoded.
	Subject string

	// From, Sender, ReplyTo, To, Cc and Bcc are the corresponding address
	// header fields. Each may contain group markers; see [Address].
	From    []Address
	Sender  []Address
	ReplyTo []Address
	To      []Address
	Cc      []Address
	Bcc     []Address

	// InReplyTo holds the message identifiers of the In-Reply-To header,
	// each including its angle brackets. See [ParseMessageIDList].
	InReplyTo []string

	// MessageID is the Message-ID header including its angle brackets, or ""
	// if absent.
	MessageID string

	_ struct{}
}
