package maildir

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
)

// AccountOptions configures one account. Configuration is copied by Open.
// Passwords are never stored in the Maildir or included in diagnostics.
// Construct with keyed fields only; fields may be added in a future release.
type AccountOptions struct {
	// Username is the exact, case-sensitive authentication identity.
	Username string
	// Password enables PLAIN and LOGIN for this account. Empty means the
	// account has no password credential: it never matches, not even an
	// empty password. Open rejects an account with no credential, and
	// Password is currently the only credential kind.
	Password string
	_        struct{}
}

// Options configures Open. Nil means no accounts and the default poll interval.
// Construct with keyed fields only; fields may be added in a future release.
type Options struct {
	Accounts []AccountOptions
	// PollInterval defaults to 100 ms. Nonzero values must be in [1 ms, 1 min].
	PollInterval time.Duration
	// MaxMetadataValueBytes defaults to 65536 octets. Nonzero values must
	// be at least 1024, the RFC 5464 minimum.
	MaxMetadataValueBytes uint32
	// MaxMetadataEntries defaults to 1024 per mailbox or server scope.
	// Nonzero values must be at least 10, the RFC 5464 minimum.
	MaxMetadataEntries int
	// MaxTombstones defaults to 10000 per mailbox. Older expunge history
	// is replaced with a durable checkpoint; older QRESYNC requests receive
	// a conservative set of missing UIDs. Negative values are invalid.
	MaxTombstones int
	// IsolateAccountErrors keeps healthy accounts available when another
	// account cannot be opened or recovered. Failed accounts reject login
	// until repaired and reopened; PollFailures reports their causes. The
	// default fails Open instead. Root registry/locking errors always fail Open.
	IsolateAccountErrors bool
	// OnPollFailure receives account/mailbox diagnostics outside backend
	// locks, with the poller's cancellation context. Calls are serialized.
	// It must return promptly. Close cancels the context and waits for
	// callbacks; do not call Close from one.
	OnPollFailure func(context.Context, *PollFailure)
	_             struct{}
}

// PollFailure describes a current account opening, recovery or delivery failure.
// Mailbox is empty for account-wide failures. Err is immutable; callers may
// retain the copied diagnostic. Construct with keyed fields only.
type PollFailure struct {
	Username string
	Mailbox  string
	Err      error
	_        struct{}
}

// CloseOptions configures Close. Nil selects the defaults.
// Construct with keyed fields only; fields may be added in a future release.
type CloseOptions struct{ _ struct{} }

// Backend is an account-isolated, concurrent Maildir backend. Construct it with
// Open; the zero value is not open. One process may own a root at a time.
type Backend struct {
	mu                    sync.Mutex
	accounts              map[string]*account
	root                  *os.Root
	lock                  *os.File
	closing               bool
	done                  chan struct{}
	pollDone              chan struct{}
	cancel                context.CancelFunc
	ops                   sync.WaitGroup
	closeErr              error
	onPollFailure         func(context.Context, *PollFailure)
	pollFailures          map[string]PollFailure
	maxMetadataValueBytes uint32
	maxMetadataEntries    int
	maxTombstones         int
	// callbackMu serialises OnPollFailure across the per-account pollers.
	callbackMu sync.Mutex
	// Tests inject interruption at durable commit boundaries, never through
	// the public API or a production environment variable.
	faultHook func(string) error
}

type account struct {
	backend                      *Backend
	root                         *os.Root
	password                     string
	username                     string
	gate                         chan struct{}
	state                        *accountState
	identity                     identityState
	checkpointBytes, changeBytes int64
	changeCount                  int
	scanCache                    map[string]directorySnapshot
	existing                     bool
	watchers                     map[uint32]map[*selected]*imapserver.Updater
	failure                      error
	// deliveryError records skipped new/ entries under the account gate.
	deliveryError error
}

// Open opens or creates root, locks it, recovers pending transactions, and
// starts polling external delivery. Each account lives in the lowercase SHA256
// hex directory of its exact username bytes; this is a persistent format.
// Empty/duplicate usernames and unsupported poll intervals are rejected.
// ctx controls construction only; cancellation after success does not stop
// polling. Failure releases every acquired root/file/lock.
func Open(ctx context.Context, root string, options *Options) (*Backend, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	opts := Options{PollInterval: 100 * time.Millisecond}
	if options != nil {
		opts = *options
		opts.Accounts = append([]AccountOptions(nil), options.Accounts...)
		if opts.PollInterval == 0 {
			opts.PollInterval = 100 * time.Millisecond
		}
	}
	if opts.PollInterval < time.Millisecond || opts.PollInterval > time.Minute {
		return nil, fmt.Errorf("maildir: poll interval must be between 1 ms and 1 min")
	}
	if opts.MaxMetadataValueBytes == 0 {
		opts.MaxMetadataValueBytes = 65536
	}
	if opts.MaxMetadataEntries == 0 {
		opts.MaxMetadataEntries = 1024
	}
	if opts.MaxTombstones == 0 {
		opts.MaxTombstones = 10000
	}
	if opts.MaxMetadataValueBytes < 1024 || opts.MaxMetadataEntries < 10 || opts.MaxTombstones < 0 {
		return nil, fmt.Errorf("maildir: metadata limits must allow at least 1024 octets and 10 entries; tombstone limit must be nonnegative")
	}
	seen := make(map[string]bool)
	for _, config := range opts.Accounts {
		if config.Username == "" || !utf8.ValidString(config.Username) || strings.ContainsAny(config.Username, "\x00\r\n") || seen[config.Username] {
			return nil, fmt.Errorf("maildir: empty, invalid or duplicate account identity")
		}
		if config.Password == "" {
			return nil, fmt.Errorf("maildir: account has no credential")
		}
		seen[config.Username] = true
	}
	if root == "" {
		return nil, fmt.Errorf("maildir: root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("maildir: root must be a directory, not a symlink")
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	r, err := os.OpenRoot(abs)
	if err != nil {
		return nil, err
	}
	b := &Backend{root: r, accounts: make(map[string]*account), done: make(chan struct{}), pollDone: make(chan struct{})}
	b.onPollFailure, b.pollFailures = opts.OnPollFailure, make(map[string]PollFailure)
	b.maxMetadataValueBytes, b.maxMetadataEntries, b.maxTombstones = opts.MaxMetadataValueBytes, opts.MaxMetadataEntries, opts.MaxTombstones
	opened := false
	defer func() {
		if !opened {
			for _, a := range b.accounts {
				if a.root != nil {
					a.root.Close()
				}
			}
			if b.lock != nil {
				b.lock.Close()
			}
			r.Close()
		}
	}()
	if err := checkedPath(r, ".imap-lock"); err != nil {
		return nil, err
	}
	b.lock, err = r.OpenFile(".imap-lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := lockFile(b.lock); err != nil {
		return nil, fmt.Errorf("maildir: root is already locked or locking is unavailable: %w", err)
	}
	if err := cleanupAtomicFiles(r, ".imap-root.json"); err != nil {
		return nil, err
	}
	registry, err := loadRootRegistry(r)
	if err != nil {
		return nil, err
	}
	for _, config := range opts.Accounts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		dir := fmt.Sprintf("%x", sha256.Sum256([]byte(config.Username)))
		a := &account{backend: b, username: config.Username, password: config.Password, gate: make(chan struct{}, 1), watchers: make(map[uint32]map[*selected]*imapserver.Updater)}
		a.existing = registry.Accounts[dir]
		b.accounts[config.Username] = a
		err := func() error {
			if a.existing {
				if _, err := r.Lstat(dir); err != nil {
					return fmt.Errorf("maildir: registered account directory is missing: %w", err)
				}
			}
			if err := mkdirAll(r, dir); err != nil {
				return err
			}
			ar, err := r.OpenRoot(dir)
			if err != nil {
				return err
			}
			a.root = ar
			return a.load()
		}()
		if err != nil {
			if !opts.IsolateAccountErrors {
				return nil, fmt.Errorf("maildir: opening account storage: %w", err)
			}
			a.failure = err
			b.recordPollFailure(a, err)
			continue
		}
		if !registry.Accounts[dir] {
			registry.Accounts[dir] = true
			if err := saveRootRegistry(r, registry); err != nil {
				return nil, err
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	pollCtx, cancel := context.WithCancel(context.Background())
	b.cancel = cancel
	opened = true
	go b.poll(pollCtx, opts.PollInterval)
	return b, nil
}

// Close initiates shutdown and waits for polling, callbacks and in-flight
// operations, then releases the root lock. It does not wait for idle sessions;
// their Close/Unselect remain safe. A pre-canceled context starts nothing.
// Once initiated, shutdown continues independently; each context bounds only
// that caller's wait. Repeated/concurrent calls wait for the same shutdown.
func (b *Backend) Close(ctx context.Context, options *CloseOptions) error {
	_ = options
	if err := checkContext(ctx); err != nil {
		return err
	}
	if b == nil || b.done == nil {
		return nil
	}
	b.mu.Lock()
	if !b.closing {
		b.closing = true
		b.cancel()
		go func() {
			<-b.pollDone
			b.ops.Wait()
			var closeErr error
			for _, a := range b.accounts {
				if a.root == nil {
					continue
				}
				if err := a.root.Close(); closeErr == nil {
					closeErr = err
				}
			}
			if err := b.lock.Close(); closeErr == nil {
				closeErr = err
			}
			if err := b.root.Close(); closeErr == nil {
				closeErr = err
			}
			b.mu.Lock()
			b.closeErr = closeErr
			b.mu.Unlock()
			close(b.done)
		}()
	}
	b.mu.Unlock()
	select {
	case <-b.done:
		b.mu.Lock()
		defer b.mu.Unlock()
		return b.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

// PollFailures returns current failures, sorted by username, as independent
// copies. A successful poll clears that account's entry. Failed Open accounts
// remain listed until repaired and reopened.
func (b *Backend) PollFailures() []PollFailure {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	failures := make([]PollFailure, 0, len(b.pollFailures))
	for _, failure := range b.pollFailures {
		failures = append(failures, failure)
	}
	slices.SortFunc(failures, func(a, b PollFailure) int { return strings.Compare(a.Username, b.Username) })
	return failures
}

func (b *Backend) begin(ctx context.Context) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	if b == nil || b.done == nil {
		return noError(imap.CodeClosed, "backend is closed")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closing {
		return noError(imap.CodeClosed, "backend is closing")
	}
	b.ops.Add(1)
	return nil
}

func (a *account) acquire(ctx context.Context) error {
	select {
	case a.gate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			a.release()
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (a *account) release() { <-a.gate }
func (a *account) enter(ctx context.Context) error {
	if err := a.backend.begin(ctx); err != nil {
		return err
	}
	if err := a.acquire(ctx); err != nil {
		a.backend.ops.Done()
		return err
	}
	if a.failure != nil {
		a.leave()
		return noError(imap.CodeUnavailable, "account requires recovery; reopen the backend")
	}
	return nil
}
func (a *account) leave() { a.release(); a.backend.ops.Done() }

// Authenticate supports only PLAIN/LOGIN, including the protocol LOGIN command.
// Usernames match exactly; a distinct authorization identity is refused.
func (b *Backend) Authenticate(ctx context.Context, conn *imapserver.ConnInfo, credentials *imapserver.Credentials, options *imapserver.AuthenticateOptions) (imapserver.Session, error) {
	_, _ = conn, options
	if err := b.begin(ctx); err != nil {
		return nil, err
	}
	defer b.ops.Done()
	if credentials == nil || (credentials.Mechanism != "PLAIN" && credentials.Mechanism != "LOGIN") || credentials.AuthzID != "" && credentials.AuthzID != credentials.Username {
		return nil, noError(imap.CodeAuthenticationFailed, "authentication failed")
	}
	a := b.accounts[credentials.Username]
	// An empty stored password is no credential. Open refuses one; refusing it
	// here as well keeps an empty LOGIN password from ever matching it.
	if a == nil || a.password == "" || subtle.ConstantTimeCompare([]byte(a.password), []byte(credentials.Password)) != 1 {
		return nil, noError(imap.CodeAuthenticationFailed, "authentication failed")
	}
	if err := a.acquire(ctx); err != nil {
		return nil, err
	}
	failed := a.failure != nil
	a.release()
	if failed {
		return nil, noError(imap.CodeUnavailable, "account requires recovery; repair storage and reopen the backend")
	}
	return &session{account: a, selections: make(map[*selected]bool)}, nil
}

// SupportsCapability witnesses exactly the supported persistent behaviours.
func (b *Backend) SupportsCapability(ctx context.Context, name string, options *imapserver.CapabilitySupportOptions) bool {
	_ = options
	if checkContext(ctx) != nil {
		return false
	}
	switch name {
	case "UIDPLUS", "MOVE", "CONDSTORE", "QRESYNC", "CHILDREN", "SPECIAL-USE", "CREATE-SPECIAL-USE", "STATUS=SIZE":
		return true
	}
	return false
}

func (b *Backend) fault(stage string) error {
	b.mu.Lock()
	hook := b.faultHook
	b.mu.Unlock()
	if hook != nil {
		return hook(stage)
	}
	return nil
}
func checkContext(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("maildir: a non-nil context is required")
	}
	return ctx.Err()
}
func noError(code imap.ResponseCode, text string) error {
	return &imap.Error{Type: imap.ErrorTypeNo, Code: code, Text: text}
}
func mailboxKey(name string) string {
	if strings.EqualFold(name, "INBOX") {
		return "INBOX"
	}
	return name
}
func validMailbox(name string) error {
	if name == "" || !utf8.ValidString(name) || strings.ContainsAny(name, "\x00\r\n") {
		return noError(imap.CodeCannot, "invalid mailbox name")
	}
	return nil
}
func defaultFlags() []imap.Flag {
	return []imap.Flag{imap.FlagAnswered, imap.FlagFlagged, imap.FlagDeleted, imap.FlagSeen, imap.FlagDraft}
}

var _ imapserver.Backend = (*Backend)(nil)
var _ imapserver.CapabilitySupport = (*Backend)(nil)
