package memory

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
	"github.com/kiliant/go-imap/internal/imapmessage"
)

// Group C support: SORT and THREAD (RFC 5256).
//
// Both need message content the framework does not have — the RFC 5256 base
// subject for ordering and grouping, and the address headers for the display
// keys — which is why they are backend operations rather than framework ones.

// Sort implements [imapserver.SortMailbox].
func (s *selected) Sort(ctx context.Context, query *imapserver.SearchQuery, keys []imap.SortKeySpec, options *imapserver.SortOptions) ([]imap.UID, error) {
	charset := ""
	if options != nil {
		charset = options.Charset
	}
	matched, err := s.matching(ctx, query, charset)
	if err != nil {
		return nil, err
	}
	s.session.account.mu.Lock()
	defer s.session.account.mu.Unlock()
	// A stable sort applied from the last key backwards yields the same order
	// as comparing keys left to right, and keeps the arrival order as the
	// final tie-break, which RFC 5256 section 3 requires.
	for i := len(keys) - 1; i >= 0; i-- {
		key := keys[i]
		slices.SortStableFunc(matched, func(a, b *message) int {
			cmp := compareBySortKey(a, b, key.Key)
			if key.Reverse {
				return -cmp
			}
			return cmp
		})
	}
	uids := make([]imap.UID, len(matched))
	for i, msg := range matched {
		uids[i] = msg.uid
	}
	return uids, nil
}

func compareBySortKey(a, b *message, key imap.SortKey) int {
	switch key {
	case imap.SortKeyArrival:
		return a.internalDate.Compare(b.internalDate)
	case imap.SortKeySize:
		return len(a.raw) - len(b.raw)
	case imap.SortKeyDate:
		return sentDate(a).Compare(sentDate(b))
	case imap.SortKeySubject:
		return strings.Compare(baseSubject(a), baseSubject(b))
	case imap.SortKeyFrom, imap.SortKeyDisplayFrom:
		return strings.Compare(addressKey(a, key, true), addressKey(b, key, true))
	case imap.SortKeyTo, imap.SortKeyCc, imap.SortKeyDisplayTo:
		return strings.Compare(addressKey(a, key, false), addressKey(b, key, false))
	default:
		// An unknown key must not reorder anything. A stable sort with a
		// constant comparison leaves the sequence untouched, which is safer
		// than guessing at an ordering the client did not ask for.
		return 0
	}
}

// sentDate is the Date header, falling back to the internal date when the
// message carries none. RFC 5256 section 2.2.
func sentDate(msg *message) time.Time {
	if msg.analysis != nil && msg.analysis.Envelope != nil && !msg.analysis.Envelope.Date.IsZero() {
		return msg.analysis.Envelope.Date
	}
	return msg.internalDate
}

// baseSubject is the RFC 5256 section 2.1 base subject: the subject with
// leading "Re:"/"Fwd:" prefixes and trailing "(fwd)" removed, folded for
// comparison. Threading and subject ordering both key on it.
func baseSubject(msg *message) string {
	subject := ""
	if msg.analysis != nil && msg.analysis.Envelope != nil {
		subject = msg.analysis.Envelope.Subject
	}
	subject = strings.ToLower(strings.Join(strings.Fields(subject), " "))
	for {
		trimmed := strings.TrimSpace(subject)
		trimmed = strings.TrimSuffix(trimmed, "(fwd)")
		trimmed = strings.TrimSpace(trimmed)
		switch {
		case strings.HasPrefix(trimmed, "re:"):
			trimmed = strings.TrimPrefix(trimmed, "re:")
		case strings.HasPrefix(trimmed, "fw:"):
			trimmed = strings.TrimPrefix(trimmed, "fw:")
		case strings.HasPrefix(trimmed, "fwd:"):
			trimmed = strings.TrimPrefix(trimmed, "fwd:")
		default:
			return trimmed
		}
		subject = trimmed
	}
}

// addressKey is the mailbox or display name the address-shaped sort keys
// compare. RFC 5256 section 3 sorts on the first address of the header, and
// RFC 5957's DISPLAY keys sort on its display name instead.
func addressKey(msg *message, key imap.SortKey, from bool) string {
	if msg.analysis == nil || msg.analysis.Envelope == nil {
		return ""
	}
	envelope := msg.analysis.Envelope
	var addresses []imap.Address
	switch {
	case from:
		addresses = envelope.From
	case key == imap.SortKeyCc:
		addresses = envelope.Cc
	default:
		addresses = envelope.To
	}
	if len(addresses) == 0 {
		return ""
	}
	address := addresses[0]
	display := key == imap.SortKeyDisplayFrom || key == imap.SortKeyDisplayTo
	if display && address.Name != "" {
		return strings.ToLower(address.Name)
	}
	return strings.ToLower(address.Mailbox + "@" + address.Host)
}

// Thread implements [imapserver.ThreadMailbox].
//
// Only ORDEREDSUBJECT is implemented. REFERENCES needs the full Message-ID and
// References graph, which this backend does not retain; a client asking for it
// is told so rather than being given ORDEREDSUBJECT results under the wrong
// name, which would silently mis-thread its view.
func (s *selected) Thread(ctx context.Context, query *imapserver.SearchQuery, algorithm imap.ThreadAlgorithm, options *imapserver.ThreadOptions) ([]imap.ThreadNode, error) {
	if algorithm != imap.ThreadOrderedSubject {
		return nil, &imap.Error{
			Type: imap.ErrorTypeNo,
			Code: imap.CodeCannot,
			Text: "unsupported threading algorithm " + string(algorithm),
		}
	}
	charset := ""
	if options != nil {
		charset = options.Charset
	}
	matched, err := s.matching(ctx, query, charset)
	if err != nil {
		return nil, err
	}
	s.session.account.mu.Lock()
	defer s.session.account.mu.Unlock()

	// RFC 5256 section 4.1: messages sharing a base subject form one thread,
	// ordered by sent date, and the threads themselves are ordered by their
	// first message's sent date.
	slices.SortStableFunc(matched, func(a, b *message) int { return sentDate(a).Compare(sentDate(b)) })
	order := make([]string, 0, len(matched))
	threads := make(map[string][]*message)
	for _, msg := range matched {
		subject := baseSubject(msg)
		if _, seen := threads[subject]; !seen {
			order = append(order, subject)
		}
		threads[subject] = append(threads[subject], msg)
	}

	roots := make([]imap.ThreadNode, 0, len(order))
	for _, subject := range order {
		thread := threads[subject]
		node := imap.ThreadNode{Num: uint32(thread[0].uid)}
		// Later messages in a thread hang off the first as replies, which is
		// what ORDEREDSUBJECT means by a flat thread.
		for _, msg := range thread[1:] {
			node.Children = append(node.Children, imap.ThreadNode{Num: uint32(msg.uid)})
		}
		roots = append(roots, node)
	}
	return roots, nil
}

// matching evaluates a search query and returns the matching messages in
// mailbox order. It takes and releases the account lock itself.
func (s *selected) matching(ctx context.Context, query *imapserver.SearchQuery, charset string) ([]*message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.session.account.mu.Lock()
	defer s.session.account.mu.Unlock()
	if s.closed {
		return nil, noError(imap.CodeClosed, "mailbox is no longer selected")
	}
	criteria := query.Criteria()
	if criteria == nil {
		criteria = imap.SearchAll
	}
	var matched []*message
	for i, msg := range s.mailbox.messages {
		metadata := imapmessage.Metadata{
			SeqNum:       imap.SeqNum(i + 1),
			UID:          msg.uid,
			Flags:        cloneFlags(msg.flags),
			InternalDate: msg.internalDate,
			RFC822Size:   int64(len(msg.raw)),
		}
		ok, err := imapmessage.Match(msg.analysis, metadata, criteria, &imapmessage.MatchOptions{Charset: charset})
		if err != nil {
			return nil, err
		}
		if ok {
			matched = append(matched, msg)
		}
	}
	return matched, nil
}

var (
	_ imapserver.SortMailbox   = (*selected)(nil)
	_ imapserver.ThreadMailbox = (*selected)(nil)
)
