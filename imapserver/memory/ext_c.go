package memory

import (
	"context"
	"io"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
	servermessage "github.com/kiliant/go-imap/imapserver/message"
	"github.com/kiliant/go-imap/internal/unicodenorm"
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
			cmp := compareBySortKey(a, b, key.Key, s.session.comparator)
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

func compareBySortKey(a, b *message, key imap.SortKey, comparator string) int {
	switch key {
	case imap.SortKeyArrival:
		return a.internalDate.Compare(b.internalDate)
	case imap.SortKeySize:
		return len(a.raw) - len(b.raw)
	case imap.SortKeyDate:
		return sentDate(a).Compare(sentDate(b))
	case imap.SortKeySubject:
		return compareCollated(baseSubject(a), baseSubject(b), comparator)
	case imap.SortKeyFrom, imap.SortKeyDisplayFrom:
		return compareCollated(addressKey(a, key, true), addressKey(b, key, true), comparator)
	case imap.SortKeyTo, imap.SortKeyCc, imap.SortKeyDisplayTo:
		return compareCollated(addressKey(a, key, false), addressKey(b, key, false), comparator)
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
	if envelope := msg.analysis.Envelope(); envelope != nil && !envelope.Date.IsZero() {
		return envelope.Date
	}
	return msg.internalDate
}

// baseSubject is the RFC 5256 section 2.1 base subject of a message's decoded
// envelope subject. The session's comparator is applied after extraction.
func baseSubject(msg *message) string {
	subject := ""
	if envelope := msg.analysis.Envelope(); envelope != nil {
		subject = envelope.Subject
	}
	return subjectBase(subject)
}

// collationKey groups ORDEREDSUBJECT threads. i;unicode-casemap is the
// SORT/THREAD default required by RFC 5256 and RFC 5255; i;octet preserves
// case and every octet. Casemap keeps invalid UTF-8 as its octets, and its
// output for valid input is valid UTF-8, so a subject that fails conversion
// groups only with octet-identical subjects.
func collationKey(s, comparator string) string {
	if comparator == "i;octet" {
		return s
	}
	return unicodenorm.Casemap(s)
}

// compareCollated orders two textual sort keys. Under i;unicode-casemap,
// RFC 5255 section 4.6 compares a string that fails conversion (here, invalid
// UTF-8) with i;octet and places it after every string the comparator
// accepts. i;octet accepts every string.
func compareCollated(a, b, comparator string) int {
	if comparator == "i;octet" {
		return strings.Compare(a, b)
	}
	switch aValid, bValid := utf8.ValidString(a), utf8.ValidString(b); {
	case aValid && bValid:
		return strings.Compare(unicodenorm.Casemap(a), unicodenorm.Casemap(b))
	case aValid:
		return -1
	case bValid:
		return 1
	default:
		return strings.Compare(a, b)
	}
}

// subjectBase follows RFC 5256 section 2.1 step by step; the section requires
// exactly this procedure, because its grammar alone is ambiguous. Step (1)'s
// RFC 2047 decoding has already happened in the envelope.
func subjectBase(subject string) string {
	// (1) Tabs, continuations and runs of spaces become a single space.
	s := strings.Join(strings.FieldsFunc(subject, func(r rune) bool { return r == ' ' || r == '\t' || r == '\r' || r == '\n' }), " ")
	for {
		// (2) Remove subj-trailer = "(fwd)" / WSP, repeatedly.
		for {
			trimmed := strings.TrimRight(s, " ")
			if len(trimmed) >= 5 && strings.EqualFold(trimmed[len(trimmed)-5:], "(fwd)") {
				trimmed = trimmed[:len(trimmed)-5]
			}
			if trimmed == s {
				break
			}
			s = trimmed
		}
		// (3)–(5) Remove subj-leaders and removable subj-blobs until neither
		// matches.
		for {
			if rest, ok := cutSubjectLeader(s); ok {
				s = rest
				continue
			}
			if rest, ok := cutSubjectBlob(s); ok && strings.TrimLeft(rest, " ") != "" {
				s = rest
				continue
			}
			break
		}
		// (6) Unwrap "[fwd:" subject "]" and repeat from (2).
		if len(s) >= 6 && strings.EqualFold(s[:5], "[fwd:") && s[len(s)-1] == ']' {
			s = s[5 : len(s)-1]
			continue
		}
		return s
	}
}

// cutSubjectLeader removes one subj-leader = (*subj-blob subj-refwd) / WSP,
// where subj-refwd = ("re" / ("fw" ["d"])) *WSP [subj-blob] ":".
func cutSubjectLeader(s string) (string, bool) {
	if strings.HasPrefix(s, " ") {
		return s[1:], true
	}
	rest := s
	for {
		next, ok := cutSubjectBlob(rest)
		if !ok {
			break
		}
		rest = next
	}
	switch {
	case len(rest) >= 3 && strings.EqualFold(rest[:3], "fwd"):
		rest = rest[3:]
	case len(rest) >= 2 && (strings.EqualFold(rest[:2], "re") || strings.EqualFold(rest[:2], "fw")):
		rest = rest[2:]
	default:
		return s, false
	}
	rest = strings.TrimLeft(rest, " ")
	if next, ok := cutSubjectBlob(rest); ok {
		rest = next
	}
	if !strings.HasPrefix(rest, ":") {
		return s, false
	}
	return rest[1:], true
}

// cutSubjectBlob removes one subj-blob = "[" *BLOBCHAR "]" *WSP, where
// BLOBCHAR is any octet except "[" and "]".
func cutSubjectBlob(s string) (string, bool) {
	if !strings.HasPrefix(s, "[") {
		return s, false
	}
	end := strings.IndexAny(s[1:], "[]")
	if end < 0 || s[1+end] != ']' {
		return s, false
	}
	return strings.TrimLeft(s[end+2:], " "), true
}

// addressKey is the value the address-shaped sort keys compare, taken from the
// first address of the header. RFC 5256 section 3 sorts FROM, TO and CC on its
// addr-mailbox alone. RFC 5957 section 3 sorts the DISPLAY keys on its decoded
// addr-name, falling back to addr-mailbox@addr-host, then to addr-mailbox.
func addressKey(msg *message, key imap.SortKey, from bool) string {
	envelope := msg.analysis.Envelope()
	if envelope == nil {
		return ""
	}
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
	if key != imap.SortKeyDisplayFrom && key != imap.SortKeyDisplayTo {
		return address.Mailbox
	}
	switch {
	case address.Name != "":
		return address.Name
	case address.Mailbox != "" && address.Host != "":
		return address.Mailbox + "@" + address.Host
	default:
		return address.Mailbox
	}
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
		subject := collationKey(baseSubject(msg), s.session.comparator)
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
	needsEmailID := searchNeedsEmailID(criteria)
	for i, msg := range s.mailbox.messages {
		// The same evaluator input as Search, so SORT and THREAD select
		// exactly what SEARCH would.
		metadata := searchMetadata(i, msg, s.recentFlags(msg.uid, msg.flags), needsEmailID)
		ok, err := servermessage.Match(ctx, msg.analysis, metadata, criteria, &servermessage.MatchOptions{Charset: charset, Comparator: s.session.comparator})
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
func (s *session) ResolveCatenateURL(ctx context.Context, raw string, _ *imapserver.CatenateOptions) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.account.mu.Lock()
	defer s.account.mu.Unlock()
	if s.closed {
		return nil, noError(imap.CodeClosed, "session is closed")
	}
	selectedName := ""
	if len(s.selections) == 1 {
		for selection := range s.selections {
			selectedName = selection.mailbox.name
		}
	}
	request, ok := parseCatenateURL(raw, s.username, selectedName)
	if !ok {
		return nil, noError(imap.CodeBadURL, "invalid or unauthorized CATENATE URL")
	}
	m := s.account.mailboxes[request.mailbox]
	if m == nil || request.uidValidity != 0 && request.uidValidity != m.uidValidity {
		return nil, noError(imap.CodeBadURL, "CATENATE mailbox or UIDVALIDITY did not resolve")
	}
	for _, msg := range m.messages {
		if msg.uid == request.uid {
			reader, _, err := msg.analysis.OpenBodySection(ctx, request.section, nil)
			if err != nil {
				return nil, err
			}
			return io.NopCloser(reader), nil
		}
	}
	return nil, noError(imap.CodeBadURL, "no such message in CATENATE URL")
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
	needsEmailID := searchNeedsEmailID(criteria)
	for _, name := range mailboxes {
		m := s.account.mailboxes[mailboxKey(name)]
		if m == nil {
			continue
		}
		var matches []imap.UID
		var modSeqs []uint64
		for i, msg := range m.messages {
			metadata := searchMetadata(i, msg, cloneFlags(msg.flags), needsEmailID)
			ok, err := servermessage.Match(ctx, msg.analysis, metadata, criteria, &servermessage.MatchOptions{Charset: charset, Comparator: s.comparator})
			if err != nil {
				return nil, err
			}
			if ok {
				matches = append(matches, msg.uid)
				modSeqs = append(modSeqs, msg.modSeq)
			}
		}
		result := imapserver.MultiSearchMailboxResult{
			Mailbox:     m.name,
			UIDValidity: m.uidValidity,
			UIDs:        matches,
			ModSeqs:     modSeqs,
		}
		if options != nil && options.ReturnRelevancy {
			result.Relevancy = exactRelevancy(len(matches))
		}
		results = append(results, result)
	}
	return results, nil
}

var _ imapserver.MultiSearchSession = (*session)(nil)
