package message

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapmessage"
)

// ErrUnsupportedCriteria identifies a criterion or key this helper cannot
// evaluate. It includes unsubstituted FILTER and criteria exceeding the helper's
// limits of 100 nesting levels or 10,000 nodes. Use errors.Is to identify it.
var ErrUnsupportedCriteria = errors.New("message: unsupported search criterion")

// ErrUnsupportedCharset identifies an unsupported SEARCH argument charset.
// Match returns an *imap.Error with CodeBadCharset wrapping this sentinel.
// Use errors.Is to identify it.
var ErrUnsupportedCharset = errors.New("message: unsupported search charset")

// Metadata supplies mailbox state used by [Match], independently of the bytes.
// RFC822Size is always the supplied value, including zero; supply Message.Size()
// when evaluating the stored message's canonical RFC822.SIZE. It is not inferred.
// Construct with keyed fields only; fields may be added in a future release.
type Metadata struct {
	SeqNum       imap.SeqNum
	UID          imap.UID
	Flags        []imap.Flag
	InternalDate time.Time
	SaveDate     *time.Time
	RFC822Size   int64
	ModSeq       uint64
	// PrivateModSeqs and SharedModSeqs optionally supply per-entry clocks.
	// When both are nil, /flags/ MODSEQ criteria use the message-wide ModSeq
	// (the single-clock model permitted by RFC 7162). Non-nil maps use zero
	// for absent entries.
	PrivateModSeqs map[string]uint64
	SharedModSeqs  map[string]uint64
	EmailID        string
	ThreadID       string
	_              struct{}
}

// MatchOptions configures [Match]. A nil pointer selects the defaults.
// Construct with keyed fields only; fields may be added in a future release.
type MatchOptions struct {
	// Charset selects the encoding of string criteria, not stored message
	// bytes. Supported names are empty/US-ASCII/ASCII/UTF-8/UTF8 (valid UTF-8)
	// and ISO-8859-1/ISO8859-1/LATIN1/LATIN-1, case-insensitively. Unknown names
	// return ErrUnsupportedCharset even when the criteria contain no strings.
	Charset string
	// Now is the reference instant for OLDER/YOUNGER. Zero uses time.Now.
	Now time.Time
	// SavedUIDs supplies the connection's SEARCHRES "$" result.
	SavedUIDs imap.UIDSet
	_         struct{}
}

// Match evaluates a SEARCH tree against a message and mailbox metadata. Use
// value-form criteria from package imap; unsupported keys/types and unsubstituted
// FILTER return ErrUnsupportedCriteria, including in short-circuited branches.
// Nil criteria and nil/zero Messages are invalid. An empty SearchAnd matches all.
//
// Headers and MIME transfer encodings are interpreted best-effort. Unknown
// stored text charsets are searched as their existing bytes; malformed transfer
// encodings fall back to their encoded bytes. FUZZY uses exact matching. These
// portable defaults do not prevent indexed backends implementing richer SEARCH.
// Options, criteria and metadata must remain immutable until Match returns.
func Match(ctx context.Context, message *Message, metadata Metadata, criterion imap.SearchCriteria, options *MatchOptions) (bool, error) {
	if err := checkContext(ctx); err != nil {
		return false, err
	}
	if message == nil || message.parsed == nil {
		return false, fmt.Errorf("message: nil or unanalysed message")
	}
	opts := imapmessage.MatchOptions{}
	if options != nil {
		opts.Charset, opts.Now, opts.SavedUIDs = options.Charset, options.Now, options.SavedUIDs
	}
	switch strings.ToLower(strings.TrimSpace(opts.Charset)) {
	case "", "us-ascii", "ascii", "utf-8", "utf8", "iso-8859-1", "iso8859-1", "latin1", "latin-1":
	default:
		return false, &imap.Error{
			Type: imap.ErrorTypeNo, Code: imap.CodeBadCharset,
			CodeArgs: "(US-ASCII UTF-8 ISO-8859-1)", Text: "unsupported SEARCH charset",
			Err: fmt.Errorf("%w: %q", ErrUnsupportedCharset, opts.Charset),
		}
	}
	source := &operationSource{ctx: ctx, source: message.source}
	matched, err := imapmessage.MatchContext(ctx, message.parsed.WithReader(source), imapmessage.Metadata{
		SeqNum: metadata.SeqNum, UID: metadata.UID, Flags: metadata.Flags,
		InternalDate: metadata.InternalDate, SaveDate: metadata.SaveDate,
		RFC822Size: metadata.RFC822Size, ModSeq: metadata.ModSeq,
		PrivateModSeqs: metadata.PrivateModSeqs, SharedModSeqs: metadata.SharedModSeqs,
		EmailID: metadata.EmailID, ThreadID: metadata.ThreadID,
	}, criterion, &opts)
	if err := operationError(ctx, source, err); err != nil {
		return false, err
	}
	return matched, nil
}
