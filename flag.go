package imap

import "strings"

// Flag is a message flag. RFC 3501 section 2.3.2, RFC 9051 section 2.3.2.
//
// Flag is a string-backed named type and NOT an enumeration. IMAP splits flags
// into system flags, which begin with a backslash and are defined by the
// protocol, and keywords, which are arbitrary atoms chosen by the server or the
// client. A closed type could not represent a keyword at all, and new system
// flags and registered keywords continue to appear — the IANA "IMAP and JMAP
// Keywords" registry (RFC 5788) exists precisely because the set is open.
//
// Comparison is case-insensitive for system flags per RFC 3501; keyword
// case-sensitivity is server-defined in practice. Use [Flag.Equal] rather than
// == when comparing a flag received from a server.
type Flag string

// System flags. RFC 3501 section 2.3.2, RFC 9051 section 2.3.2.
const (
	// FlagSeen marks a message as read.
	FlagSeen Flag = "\\Seen"
	// FlagAnswered marks a message as answered.
	FlagAnswered Flag = "\\Answered"
	// FlagFlagged marks a message as urgent or otherwise special.
	FlagFlagged Flag = "\\Flagged"
	// FlagDeleted marks a message for removal by EXPUNGE.
	FlagDeleted Flag = "\\Deleted"
	// FlagDraft marks a message as not yet complete.
	FlagDraft Flag = "\\Draft"
	// FlagRecent marks a message as newly arrived in this session. It is a
	// session flag: it cannot be set by a client, and RFC 9051 removes it
	// from IMAP4rev2 entirely.
	FlagRecent Flag = "\\Recent"

	// FlagWildcard is the "\*" token, valid only in a PERMANENTFLAGS
	// response code, where it means the server allows the client to define
	// new keywords in this mailbox. RFC 3501 section 7.1.
	FlagWildcard Flag = "\\*"
)

// Keywords registered in the IANA "IMAP and JMAP Keywords" registry, which
// RFC 5788 establishes. Unlike system flags these are ordinary atoms; they are
// listed here for convenience and carry no special handling.
const (
	// FlagForwarded marks a message that has been forwarded.
	FlagForwarded Flag = "$Forwarded"
	// FlagMDNSent marks a message whose message disposition notification
	// has been sent.
	FlagMDNSent Flag = "$MDNSent"
	// FlagJunk marks a message the user considers junk mail.
	FlagJunk Flag = "$Junk"
	// FlagNotJunk marks a message the user considers not to be junk mail.
	FlagNotJunk Flag = "$NotJunk"
	// FlagPhishing marks a message believed to be a phishing attempt.
	FlagPhishing Flag = "$Phishing"
	// FlagImportant marks a message the user or server considers important.
	// RFC 8457.
	FlagImportant Flag = "$Important"
)

// IsSystem reports whether the flag is a system flag, that is whether it begins
// with a backslash. RFC 3501 section 2.3.2.
func (f Flag) IsSystem() bool { return strings.HasPrefix(string(f), "\\") }

// Equal reports whether f and other denote the same flag. System flags are
// compared case-insensitively, as RFC 3501 requires; keywords are compared
// case-insensitively too, because deployed servers differ on whether they
// preserve keyword case and treating "$junk" and "$Junk" as distinct causes
// duplicate keywords in practice.
func (f Flag) Equal(other Flag) bool {
	return strings.EqualFold(string(f), string(other))
}

// ContainsFlag reports whether flags contains f, comparing with [Flag.Equal].
func ContainsFlag(flags []Flag, f Flag) bool {
	for _, g := range flags {
		if g.Equal(f) {
			return true
		}
	}
	return false
}
