package imap

import (
	"errors"
	"fmt"
	"strings"
)

// ErrorType classifies an [Error].
//
// It is a string-backed named type rather than an enumeration so that a future
// protocol revision introducing another condition does not require changing the
// type. Unknown values are preserved verbatim.
type ErrorType string

// Error types.
//
// The first three mirror the IMAP response conditions of RFC 3501 section 7.1
// and RFC 9051 section 7.1. [ErrorTypeProtocol] covers everything the peer did
// that the grammar does not allow.
const (
	// ErrorTypeNo is a tagged or untagged NO: the command was understood but
	// did not succeed. RFC 3501 section 7.1.2.
	ErrorTypeNo ErrorType = "NO"
	// ErrorTypeBad is a tagged or untagged BAD: the command was not
	// understood, or a protocol error occurred. RFC 3501 section 7.1.3.
	ErrorTypeBad ErrorType = "BAD"
	// ErrorTypeBye is an untagged BYE: the server is closing the connection.
	// RFC 3501 section 7.1.5.
	ErrorTypeBye ErrorType = "BYE"
	// ErrorTypeProtocol is a violation of the wire grammar by the peer: a
	// malformed response, a response that cannot be reconciled with the
	// connection state, or a limit exceeded. It has no equivalent on the
	// wire; it is how this library reports that the server misbehaved.
	ErrorTypeProtocol ErrorType = "PROTOCOL"
)

// ResponseCode is an IMAP response code, the machine-readable token that may
// appear in square brackets at the start of the human-readable text of a status
// response. RFC 3501 section 7.1, RFC 9051 section 7.1.
//
// ResponseCode is deliberately a string-backed named type and NOT an
// enumeration. The IANA "IMAP Response Codes" registry grows continuously —
// RFC 5530 alone added seventeen codes — so a closed set would force a breaking
// change with every extension. Constants are provided for the codes registered
// with IANA at the time of writing; a code this library does not know is passed
// through exactly as the server sent it, upper-cased. It is never flattened to
// an "unknown" value, because the code may be the only thing that tells the
// caller what went wrong.
//
// Compare with ==:
//
//	var ierr *imap.Error
//	if errors.As(err, &ierr) && ierr.Code == imap.CodeAuthenticationFailed {
//		// ...
//	}
type ResponseCode string

// Response codes registered in the IANA "IMAP Response Codes" registry.
//
// The RFC cited on each constant is the reference recorded in that registry,
// which is not always the most recent RFC to mention the code: the CONDSTORE
// codes, for example, are registered against RFC 4551 even though RFC 7162
// obsoletes it.
//
// Codes not listed here are still usable: convert a string, or compare against
// whatever the server sent.
const (
	// RFC 3501 section 7.1 / RFC 9051 section 7.1.
	CodeAlert          ResponseCode = "ALERT"
	CodeBadCharset     ResponseCode = "BADCHARSET"
	CodeCapability     ResponseCode = "CAPABILITY"
	CodeParse          ResponseCode = "PARSE"
	CodePermanentFlags ResponseCode = "PERMANENTFLAGS"
	CodeReadOnly       ResponseCode = "READ-ONLY"
	CodeReadWrite      ResponseCode = "READ-WRITE"
	CodeTryCreate      ResponseCode = "TRYCREATE"
	CodeUIDNext        ResponseCode = "UIDNEXT"
	CodeUIDValidity    ResponseCode = "UIDVALIDITY"
	CodeUnseen         ResponseCode = "UNSEEN"

	// RFC 9051 section 7.1.
	CodeHasChildren ResponseCode = "HASCHILDREN"

	// RFC 5530: general-purpose failure codes.
	CodeUnavailable          ResponseCode = "UNAVAILABLE"
	CodeAuthenticationFailed ResponseCode = "AUTHENTICATIONFAILED"
	CodeAuthorizationFailed  ResponseCode = "AUTHORIZATIONFAILED"
	CodeExpired              ResponseCode = "EXPIRED"
	CodePrivacyRequired      ResponseCode = "PRIVACYREQUIRED"
	CodeContactAdmin         ResponseCode = "CONTACTADMIN"
	CodeNoPerm               ResponseCode = "NOPERM"
	CodeInUse                ResponseCode = "INUSE"
	CodeExpungeIssued        ResponseCode = "EXPUNGEISSUED"
	CodeCorruption           ResponseCode = "CORRUPTION"
	CodeServerBug            ResponseCode = "SERVERBUG"
	CodeClientBug            ResponseCode = "CLIENTBUG"
	CodeCannot               ResponseCode = "CANNOT"
	CodeLimit                ResponseCode = "LIMIT"
	CodeOverQuota            ResponseCode = "OVERQUOTA"
	CodeAlreadyExists        ResponseCode = "ALREADYEXISTS"
	CodeNonExistent          ResponseCode = "NONEXISTENT"

	// RFC 2221: LOGIN-REFERRALS / RFC 2193: MAILBOX-REFERRALS.
	CodeReferral ResponseCode = "REFERRAL"

	// RFC 3516: BINARY.
	CodeUnknownCTE ResponseCode = "UNKNOWN-CTE"

	// RFC 4315: UIDPLUS.
	CodeUIDNotSticky ResponseCode = "UIDNOTSTICKY"
	CodeAppendUID    ResponseCode = "APPENDUID"
	CodeCopyUID      ResponseCode = "COPYUID"

	// RFC 4467: URLAUTH.
	CodeURLMech ResponseCode = "URLMECH"

	// RFC 4469: CATENATE.
	CodeTooBig ResponseCode = "TOOBIG"
	CodeBadURL ResponseCode = "BADURL"

	// RFC 4551, obsoleted by RFC 7162: CONDSTORE.
	CodeHighestModSeq ResponseCode = "HIGHESTMODSEQ"
	CodeNoModSeq      ResponseCode = "NOMODSEQ"
	CodeModified      ResponseCode = "MODIFIED"

	// RFC 4978: COMPRESS.
	CodeCompressionActive ResponseCode = "COMPRESSIONACTIVE"

	// RFC 5162, obsoleted by RFC 7162; also RFC 9051: QRESYNC.
	CodeClosed ResponseCode = "CLOSED"

	// RFC 5182: SEARCHRES.
	CodeNotSaved ResponseCode = "NOTSAVED"

	// RFC 5255: I18NLEVEL.
	CodeBadComparator ResponseCode = "BADCOMPARATOR"

	// RFC 5257: ANNOTATE-EXPERIMENT-1.
	CodeAnnotate    ResponseCode = "ANNOTATE"
	CodeAnnotations ResponseCode = "ANNOTATIONS"

	// RFC 5259: CONVERT.
	CodeTempFail           ResponseCode = "TEMPFAIL"
	CodeMaxConvertMessages ResponseCode = "MAXCONVERTMESSAGES"
	CodeMaxConvertParts    ResponseCode = "MAXCONVERTPARTS"

	// RFC 5267: CONTEXT.
	CodeNoUpdate ResponseCode = "NOUPDATE"

	// RFC 5464: METADATA. The arguments distinguish MAXSIZE, TOOMANY and
	// NOPRIVATE; see [Error.CodeArgs].
	CodeMetadata ResponseCode = "METADATA"

	// RFC 5465: NOTIFY.
	CodeNotificationOverflow ResponseCode = "NOTIFICATIONOVERFLOW"
	CodeBadEvent             ResponseCode = "BADEVENT"

	// RFC 5466: FILTERS.
	CodeUndefinedFilter ResponseCode = "UNDEFINED-FILTER"

	// RFC 6154: SPECIAL-USE.
	CodeUseAttr ResponseCode = "USEATTR"

	// RFC 6858: downgraded message delivery.
	CodeDowngraded ResponseCode = "DOWNGRADED"

	// RFC 8474: OBJECTID.
	CodeMailboxID ResponseCode = "MAILBOXID"

	// RFC 9585: INPROGRESS.
	CodeInProgress ResponseCode = "INPROGRESS"

	// RFC 9586: UIDONLY.
	CodeUIDRequired ResponseCode = "UIDREQUIRED"
)

// Error is the single error type for every IMAP protocol failure. Extensions
// add response *codes*, never error types, so this type does not grow a
// per-extension family of siblings.
//
// Construct with keyed fields only; fields may be added in a future release.
//
// Match with [errors.As] and compare [Error.Code]:
//
//	var ierr *imap.Error
//	if errors.As(err, &ierr) && ierr.Type == imap.ErrorTypeNo {
//		log.Println(ierr.Code, ierr.Text)
//	}
//
// Transport-level problems (dial failures, TLS handshake errors, timeouts,
// [context.Canceled]) are reported as the underlying error, wrapped where
// useful; they are not protocol failures and do not become an Error.
type Error struct {
	// Type classifies the failure. See [ErrorType].
	Type ErrorType

	// Code is the response code, without the surrounding brackets and
	// without its arguments, or "" if the server sent none. See
	// [ResponseCode].
	Code ResponseCode

	// CodeArgs is the verbatim text between the response code atom and the
	// closing bracket, with the separating space removed, or "" if the code
	// had no arguments.
	//
	// Codes whose arguments this library models — COPYUID, APPENDUID,
	// HIGHESTMODSEQ and the like — surface those as typed data elsewhere.
	// CodeArgs exists so that the arguments of a code we do not model are
	// still available to the caller rather than being discarded, which
	// would be data loss.
	CodeArgs string

	// Text is the human-readable text of the response, with any response
	// code removed. RFC 3501 section 7.1 requires it to be present; some
	// servers send nothing but a code, in which case it is "".
	Text string

	// Tag is the command tag the failing response was tagged with, or "" for
	// an untagged response such as BYE.
	Tag string

	// Err is an optional underlying error, exposed through [Error.Unwrap].
	Err error

	_ struct{}
}

// Error implements the error interface.
func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("imap: ")
	if e.Type != "" {
		b.WriteString(string(e.Type))
	} else {
		b.WriteString("error")
	}
	if e.Tag != "" {
		fmt.Fprintf(&b, " (tag %s)", e.Tag)
	}
	if e.Code != "" {
		b.WriteString(" [")
		b.WriteString(string(e.Code))
		if e.CodeArgs != "" {
			b.WriteByte(' ')
			b.WriteString(e.CodeArgs)
		}
		b.WriteByte(']')
	}
	if e.Text != "" {
		b.WriteString(": ")
		b.WriteString(e.Text)
	} else if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

// Unwrap returns the underlying error, if any, so that [errors.Is] and
// [errors.As] traverse it.
func (e *Error) Unwrap() error { return e.Err }

// Is reports whether target is an *Error with the same Type and Code, so that
// sentinel comparisons such as
//
//	errors.Is(err, &imap.Error{Code: imap.CodeAuthenticationFailed})
//
// work. An empty Type or Code in target matches any value.
func (e *Error) Is(target error) bool {
	var t *Error
	if !errors.As(target, &t) {
		return false
	}
	if t.Type != "" && t.Type != e.Type {
		return false
	}
	if t.Code != "" && t.Code != e.Code {
		return false
	}
	return true
}

// newProtocolError builds an [Error] describing a violation of the wire
// grammar by the peer.
func newProtocolError(format string, args ...any) *Error {
	return &Error{Type: ErrorTypeProtocol, Text: fmt.Sprintf(format, args...)}
}
