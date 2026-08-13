package memory

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"strings"

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

// urlAuthKey returns the per-mailbox secret behind its URLAUTH tokens, creating
// one on first use.
//
// The caller must hold the account lock.
func (a *account) urlAuthKeyLocked(mailbox string) ([]byte, error) {
	if a.urlAuthKeys == nil {
		a.urlAuthKeys = make(map[string][]byte)
	}
	if key, ok := a.urlAuthKeys[mailbox]; ok {
		return key, nil
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	a.urlAuthKeys[mailbox] = key
	return key, nil
}

// GenerateURLAuth implements [imapserver.URLAuthSession].
//
// The token is an HMAC over the URL under the mailbox's secret, which gives the
// two properties RFC 4467 section 3 needs: it cannot be forged without the
// secret, and RESETKEY invalidates every token for that mailbox at once by
// discarding the secret.
func (s *session) GenerateURLAuth(ctx context.Context, url, mechanism string, _ *imapserver.URLAuthOptions) (string, error) {
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
	mailbox := urlMailbox(url)
	s.account.mu.Lock()
	defer s.account.mu.Unlock()
	key, err := s.account.urlAuthKeyLocked(mailbox)
	if err != nil {
		return "", err
	}
	return url + ":internal:" + urlAuthToken(key, url), nil
}

// FetchURLAuth implements [imapserver.URLAuthSession].
//
// The token is verified with a constant-time comparison before anything is
// returned. A variable-time comparison here would leak the token a byte at a
// time to anyone allowed to present a guess, which is the whole attack URLAUTH
// has to withstand.
func (s *session) FetchURLAuth(ctx context.Context, url string, _ *imapserver.URLAuthOptions) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	base, token, found := strings.Cut(url, ":internal:")
	if !found {
		return nil, nil
	}
	mailbox := urlMailbox(base)
	s.account.mu.Lock()
	defer s.account.mu.Unlock()
	key, ok := s.account.urlAuthKeys[mailbox]
	if !ok {
		return nil, nil
	}
	if !hmac.Equal([]byte(token), []byte(urlAuthToken(key, base))) {
		return nil, nil
	}
	m := s.account.mailboxes[mailboxKey(mailbox)]
	if m == nil {
		return nil, nil
	}
	uid := urlUID(base)
	for _, msg := range m.messages {
		if msg.uid == uid {
			return io.NopCloser(bytes.NewReader(append([]byte(nil), msg.raw...))), nil
		}
	}
	return nil, nil
}

// ResetURLAuthKey implements [imapserver.URLAuthSession]. An empty mailbox name
// revokes every URL in the account, per RFC 4467 section 5.1.
func (s *session) ResetURLAuthKey(ctx context.Context, mailbox string, _ *imapserver.URLAuthOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.account.mu.Lock()
	defer s.account.mu.Unlock()
	if mailbox == "" {
		s.account.urlAuthKeys = nil
		return nil
	}
	delete(s.account.urlAuthKeys, mailbox)
	return nil
}

func urlAuthToken(key []byte, url string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(url))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// urlMailbox and urlUID pull the mailbox and UID out of an IMAP URL of the form
// "imap://user@host/MAILBOX/;UID=n". This is a deliberately small parser: a
// reference backend needs enough to demonstrate the token mechanism, and a
// complete RFC 5092 parser belongs in a URL package rather than here.
func urlMailbox(url string) string {
	rest, found := strings.CutPrefix(url, "imap://")
	if !found {
		return ""
	}
	_, path, found := strings.Cut(rest, "/")
	if !found {
		return ""
	}
	mailbox, _, _ := strings.Cut(path, "/;")
	return mailbox
}

func urlUID(url string) imap.UID {
	_, rest, found := strings.Cut(url, "/;UID=")
	if !found {
		return 0
	}
	value, _, _ := strings.Cut(rest, "/")
	var uid uint32
	if _, err := fmt.Sscanf(value, "%d", &uid); err != nil {
		return 0
	}
	return imap.UID(uid)
}

// comparators are the collations this backend can apply. Only the RFC 5255
// default is real; the ordinal comparator is offered because a client that asks
// for exact octet comparison can genuinely be served it.
var comparators = []string{"i;unicode-casemap", "i;octet"}

// Comparators implements [imapserver.ComparatorSession].
func (s *session) Comparators(ctx context.Context, _ *imapserver.ComparatorOptions) (string, []string, error) {
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	s.account.mu.Lock()
	defer s.account.mu.Unlock()
	active := s.comparator
	if active == "" {
		active = comparators[0]
	}
	return active, append([]string(nil), comparators...), nil
}

// SetComparator implements [imapserver.ComparatorSession]. It takes the first
// name it can serve, and reports empty when it can serve none — which the
// framework turns into BADCOMPARATOR rather than a generic failure.
func (s *session) SetComparator(ctx context.Context, order []string, _ *imapserver.ComparatorOptions) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	s.account.mu.Lock()
	defer s.account.mu.Unlock()
	for _, requested := range order {
		for _, available := range comparators {
			if strings.EqualFold(requested, available) {
				s.comparator = available
				return available, nil
			}
		}
	}
	return "", nil
}

// filters are the saved searches this backend serves. A real server would store
// them per account; a fixed set is enough to exercise the substitution, which is
// the part the framework owns.
var filters = map[string]imap.SearchCriteria{
	"unseen":  imap.SearchNot{Criteria: imap.SearchFlagKeyword{Flag: imap.FlagSeen}},
	"flagged": imap.SearchFlagKeyword{Flag: imap.FlagFlagged},
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
