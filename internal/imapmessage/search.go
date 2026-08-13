package imapmessage

import (
	"bufio"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime/quotedprintable"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/kiliant/go-imap"
)

// ErrUnsupportedCriteria reports a SEARCH key this helper does not know. An
// indexed backend may still support it natively; this helper is not policy.
var ErrUnsupportedCriteria = errors.New("imapmessage: unsupported search criterion")

// Metadata is the mailbox state SEARCH needs in addition to message bytes.
type Metadata struct {
	SeqNum       imap.SeqNum
	UID          imap.UID
	Flags        []imap.Flag
	InternalDate time.Time
	SaveDate     *time.Time
	RFC822Size   int64
	ModSeq       uint64
	// PrivateModSeqs and SharedModSeqs carry per-entry modification
	// sequences for the extended MODSEQ search form.
	PrivateModSeqs map[string]uint64
	SharedModSeqs  map[string]uint64
	EmailID        string
	ThreadID       string
}

// MatchOptions supplies connection-scoped SEARCH inputs.
type MatchOptions struct {
	// Charset names the charset of string arguments. Empty means US-ASCII or
	// UTF-8 after UTF8=ACCEPT.
	Charset string
	// Now is the reference instant for OLDER/YOUNGER. Zero uses time.Now.
	Now time.Time
	// SavedUIDs is the connection's SEARCHRES "$" result.
	SavedUIDs imap.UIDSet
}

// Match evaluates one criteria tree against a message and its mailbox metadata.
func Match(message *Message, metadata Metadata, criterion imap.SearchCriteria, options *MatchOptions) (bool, error) {
	if message == nil || message.root == nil {
		return false, fmt.Errorf("imapmessage: nil message")
	}
	opts := MatchOptions{}
	if options != nil {
		opts = *options
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	return match(message, metadata, criterion, opts)
}

func match(message *Message, metadata Metadata, criterion imap.SearchCriteria, opts MatchOptions) (bool, error) {
	switch c := criterion.(type) {
	case imap.SearchAnd:
		for _, child := range c {
			ok, err := match(message, metadata, child, opts)
			if err != nil || !ok {
				return ok, err
			}
		}
		return true, nil
	case imap.SearchOr:
		left, err := match(message, metadata, c.Left, opts)
		if err != nil || left {
			return left, err
		}
		return match(message, metadata, c.Right, opts)
	case imap.SearchNot:
		ok, err := match(message, metadata, c.Criteria, opts)
		return !ok, err
	case imap.SearchFuzzy:
		// The portable helper has no corpus-specific fuzzy policy. Exact matches
		// are valid fuzzy matches and make the fallback deterministic.
		return match(message, metadata, c.Criteria, opts)
	case imap.SearchFilter:
		// A FILTER key names criteria the server stores; whoever evaluates a
		// search is expected to have substituted it already. Reaching here means
		// it was not substituted, and matching nothing would hide that behind a
		// plausible empty result. RFC 5466 section 3.
		return false, fmt.Errorf("imapmessage: unsubstituted SEARCH FILTER %q", string(c))
	case imap.SearchKeyword:
		return matchKeyword(metadata, c)
	case imap.SearchFlagKeyword:
		ok := hasFlag(metadata.Flags, c.Flag)
		return ok != c.Not, nil
	case imap.SearchHeaderField:
		needle, err := decodeSearchArgument(c.Value, opts.Charset)
		if err != nil {
			return false, err
		}
		for _, value := range message.root.headers.values(c.Field) {
			if containsFoldString(imap.DecodeHeader(value), needle) {
				return true, nil
			}
		}
		return false, nil
	case imap.SearchString:
		needle, err := decodeSearchArgument(c.Value, opts.Charset)
		if err != nil {
			return false, err
		}
		return matchString(message, c.Key, needle)
	case imap.SearchDate:
		return matchDate(message, metadata, c), nil
	case imap.SearchSize:
		switch c.Key {
		case imap.SearchSizeKeyLarger:
			return metadata.RFC822Size > c.Size, nil
		case imap.SearchSizeKeySmaller:
			return metadata.RFC822Size < c.Size, nil
		}
	case imap.SearchSeqNum:
		return c.Set.Contains(metadata.SeqNum), nil
	case imap.SearchUID:
		return c.Set.Contains(metadata.UID), nil
	case imap.SearchSavedResult:
		return opts.SavedUIDs.Contains(metadata.UID), nil
	case imap.SearchWithin:
		if c.Seconds < 0 {
			return false, fmt.Errorf("%w: negative WITHIN interval", ErrUnsupportedCriteria)
		}
		ageSeconds := opts.Now.Unix() - metadata.InternalDate.Unix()
		switch c.Key {
		case imap.SearchWithinKeyOlder:
			return ageSeconds > c.Seconds, nil
		case imap.SearchWithinKeyYounger:
			return ageSeconds < c.Seconds, nil
		}
	case imap.SearchObjectID:
		switch c.Key {
		case imap.SearchObjectIDKeyEmail:
			return metadata.EmailID == c.Value, nil
		case imap.SearchObjectIDKeyThread:
			return metadata.ThreadID == c.Value, nil
		}
	case imap.SearchModSeq:
		if c.EntryName == "" {
			return metadata.ModSeq >= c.ModSeq, nil
		}
		var modSeq uint64
		switch c.EntryType {
		case imap.SearchModSeqMetadataPrivate:
			modSeq = metadata.PrivateModSeqs[c.EntryName]
		case imap.SearchModSeqMetadataShared:
			modSeq = metadata.SharedModSeqs[c.EntryName]
		case imap.SearchModSeqMetadataAll:
			modSeq = max(metadata.PrivateModSeqs[c.EntryName], metadata.SharedModSeqs[c.EntryName])
		default:
			return false, fmt.Errorf("%w: MODSEQ metadata type %q", ErrUnsupportedCriteria, c.EntryType)
		}
		return modSeq >= c.ModSeq, nil
	}
	return false, fmt.Errorf("%w: %T", ErrUnsupportedCriteria, criterion)
}

func matchKeyword(metadata Metadata, keyword imap.SearchKeyword) (bool, error) {
	switch keyword {
	case imap.SearchAll:
		return true, nil
	case imap.SearchAnswered:
		return hasFlag(metadata.Flags, imap.FlagAnswered), nil
	case imap.SearchDeleted:
		return hasFlag(metadata.Flags, imap.FlagDeleted), nil
	case imap.SearchDraft:
		return hasFlag(metadata.Flags, imap.FlagDraft), nil
	case imap.SearchFlagged:
		return hasFlag(metadata.Flags, imap.FlagFlagged), nil
	case imap.SearchNew:
		return hasFlag(metadata.Flags, imap.FlagRecent) && !hasFlag(metadata.Flags, imap.FlagSeen), nil
	case imap.SearchOld:
		return !hasFlag(metadata.Flags, imap.FlagRecent), nil
	case imap.SearchRecent:
		return hasFlag(metadata.Flags, imap.FlagRecent), nil
	case imap.SearchSeen:
		return hasFlag(metadata.Flags, imap.FlagSeen), nil
	case imap.SearchUnanswered:
		return !hasFlag(metadata.Flags, imap.FlagAnswered), nil
	case imap.SearchUndeleted:
		return !hasFlag(metadata.Flags, imap.FlagDeleted), nil
	case imap.SearchUndraft:
		return !hasFlag(metadata.Flags, imap.FlagDraft), nil
	case imap.SearchUnflagged:
		return !hasFlag(metadata.Flags, imap.FlagFlagged), nil
	case imap.SearchUnseen:
		return !hasFlag(metadata.Flags, imap.FlagSeen), nil
	case imap.SearchSaveDateSupported:
		return metadata.SaveDate != nil, nil
	default:
		return false, fmt.Errorf("%w: keyword %q", ErrUnsupportedCriteria, keyword)
	}
}

func hasFlag(flags []imap.Flag, want imap.Flag) bool {
	for _, flag := range flags {
		if strings.EqualFold(string(flag), string(want)) {
			return true
		}
	}
	return false
}

func matchString(message *Message, key imap.SearchStringKey, needle string) (bool, error) {
	headerContains := func(name string) bool {
		for _, value := range message.root.headers.values(name) {
			if containsFoldString(imap.DecodeHeader(value), needle) {
				return true
			}
		}
		return false
	}
	switch key {
	case imap.SearchKeyBcc:
		return headerContains("Bcc"), nil
	case imap.SearchKeyCc:
		return headerContains("Cc"), nil
	case imap.SearchKeyFrom:
		return headerContains("From"), nil
	case imap.SearchKeySubject:
		return headerContains("Subject"), nil
	case imap.SearchKeyTo:
		return headerContains("To"), nil
	case imap.SearchKeyBody:
		return message.bodyContains(needle)
	case imap.SearchKeyText:
		for _, field := range message.root.headers.fields {
			if containsFoldString(field.Name, needle) || containsFoldString(imap.DecodeHeader(field.Value), needle) {
				return true, nil
			}
		}
		return message.bodyContains(needle)
	default:
		return false, fmt.Errorf("%w: string key %q", ErrUnsupportedCriteria, key)
	}
}

func matchDate(message *Message, metadata Metadata, criterion imap.SearchDate) bool {
	var value time.Time
	switch criterion.Key {
	case imap.SearchDateKeyBefore, imap.SearchDateKeyOn, imap.SearchDateKeySince:
		value = metadata.InternalDate
	case imap.SearchDateKeySentBefore, imap.SearchDateKeySentOn, imap.SearchDateKeySentSince:
		value = message.Envelope.Date
	case imap.SearchDateKeySavedBefore, imap.SearchDateKeySavedOn, imap.SearchDateKeySavedSince:
		if metadata.SaveDate == nil {
			return false
		}
		value = *metadata.SaveDate
	default:
		return false
	}
	if value.IsZero() {
		return false
	}
	cmp := compareDate(value, criterion.Date)
	switch criterion.Key {
	case imap.SearchDateKeyBefore, imap.SearchDateKeySentBefore, imap.SearchDateKeySavedBefore:
		return cmp < 0
	case imap.SearchDateKeyOn, imap.SearchDateKeySentOn, imap.SearchDateKeySavedOn:
		return cmp == 0
	case imap.SearchDateKeySince, imap.SearchDateKeySentSince, imap.SearchDateKeySavedSince:
		return cmp >= 0
	}
	return false
}

func compareDate(a, b time.Time) int {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	switch {
	case ay != by:
		if ay < by {
			return -1
		}
		return 1
	case am != bm:
		if am < bm {
			return -1
		}
		return 1
	case ad < bd:
		return -1
	case ad > bd:
		return 1
	default:
		return 0
	}
}

func decodeSearchArgument(value, charset string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(charset)) {
	case "", "us-ascii", "ascii", "utf-8", "utf8":
		if !utf8.ValidString(value) {
			return "", fmt.Errorf("SEARCH argument is not valid UTF-8")
		}
		return value, nil
	case "iso-8859-1", "iso8859-1", "latin1", "latin-1":
		runes := make([]rune, len(value))
		for i := range value {
			runes[i] = rune(value[i])
		}
		return string(runes), nil
	default:
		return "", fmt.Errorf("unsupported SEARCH charset %q", charset)
	}
}

func containsFoldString(haystack, needle string) bool {
	ok, _ := containsFoldReader(strings.NewReader(haystack), needle)
	return ok
}

func containsFoldReader(reader io.Reader, needle string) (bool, error) {
	want := []rune(strings.ToLower(needle))
	if len(want) == 0 {
		return true, nil
	}
	// KMP keeps both time and memory bounded even for adversarial repeated text.
	prefix := make([]int, len(want))
	for i, j := 1, 0; i < len(want); i++ {
		for j > 0 && want[i] != want[j] {
			j = prefix[j-1]
		}
		if want[i] == want[j] {
			j++
		}
		prefix[i] = j
	}
	br := bufio.NewReaderSize(reader, 32<<10)
	matched := 0
	for {
		r, _, err := br.ReadRune()
		if err == io.EOF {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		r = unicode.ToLower(r)
		for matched > 0 && r != want[matched] {
			matched = prefix[matched-1]
		}
		if r == want[matched] {
			matched++
			if matched == len(want) {
				return true, nil
			}
		}
	}
}

func (m *Message) bodyContains(needle string) (bool, error) {
	if needle == "" {
		return true, nil
	}
	var walk func(*part) (bool, error)
	walk = func(p *part) (bool, error) {
		if len(p.children) > 0 {
			for _, child := range p.children {
				ok, err := walk(child)
				if err != nil || ok {
					return ok, err
				}
			}
			return false, nil
		}
		if p.message != nil {
			return walk(p.message)
		}
		if !strings.EqualFold(p.typeName, "text") {
			return false, nil
		}
		reader, err := m.decodedBodyReader(p)
		if err != nil {
			return false, nil // malformed encodings are searched best-effort
		}
		ok, err := containsFoldReader(reader, needle)
		if err == nil {
			return ok, nil
		}
		// A malformed transfer encoding must not make the whole stored
		// message unsearchable. Fall back to its encoded octets.
		return containsFoldReader(io.NewSectionReader(m.r, p.headers.bodyStart, p.end-p.headers.bodyStart), needle)
	}
	return walk(m.root)
}

func (m *Message) decodedBodyReader(p *part) (io.Reader, error) {
	var reader io.Reader = io.NewSectionReader(m.r, p.headers.bodyStart, p.end-p.headers.bodyStart)
	switch strings.ToLower(strings.TrimSpace(p.encoding)) {
	case "base64":
		reader = base64.NewDecoder(base64.StdEncoding, reader)
	case "quoted-printable":
		reader = quotedprintable.NewReader(reader)
	case "", "7bit", "8bit", "binary":
	default:
		return reader, fmt.Errorf("unknown transfer encoding %q", p.encoding)
	}
	charset := strings.ToLower(p.params["charset"])
	switch charset {
	case "", "us-ascii", "ascii", "utf-8", "utf8":
		return reader, nil
	case "iso-8859-1", "iso8859-1", "latin1", "latin-1":
		return &latin1Reader{r: bufio.NewReader(reader)}, nil
	default:
		return reader, nil
	}
}

type latin1Reader struct {
	r       *bufio.Reader
	pending []byte
}

func (r *latin1Reader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	for n < len(p) {
		b, err := r.r.ReadByte()
		if err != nil {
			if n > 0 {
				return n, nil
			}
			return 0, err
		}
		var encoded [utf8.UTFMax]byte
		count := utf8.EncodeRune(encoded[:], rune(b))
		written := copy(p[n:], encoded[:count])
		n += written
		if written < count {
			r.pending = append(r.pending[:0], encoded[written:count]...)
		}
	}
	return n, nil
}
