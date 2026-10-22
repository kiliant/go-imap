// Package memory provides a supported in-memory implementation of
// [imapserver.Backend]. It is intended for tests, examples, and ephemeral
// servers. It is not durable and must not be used for production mail storage.
//
// The package implements the mandatory rev1 contract, all current optional
// server interfaces, and PLAIN, LOGIN and SCRAM authentication with configured
// credentials. The capability witnesses in ext_a.go specify its supported
// extensions. Future optional interfaces are adopted deliberately.
//
// RENAME INBOX moves its messages to a newly created mailbox and leaves the
// empty INBOX, its identity, metadata, subscription and inferiors in place.
// A destination beneath INBOX is refused with NO [CANNOT]. COPY, MOVE and
// RENAME INBOX preserve INTERNALDATE and assign a new SAVEDATE on arrival.
//
// CATENATE resolves URLs within the authenticated account. In absolute IMAP
// URLs, hostnames and ports are logical aliases for this backend; they do not
// cause a connection to another server. Absolute URLs without userinfo inherit
// the current account; mechanism-only userinfo is refused. URLs naming another
// user or requiring URLAUTH are refused.
package memory

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
)

// Options configures a memory backend. A nil pointer selects an empty backend.
// Construct with keyed fields only; fields may be added in a future release.
type Options struct {
	// Users maps authentication usernames to plaintext test passwords. Values
	// are copied by New. The memory backend is not a credential store.
	Users map[string]string
	_     struct{}
}

// Backend is an in-memory implementation of [imapserver.Backend].
// Construct it with [New]; its zero value is not an initialized backend.
type Backend struct {
	mu       sync.RWMutex
	accounts map[string]*account
	// scramCache holds this backend's SCRAM derivations. See ext_e_scram.go
	// for why it must not be package-level.
	scramMu    sync.Mutex
	scramCache map[string]*scramDerivation
}

type account struct {
	mu        sync.Mutex
	password  string
	mailboxes map[string]*mailbox
	// subscriptions maps mailbox keys to subscribed names. They belong to the
	// name, not the mailbox: DELETE and RENAME leave them in place (RFC 3501
	// section 6.3.6, RFC 9051 sections 6.3.6 and 6.3.7).
	subscriptions   map[string]string
	nextUIDValidity uint32
	selectFailure   map[string]bool
	// Group D per-account state. An unset limit is unlimited; a configured
	// zero is distinct and is reported as zero. See ext_d.go.
	quotaStorage     uint64
	quotaStorageSet  bool
	quotaMessages    uint64
	quotaMessagesSet bool
	metadata         map[imap.MetadataEntryName]string
	// urlAuthKeys holds the per-mailbox URLAUTH secrets. See ext_e.go.
	urlAuthKeys map[string]urlAuthKey
	// sessions are the live sessions on this account, so a change in one can
	// be reported to a NOTIFY registration in another. See ext_d.go.
	sessions map[*session]struct{}
}

type mailbox struct {
	account     *account
	name        string
	uidValidity uint32
	uidNext     imap.UID
	revision    uint64
	// flags is the mailbox's persistent registry of applicable flags. Keywords
	// remain applicable after their last message reference disappears; treating
	// FLAGS as a projection of current messages makes concurrent STORE commands
	// withdraw and re-add the same keyword around FETCH responses.
	flags []imap.Flag
	// specialUse holds the RFC 6154 use attributes assigned at CREATE time.
	// See ext_a.go.
	specialUse []imap.MailboxAttr
	// highestModSeq is the CONDSTORE modification sequence of this mailbox, and
	// expunged the QRESYNC record of removals retained after the messages
	// themselves are gone. See ext_b.go.
	highestModSeq uint64
	expunged      []expungedRecord
	// acl and metadata are the group D per-mailbox state. See ext_d.go.
	acl      map[string]imap.ACLRights
	metadata map[imap.MetadataEntryName]string
	messages []*message
	watchers map[*selected]*imapserver.Updater
}

// expungedRecord remembers one removal for QRESYNC, so a client that was
// offline can be told what vanished while it was away.
type expungedRecord struct {
	uid    imap.UID
	modSeq uint64
}

// New returns an in-memory backend configured by options.
func New(options *Options) *Backend {
	b := &Backend{accounts: make(map[string]*account)}
	if options == nil {
		return b
	}
	for username, password := range options.Users {
		account := newAccount(password)
		b.accounts[username] = account
	}
	return b
}

func newAccount(password string) *account {
	a := &account{
		password:        password,
		mailboxes:       make(map[string]*mailbox),
		subscriptions:   make(map[string]string),
		nextUIDValidity: 1,
		selectFailure:   make(map[string]bool),
	}
	a.createMailboxLocked("INBOX")
	return a
}

// createMailboxLocked starts a mailbox at modification sequence 1. Zero is not
// a usable starting value: RFC 7162 reserves it to mean "this mailbox does not
// support modification sequences", which is what NOMODSEQ reports.
func (a *account) createMailboxLocked(name string) *mailbox {
	m := &mailbox{
		account:       a,
		name:          name,
		uidValidity:   a.nextUIDValidity,
		uidNext:       1,
		revision:      1,
		highestModSeq: 1,
		flags:         defaultMailboxFlags(),
		watchers:      make(map[*selected]*imapserver.Updater),
	}
	a.nextUIDValidity++
	a.mailboxes[mailboxKey(name)] = m
	return m
}

func mailboxKey(name string) string {
	if strings.EqualFold(name, "INBOX") {
		return "INBOX"
	}
	return name
}

// Authenticate authenticates a configured PLAIN, LOGIN or verified SCRAM username and returns
// one session.
func (b *Backend) Authenticate(ctx context.Context, _ *imapserver.ConnInfo, credentials *imapserver.Credentials, _ *imapserver.AuthenticateOptions) (imapserver.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if credentials == nil || credentials.Username == "" || (credentials.AuthzID != "" && credentials.AuthzID != credentials.Username) {
		return nil, authenticationError()
	}
	// SCRAM arrives already verified: the framework checked the client's proof
	// against the derivation this backend supplied, and no password was ever
	// sent. Re-checking one here would reject every SCRAM login.
	mechanism := strings.ToUpper(credentials.Mechanism)
	verified := strings.HasPrefix(mechanism, "SCRAM-")
	if !verified && mechanism != "PLAIN" && mechanism != "LOGIN" {
		return nil, authenticationError()
	}
	b.mu.RLock()
	a := b.accounts[credentials.Username]
	b.mu.RUnlock()
	if a == nil || (!verified && credentials.Password != a.password) {
		return nil, authenticationError()
	}
	s := &session{backend: b, account: a, username: credentials.Username, selections: make(map[*selected]struct{}), comparator: comparators[0]}
	a.mu.Lock()
	if a.sessions == nil {
		a.sessions = make(map[*session]struct{})
	}
	a.sessions[s] = struct{}{}
	a.mu.Unlock()
	return s, nil
}

func authenticationError() error {
	return &imap.Error{Type: imap.ErrorTypeNo, Code: imap.CodeAuthenticationFailed, Text: "authentication failed"}
}

func noError(code imap.ResponseCode, text string) error {
	return &imap.Error{Type: imap.ErrorTypeNo, Code: code, Text: text}
}

func nonexistentMailbox(name string) error {
	return noError(imap.CodeNonExistent, fmt.Sprintf("mailbox %q does not exist", name))
}

var (
	_ imapserver.Backend           = (*Backend)(nil)
	_ imapserver.CapabilitySupport = (*Backend)(nil)
)
