package imapclient

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// UIDExpungeOptions configures UID EXPUNGE. A nil pointer selects the defaults.
//
// Construct with keyed fields only; fields may be added in a future release.
type UIDExpungeOptions struct {
	// SavedSearchResult sends the SEARCHRES "$" marker in place of the UID
	// set. When it is non-nil the set argument must be empty, because the two
	// address the same argument position. See [SavedSearchResult].
	SavedSearchResult *SavedSearchResult

	_ struct{}
}

// UIDExpunge permanently removes the messages that both carry \Deleted and
// have a UID in set. UIDPLUS, RFC 4315 section 2.1.
//
// It is gated on the UIDPLUS capability and has deliberately **no emulated
// fallback**. Plain EXPUNGE is not a degraded UID EXPUNGE: it removes every
// \Deleted message in the mailbox, including messages another client marked
// while this client was disconnected, which is precisely the data loss RFC 4315
// section 2.1 introduced UID EXPUNGE to prevent. When UIDPLUS is absent this
// returns an [imap.Error] wrapping [ErrCapabilityNotAdvertised] without writing
// anything to the connection, and the caller chooses between the two documented
// RFC 4315 strategies: temporarily clearing \Deleted on the messages to keep and
// issuing EXPUNGE, or accepting the wider removal.
//
// The set may use "*" — the UID EXPUNGE argument is a sequence-set, not the
// stricter uid-set of the COPYUID response code.
func (c *Client) UIDExpunge(set imap.UIDSet, options *UIDExpungeOptions) *Command {
	const name = "UID EXPUNGE"
	if !c.hasCapability("UIDPLUS") {
		return unsupportedCommand(name, "UIDPLUS")
	}
	argument, err := savedResultArgument(c, name, set.String(), options.savedSearchResult())
	if err != nil {
		return failedCommand(name, err)
	}
	return c.uidExpunge(argument)
}

func (o *UIDExpungeOptions) savedSearchResult() *SavedSearchResult {
	if o == nil {
		return nil
	}
	return o.SavedSearchResult
}

// uidExpunge issues UID EXPUNGE with an already-validated argument. The
// argument is either a UID set or the SEARCHRES "$" marker.
func (c *Client) uidExpunge(argument string) *Command {
	return c.beginCommand("UID EXPUNGE", stateSelected, func(enc *imapwire.Encoder) {
		enc.SP()
		if argument == "$" {
			enc.Atom(argument)
		} else {
			writeNumSet(enc, argument)
		}
	}, nil)
}

// parseCopyUID parses the arguments of a COPYUID response code:
//
//	resp-code-copy = "COPYUID" SP nz-number SP uid-set SP uid-set
//
// RFC 4315 section 4. The source and destination sets correspond positionally,
// so neither may contain "*" and both must contain the same number of UIDs.
func parseCopyUID(args string) (*CopyData, error) {
	fields := strings.Fields(args)
	if len(fields) != 3 {
		return nil, fmt.Errorf("invalid COPYUID response code %q", args)
	}
	validity, err := parseUIDValidity(fields[0])
	if err != nil {
		return nil, err
	}
	source, err := parseStaticUIDSet(fields[1])
	if err != nil {
		return nil, err
	}
	destination, err := parseStaticUIDSet(fields[2])
	if err != nil {
		return nil, err
	}
	// RFC 4315 section 3: "The source UID set is in the order the message(s)
	// were copied; the destination UID set corresponds to the source UID set
	// and is in the same order." A pairing of different lengths cannot be
	// interpreted, and guessing would silently mislabel messages.
	if countUIDs(source) != countUIDs(destination) {
		return nil, fmt.Errorf("COPYUID source and destination sets have different lengths: %q", args)
	}
	return &CopyData{HasUIDs: true, UIDValidity: validity, SourceUIDs: source, DestinationUIDs: destination}, nil
}

// parseAppendUID parses the arguments of an APPENDUID response code:
//
//	resp-code-apnd = "APPENDUID" SP nz-number SP append-uid
//
// RFC 4315 section 4. With MULTIAPPEND the second value is a uid-set rather
// than a single UID; both spellings are accepted here.
func parseAppendUID(args string) (*CopyData, error) {
	fields := strings.Fields(args)
	if len(fields) != 2 {
		return nil, fmt.Errorf("invalid APPENDUID response code %q", args)
	}
	validity, err := parseUIDValidity(fields[0])
	if err != nil {
		return nil, err
	}
	destination, err := parseStaticUIDSet(fields[1])
	if err != nil {
		return nil, err
	}
	return &CopyData{HasUIDs: true, UIDValidity: validity, DestinationUIDs: destination}, nil
}

func parseUIDValidity(s string) (uint32, error) {
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil || n == 0 {
		return 0, fmt.Errorf("invalid UIDVALIDITY %q in UIDPLUS response code", s)
	}
	return uint32(n), nil
}

// parseStaticUIDSet parses a uid-set. RFC 4315 section 1 excludes "*" from a
// UID set, and section 3 excludes extraneous UIDs, so a dynamic set here is a
// server error rather than something to pass through.
func parseStaticUIDSet(s string) (imap.UIDSet, error) {
	set, err := imap.ParseUIDSet(s)
	if err != nil {
		return nil, fmt.Errorf("invalid uid-set %q in UIDPLUS response code: %w", s, err)
	}
	if set.Dynamic() {
		return nil, fmt.Errorf("uid-set %q in a UIDPLUS response code must not contain %q", s, "*")
	}
	if set.IsEmpty() {
		return nil, fmt.Errorf("empty uid-set in a UIDPLUS response code")
	}
	return set, nil
}

func countUIDs(set imap.UIDSet) uint64 {
	var total uint64
	for _, r := range set {
		lo, hi := uint64(r.Start), uint64(r.Stop)
		if lo > hi {
			lo, hi = hi, lo
		}
		total += hi - lo + 1
	}
	return total
}
