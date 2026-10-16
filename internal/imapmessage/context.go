package imapmessage

import (
	"context"
	"fmt"
	"io"

	"github.com/kiliant/go-imap"
)

// WithReader returns an independent view over the same immutable analysis.
// It lets the public helper bind each operation to its own cancellation-aware
// source without retaining the context used for the initial analysis.
func (m *Message) WithReader(r io.ReaderAt) *Message {
	copy := *m
	copy.r = r
	return &copy
}

// MatchContext validates every criterion before evaluating the tree. In
// particular, short-circuiting must not hide a future key this helper cannot
// evaluate. The public facade translates ErrUnsupportedCriteria independently.
func MatchContext(ctx context.Context, message *Message, metadata Metadata, criterion imap.SearchCriteria, options *MatchOptions) (bool, error) {
	opts := MatchOptions{}
	if options != nil {
		opts = *options
	}
	remaining := 10_000
	if err := validateCriteria(ctx, criterion, opts.Charset, 0, &remaining); err != nil {
		return false, err
	}
	opts.ctx = ctx
	return Match(message, metadata, criterion, &opts)
}

func validateCriteria(ctx context.Context, criterion imap.SearchCriteria, charset string, depth int, remaining *int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	*remaining -= 1
	if depth > 100 || *remaining < 0 {
		return fmt.Errorf("%w: criteria exceed depth or node limit", ErrUnsupportedCriteria)
	}
	child := func(c imap.SearchCriteria) error { return validateCriteria(ctx, c, charset, depth+1, remaining) }
	switch c := criterion.(type) {
	case imap.SearchAnd:
		for _, c := range c {
			if err := child(c); err != nil {
				return err
			}
		}
		return nil
	case imap.SearchOr:
		if err := child(c.Left); err != nil {
			return err
		}
		return child(c.Right)
	case imap.SearchNot:
		return child(c.Criteria)
	case imap.SearchFuzzy:
		return child(c.Criteria)
	case imap.SearchKeyword:
		_, err := matchKeyword(Metadata{}, c)
		return err
	case imap.SearchFlagKeyword, imap.SearchSeqNum, imap.SearchUID, imap.SearchSavedResult:
		return nil
	case imap.SearchHeaderField:
		_, err := decodeSearchArgument(c.Value, charset)
		return err
	case imap.SearchString:
		switch c.Key {
		case imap.SearchKeyBcc, imap.SearchKeyBody, imap.SearchKeyCc, imap.SearchKeyFrom, imap.SearchKeySubject, imap.SearchKeyText, imap.SearchKeyTo:
			_, err := decodeSearchArgument(c.Value, charset)
			return err
		}
	case imap.SearchDate:
		switch c.Key {
		case imap.SearchDateKeyBefore, imap.SearchDateKeyOn, imap.SearchDateKeySince,
			imap.SearchDateKeySentBefore, imap.SearchDateKeySentOn, imap.SearchDateKeySentSince,
			imap.SearchDateKeySavedBefore, imap.SearchDateKeySavedOn, imap.SearchDateKeySavedSince:
			return nil
		}
	case imap.SearchSize:
		if c.Key == imap.SearchSizeKeyLarger || c.Key == imap.SearchSizeKeySmaller {
			return nil
		}
	case imap.SearchWithin:
		if c.Seconds >= 0 && (c.Key == imap.SearchWithinKeyOlder || c.Key == imap.SearchWithinKeyYounger) {
			return nil
		}
	case imap.SearchObjectID:
		if c.Key == imap.SearchObjectIDKeyEmail || c.Key == imap.SearchObjectIDKeyThread {
			return nil
		}
	case imap.SearchModSeq:
		if c.EntryName == "" || c.EntryType == imap.SearchModSeqMetadataAll || c.EntryType == imap.SearchModSeqMetadataPrivate || c.EntryType == imap.SearchModSeqMetadataShared {
			return nil
		}
	}
	return fmt.Errorf("%w: %T (%v)", ErrUnsupportedCriteria, criterion, criterion)
}
