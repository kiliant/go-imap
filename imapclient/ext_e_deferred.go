package imapclient

import (
	"errors"
	"strings"

	"github.com/kiliant/go-imap"
)

// Deferred group E capabilities: CONVERT (RFC 5259), IMAPSIEVE= (RFC 6785),
// and ANNOTATE-EXPERIMENT-1 (RFC 5257). Per docs/tasks/T11-ext-group-de.md
// these stay parse-only — no command support — so responses from servers that
// advertise them do not break the client.

// ParseAnnotateArgs validates an ANNOTATE response-code argument list.
// RFC 5257. The arguments are opaque; this preserves them for callers.
func ParseAnnotateArgs(args string) (string, error) {
	return strings.TrimSpace(args), nil
}

// ParseAnnotationsArgs validates an ANNOTATIONS response-code argument list.
// RFC 5257.
func ParseAnnotationsArgs(args string) (string, error) {
	return strings.TrimSpace(args), nil
}

// ParseMaxConvertMessagesArgs parses MAXCONVERTMESSAGES. RFC 5259.
func ParseMaxConvertMessagesArgs(args string) (uint32, error) {
	return responseCodeUint32(strings.TrimSpace(args))
}

// ParseMaxConvertPartsArgs parses MAXCONVERTPARTS. RFC 5259.
func ParseMaxConvertPartsArgs(args string) (uint32, error) {
	return responseCodeUint32(strings.TrimSpace(args))
}

// IMAPSieveScripts returns the script names advertised by IMAPSIEVE=
// capabilities. RFC 6785. The returned slice is owned by the caller.
func (c *Client) IMAPSieveScripts() []string {
	return c.CapabilityValues("IMAPSIEVE")
}

// SupportsConvert reports the CONVERT capability. RFC 5259. Command support
// is deferred; this exists so callers can detect the advertisement.
func (c *Client) SupportsConvert() bool { return c.Supports("CONVERT") }

// SupportsAnnotateExperiment reports ANNOTATE-EXPERIMENT-1. RFC 5257.
// Superseded by METADATA; parse-only.
func (c *Client) SupportsAnnotateExperiment() bool {
	return c.Supports("ANNOTATE-EXPERIMENT-1")
}

// ConvertFromError reports whether err carries a CONVERT-related response
// code (TEMPFAIL, MAXCONVERTMESSAGES, MAXCONVERTPARTS).
func ConvertFromError(err error) (code imap.ResponseCode, args string, ok bool) {
	var ierr *imap.Error
	if !errors.As(err, &ierr) {
		return "", "", false
	}
	switch ierr.Code {
	case imap.CodeTempFail, imap.CodeMaxConvertMessages, imap.CodeMaxConvertParts:
		return ierr.Code, ierr.CodeArgs, true
	default:
		return "", "", false
	}
}