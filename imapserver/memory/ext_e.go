package memory

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
)

// Group E support: LANGUAGE (RFC 5255) and URLAUTH (RFC 4467).
//
// URLAUTH is the interesting one to implement even in a reference backend,
// because it is the only extension here whose correctness is a security
// property rather than a formatting question: a forged token must not grant
// access.

// languages are the tags this backend claims. Only "en" is real — the backend
// has no message catalogue — but the negotiation shape is what a client
// exercises, and answering honestly with one language is better than claiming
// several and serving English for all of them.
var languages = []string{"en"}

// Languages implements [imapserver.LanguageSession].
func (s *session) Languages(ctx context.Context, _ *imapserver.LanguageOptions) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return append([]string(nil), languages...), nil
}

// SetLanguage implements [imapserver.LanguageSession].
//
// RFC 4647 matches language tags case-insensitively and by prefix, so "en-GB"
// selects "en". Returning the tag actually adopted rather than the one asked
// for is what lets the client know which it got.
func (s *session) SetLanguage(ctx context.Context, tag string, _ *imapserver.LanguageOptions) (*imapserver.LanguageResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	requested := strings.ToLower(strings.TrimSpace(tag))
	for _, available := range languages {
		if requested == available || strings.HasPrefix(requested, available+"-") {
			return &imapserver.LanguageResult{Tag: available}, nil
		}
	}
	return nil, nil
}

// Keys bind to mailbox identity as well as its name, so recreating a mailbox
// or changing UIDVALIDITY never revives an old authorization token.
type urlAuthKey struct {
	secret      []byte
	uidValidity uint32
}

// The caller must hold the account lock and have validated the mailbox.
func (a *account) urlAuthKeyLocked(mailbox string) (urlAuthKey, error) {
	if a.urlAuthKeys == nil {
		a.urlAuthKeys = make(map[string]urlAuthKey)
	}
	validity := a.mailboxes[mailbox].uidValidity
	if key, ok := a.urlAuthKeys[mailbox]; ok && key.uidValidity == validity {
		return key, nil
	}
	key := urlAuthKey{secret: make([]byte, 32), uidValidity: validity}
	if _, err := rand.Read(key.secret); err != nil {
		return urlAuthKey{}, err
	}
	a.urlAuthKeys[mailbox] = key
	return key, nil
}

// GenerateURLAuth implements [imapserver.URLAuthSession].
//
// The token is an HMAC over the URL under the mailbox's secret, which gives the
// two properties RFC 4467 section 3 needs: it cannot be forged without the
// secret, and RESETKEY invalidates every token for that mailbox at once by
// discarding the secret. Rump URLs must identify this account as key owner,
// an existing mailbox, a message UID and a supported access identifier. Expired
// URLs and malformed arguments fail with BAD before any key is created.
// user+ identities must be configured accounts; submit+ URLs can be generated
// but this backend grants no submission role, so no session can resolve them.
// URL authorities are logical names within this Backend; no network fetch is
// performed. Exact supplied bytes, not a normalized URL, are authenticated.
func (s *session) GenerateURLAuth(ctx context.Context, url, mechanism string, _ *imapserver.GenURLAuthOptions) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !strings.EqualFold(mechanism, "INTERNAL") {
		return "", &imap.Error{
			Type: imap.ErrorTypeNo,
			Code: imap.CodeCannot,
			Text: "unsupported URLAUTH mechanism " + mechanism,
		}
	}
	request, ok := parseURLAuthURL(url, false, time.Now())
	if !ok || request.owner != s.username {
		return "", invalidURLAuthGeneration()
	}
	if request.access == "user" && s.urlAuthAccount(request.identity) == nil {
		return "", invalidURLAuthGeneration()
	}
	s.account.mu.Lock()
	defer s.account.mu.Unlock()
	if s.closed {
		return "", noError(imap.CodeClosed, "session is closed")
	}
	m := s.account.mailboxes[request.mailbox]
	if m == nil || request.uidValidity != 0 && request.uidValidity != m.uidValidity {
		return "", invalidURLAuthGeneration()
	}
	key, err := s.account.urlAuthKeyLocked(request.mailbox)
	if err != nil {
		return "", err
	}
	return url + ":internal:" + urlAuthToken(key.secret, url), nil
}

// FetchURLAuth implements [imapserver.URLAuthSession].
//
// The token is verified with a constant-time comparison before anything is
// returned. A variable-time comparison here would leak the token a byte at a
// time to anyone allowed to present a guess, which is the whole attack URLAUTH
// has to withstand. Resolution uses the URL owner's account, then enforces
// the session access restriction, expiry and UIDVALIDITY. Invalid, expired,
// unauthorized or unresolvable URLs return nil. Known-size readers retain the
// caller's context and do not set Seen. Sections and URL-PARTIAL ranges use the
// same stored-byte semantics as BODY sections.
func (s *session) FetchURLAuth(ctx context.Context, url string, _ *imapserver.URLFetchOptions) (*imapserver.URLFetchResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.account.mu.Lock()
	closed := s.closed
	s.account.mu.Unlock()
	if closed {
		return nil, noError(imap.CodeClosed, "session is closed")
	}
	request, ok := parseURLAuthURL(url, true, time.Now())
	if !ok || request.access == "user" && request.identity != s.username || request.access == "submit" {
		// This backend grants no message-submission role. A submit+ token
		// cannot become usable merely because its userid matches the session.
		return nil, nil
	}
	owner := s.urlAuthAccount(request.owner)
	if owner == nil {
		return nil, nil
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	key, ok := owner.urlAuthKeys[request.mailbox]
	m := owner.mailboxes[request.mailbox]
	if !ok || m == nil || key.uidValidity != m.uidValidity || request.uidValidity != 0 && request.uidValidity != m.uidValidity {
		return nil, nil
	}
	expected, _ := hex.DecodeString(urlAuthToken(key.secret, request.rump))
	if !hmac.Equal(request.token, expected) {
		return nil, nil
	}
	for _, msg := range m.messages {
		if msg.uid == request.uid {
			reader, size, err := msg.analysis.OpenBodySection(ctx, request.section, nil)
			if err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				return nil, nil
			}
			return &imapserver.URLFetchResult{Body: io.NopCloser(reader), Size: size, HasSize: true}, nil
		}
	}
	return nil, nil
}

func invalidURLAuthGeneration() error {
	return &imap.Error{Type: imap.ErrorTypeBad, Text: "invalid or unauthorized GENURLAUTH URL"}
}

func (s *session) urlAuthAccount(username string) *account {
	if s.backend == nil {
		if username == s.username {
			return s.account
		}
		return nil
	}
	s.backend.mu.RLock()
	defer s.backend.mu.RUnlock()
	return s.backend.accounts[username]
}

// ResetURLAuthKey implements [imapserver.URLAuthSession]. An empty mailbox name
// revokes every URL in the account, per RFC 4467 section 5.1.
func (s *session) ResetURLAuthKey(ctx context.Context, mailbox string, options *imapserver.ResetURLAuthKeyOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if options != nil {
		for _, mechanism := range options.Mechanisms {
			if mailbox == "" || !strings.EqualFold(mechanism, "INTERNAL") {
				return noError(imap.CodeCannot, "unsupported RESETKEY mechanism")
			}
		}
	}
	s.account.mu.Lock()
	defer s.account.mu.Unlock()
	if s.closed {
		return noError(imap.CodeClosed, "session is closed")
	}
	if mailbox == "" {
		s.account.urlAuthKeys = nil
		return nil
	}
	delete(s.account.urlAuthKeys, mailboxKey(mailbox))
	return nil
}

func urlAuthToken(key []byte, url string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(url))
	return hex.EncodeToString(mac.Sum(nil))
}

// comparators apply to SEARCH, SORT and ORDEREDSUBJECT (RFC 5255).
var comparators = []string{"i;unicode-casemap", "i;octet"}

// Comparators implements [imapserver.ComparatorSession].
func (s *session) Comparators(ctx context.Context, _ *imapserver.ComparatorOptions) (*imapserver.ComparatorData, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.account.mu.Lock()
	defer s.account.mu.Unlock()
	active := s.comparator
	if active == "" {
		active = comparators[0]
	}
	return &imapserver.ComparatorData{
		Active:    active,
		Available: append([]string(nil), comparators...),
	}, nil
}

// SetComparator implements [imapserver.ComparatorSession]. It takes the first
// name it can serve, and reports nil when it can serve none — which the
// framework turns into BADCOMPARATOR rather than a generic failure.
func (s *session) SetComparator(ctx context.Context, order []string, _ *imapserver.ComparatorOptions) (*imapserver.ComparatorResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.account.mu.Lock()
	defer s.account.mu.Unlock()
	for _, requested := range order {
		for _, available := range comparators {
			if strings.EqualFold(requested, available) {
				s.comparator = available
				return &imapserver.ComparatorResult{Active: available}, nil
			}
		}
	}
	return nil, nil
}

// filters are the saved searches this backend serves. A real server would store
// them per account; a fixed set is enough to exercise the substitution, which is
// the part the framework owns.
var filters = map[string]imap.SearchCriteria{
	"unseen":  imap.SearchNot{Criteria: imap.SearchFlagKeyword{Flag: imap.FlagSeen}},
	"flagged": imap.SearchFlagKeyword{Flag: imap.FlagFlagged},
	// A filter is stored criteria, so nothing stops it holding a sequence
	// number. It exists to be composed with the framework's normalisation: the
	// number has to survive substitution and then be resolved or refused like
	// any other, rather than slipping past because it arrived by expansion
	// instead of off the wire.
	"firstmessage": imap.SearchSeqNum{Set: imap.SeqSetNum(1)},
}

// Filter implements [imapserver.FilterSession]. A name it does not know returns
// nil, which the framework turns into UNDEFINED-FILTER.
func (s *session) Filter(ctx context.Context, name string, _ *imapserver.FilterOptions) (imap.SearchCriteria, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	criteria, ok := filters[strings.ToLower(name)]
	if !ok {
		return nil, nil
	}
	return criteria, nil
}

var (
	_ imapserver.FilterSession     = (*session)(nil)
	_ imapserver.ComparatorSession = (*session)(nil)
	_ imapserver.LanguageSession   = (*session)(nil)
	_ imapserver.URLAuthSession    = (*session)(nil)
)
