package memory

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"mime/quotedprintable"
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

// ResolveCatenateURL implements [imapserver.CatenateSession].
//
// CATENATE URLs name a message this account can read. They are resolved through
// the same path URLFETCH uses, minus the authorization token: the client is
// already authenticated as the owner, so RFC 4469 section 3 requires only that
// the URL refer to something it may read — which here means its own mailboxes.
func (s *session) ResolveCatenateURL(ctx context.Context, url string, _ *imapserver.CatenateOptions) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.account.mu.Lock()
	defer s.account.mu.Unlock()
	if s.closed {
		return nil, noError(imap.CodeClosed, "session is closed")
	}
	m := s.account.mailboxes[mailboxKey(urlMailbox(url))]
	if m == nil {
		return nil, noError(imap.CodeNonExistent, "no such mailbox in CATENATE URL")
	}
	uid := urlUID(url)
	for _, msg := range m.messages {
		if msg.uid == uid {
			return io.NopCloser(bytes.NewReader(append([]byte(nil), msg.raw...))), nil
		}
	}
	return nil, noError(imap.CodeNonExistent, "no such message in CATENATE URL")
}

var (
	_ imapserver.CatenateSession = (*session)(nil)
	_ imapserver.SortMailbox     = (*selected)(nil)
	_ imapserver.ThreadMailbox   = (*selected)(nil)
)

// MultiSearch implements [imapserver.MultiSearchSession].
//
// Each named mailbox is evaluated independently and reported with its own
// UIDVALIDITY, because a UID from one mailbox means nothing in another.
//
// A mailbox that does not exist is skipped rather than failing the command: RFC
// 7377 section 2.1 reports per-mailbox results, and one bad name in a list
// should not discard the results for every other mailbox the client asked about.
func (s *session) MultiSearch(ctx context.Context, mailboxes []string, criteria imap.SearchCriteria, options *imapserver.MultiSearchOptions) ([]imapserver.MultiSearchMailboxResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	charset := ""
	if options != nil {
		charset = options.Charset
	}
	if criteria == nil {
		criteria = imap.SearchAll
	}
	s.account.mu.Lock()
	defer s.account.mu.Unlock()
	if s.closed {
		return nil, noError(imap.CodeClosed, "session is closed")
	}
	results := make([]imapserver.MultiSearchMailboxResult, 0, len(mailboxes))
	for _, name := range mailboxes {
		m := s.account.mailboxes[mailboxKey(name)]
		if m == nil {
			continue
		}
		var matches []imap.UID
		for i, msg := range m.messages {
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
				matches = append(matches, msg.uid)
			}
		}
		results = append(results, imapserver.MultiSearchMailboxResult{
			Mailbox:     m.name,
			UIDValidity: m.uidValidity,
			UIDs:        matches,
		})
	}
	return results, nil
}

var _ imapserver.MultiSearchSession = (*session)(nil)

// openBinarySection answers a BINARY[] fetch: the part's bytes with its
// content-transfer-encoding undone.
//
// The decoding is done here rather than in internal/imapmessage because it is
// backend work by the design's own split — the framework has no access to the
// message — and because imapmessage belongs to another task. A backend with a
// store that already keeps decoded parts would answer this without decoding
// anything.
//
// RFC 3516 section 4.3 requires an UNKNOWN-CTE failure rather than a guess when
// the encoding cannot be undone; returning the raw bytes would hand the client
// base64 it believes is binary.
func openBinarySection(msg *message, part []int) (io.Reader, error) {
	raw, _, err := msg.analysis.OpenBodySection(&imap.FetchItemBodySection{Part: part, Peek: true})
	if err != nil {
		return nil, err
	}
	switch strings.ToLower(partEncoding(msg.analysis.BodyStructure, part)) {
	case "", "7bit", "8bit", "binary":
		return raw, nil
	case "base64":
		return base64.NewDecoder(base64.StdEncoding, raw), nil
	case "quoted-printable":
		return quotedprintable.NewReader(raw), nil
	default:
		return nil, &imap.Error{
			Type: imap.ErrorTypeNo,
			Code: imap.CodeUnknownCTE,
			Text: "cannot decode this content-transfer-encoding",
		}
	}
}

// partEncoding walks the body structure to the named part and reports its
// content-transfer-encoding. An empty path means the whole message.
func partEncoding(structure imap.BodyStructure, part []int) string {
	current := structure
	for _, index := range part {
		multipart, ok := current.(*imap.BodyStructureMultiPart)
		if !ok || index < 1 || index > len(multipart.Children) {
			// A path into a non-multipart is the message itself, which the
			// single-part branch below then reports on.
			break
		}
		current = multipart.Children[index-1]
	}
	if single, ok := current.(*imap.BodyStructureSinglePart); ok {
		return single.Encoding
	}
	return ""
}
