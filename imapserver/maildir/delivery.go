package maildir

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
)

var filenameFlags = []struct {
	flag   imap.Flag
	letter byte
}{{imap.FlagDraft, 'D'}, {imap.FlagFlagged, 'F'}, {imap.FlagAnswered, 'R'}, {imap.FlagSeen, 'S'}, {imap.FlagDeleted, 'T'}}

func flagSuffix(flags []imap.Flag) string {
	var b strings.Builder
	b.WriteString(":2,")
	for _, item := range filenameFlags {
		if imap.ContainsFlag(flags, item.flag) {
			b.WriteByte(item.letter)
		}
	}
	return b.String()
}
func flagsFromFilename(name string) []imap.Flag {
	_, info, ok := strings.Cut(name, ":2,")
	if !ok {
		return nil
	}
	var flags []imap.Flag
	for _, item := range filenameFlags {
		if strings.ContainsRune(info, rune(item.letter)) {
			flags = append(flags, item.flag)
		}
	}
	return flags
}
func messagePath(name string, msg *messageRecord) string {
	return filepath.Join(mailboxPath(name), "cur", msg.File)
}

// Retain staged bytes after a possibly committed journal error; replay owns
// them. Before the journal exists, failed/canceled operations may discard them.
func (a *account) discardStage(path string) {
	if a.failure == nil {
		_ = a.root.Remove(path)
	}
}

type normalizer struct {
	source  io.Reader
	prev    byte
	pending []byte
	err     error
	buffer  [32768]byte
}

func (r *normalizer) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(r.pending) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		n, err := r.source.Read(r.buffer[:])
		r.err = err
		for _, c := range r.buffer[:n] {
			if c == '\n' && r.prev != '\r' {
				r.pending = append(r.pending, '\r')
			}
			r.pending = append(r.pending, c)
			r.prev = c
		}
		if len(r.pending) == 0 {
			return 0, err
		}
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

func (a *account) stage(ctx context.Context, source io.Reader, date time.Time, normalize bool) (path string, size int64, actualDate time.Time, retErr error) {
	id, err := uniqueName()
	if err != nil {
		return "", 0, time.Time{}, err
	}
	path = filepath.Join(".staging", id)
	stagedPath := path
	f, err := a.root.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return "", 0, time.Time{}, err
	}
	defer func() {
		f.Close()
		if retErr != nil {
			a.root.Remove(stagedPath)
		}
	}()
	reader := io.Reader(contextReader{ctx: ctx, source: source})
	if normalize {
		reader = &normalizer{source: reader}
	}
	size, err = io.Copy(f, reader)
	if err != nil {
		return "", 0, time.Time{}, err
	}
	if err := ctx.Err(); err != nil {
		return "", 0, time.Time{}, err
	}
	if err := f.Chmod(0600); err != nil {
		return "", 0, time.Time{}, err
	}
	if err := chtimesFile(f, date); err != nil {
		return "", 0, time.Time{}, err
	}
	if err := f.Sync(); err != nil {
		return "", 0, time.Time{}, err
	}
	info, err := f.Stat()
	if err != nil {
		return "", 0, time.Time{}, err
	}
	actualDate = info.ModTime()
	if err := f.Close(); err != nil {
		return "", 0, time.Time{}, err
	}
	if err := syncDir(a.root, ".staging"); err != nil {
		return "", 0, time.Time{}, err
	}
	return path, size, actualDate, nil
}

// poll runs one poller per account. A single poller walking the accounts in
// turn waited on each account's lock, so an operation holding one account —
// an APPEND whose client sends its literal slowly, a FETCH streaming to a slow
// reader — stalled new-mail delivery for every account until it finished.
func (b *Backend) poll(ctx context.Context, interval time.Duration) {
	defer close(b.pollDone)
	var pollers sync.WaitGroup
	for _, a := range b.accounts {
		pollers.Add(1)
		go func() {
			defer pollers.Done()
			b.pollAccount(ctx, a, interval)
		}()
	}
	pollers.Wait()
}
func (b *Backend) pollAccount(ctx context.Context, a *account, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		err := a.pollOnce(ctx)
		if err != nil && ctx.Err() == nil {
			b.reportPollFailure(ctx, a, err)
		} else if err == nil {
			b.mu.Lock()
			delete(b.pollFailures, a.username)
			b.mu.Unlock()
		}
	}
}

func (a *account) pollOnce(ctx context.Context) error {
	if err := a.backend.begin(ctx); err != nil {
		return err
	}
	if err := a.acquire(ctx); err != nil {
		a.backend.ops.Done()
		return err
	}
	defer a.leave()
	if a.failure != nil {
		return a.failure
	}
	err := a.reconcileContext(ctx)
	return errors.Join(err, a.deliveryError)
}

func (b *Backend) recordPollFailure(a *account, err error) PollFailure {
	failure := PollFailure{Username: a.username, Err: err}
	var mailboxError *mailboxPollError
	if errors.As(err, &mailboxError) {
		failure.Mailbox = mailboxError.mailbox
	}
	b.mu.Lock()
	b.lastPollError = fmt.Errorf("maildir: account %q: %w", a.username, err)
	if b.pollFailures == nil {
		b.pollFailures = make(map[string]PollFailure)
	}
	b.pollFailures[a.username] = failure
	b.mu.Unlock()
	return failure
}

// callbackMu serializes callbacks, outside account/backend locks. Give a
// callback its own copy so it cannot mutate the diagnostics retained by b.
func (b *Backend) reportPollFailure(ctx context.Context, a *account, err error) {
	failure := b.recordPollFailure(a, err)
	if b.onPollFailure != nil || b.onPollError != nil {
		b.callbackMu.Lock()
		defer b.callbackMu.Unlock()
		if b.onPollFailure != nil {
			b.onPollFailure(ctx, &failure)
		} else {
			b.onPollError(err)
		}
	}
}

type mailboxPollError struct {
	mailbox string
	err     error
}

func (e *mailboxPollError) Error() string {
	return fmt.Sprintf("maildir: mailbox %q: %v", e.mailbox, e.err)
}
func (e *mailboxPollError) Unwrap() error { return e.err }

func (a *account) reconcile() error { return a.reconcileContext(context.Background()) }
func (a *account) reconcileContext(ctx context.Context) (retErr error) {
	var currentMailbox string
	defer func() {
		if retErr != nil && currentMailbox != "" {
			retErr = &mailboxPollError{mailbox: currentMailbox, err: retErr}
		}
	}()
	a.deliveryError = nil
	if changed, err := a.reconciliationNeeded(ctx); err != nil || !changed {
		return err
	}
	next, err := cloneState(a.state)
	if err != nil {
		return err
	}
	var actions []fileAction
	changes := make(map[uint32][]imapserver.Update)
	names := make([]string, 0, len(next.Mailboxes))
	for name := range next.Mailboxes {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		currentMailbox = name
		if err := ctx.Err(); err != nil {
			return err
		}
		m := next.Mailboxes[name]
		known := make(map[string]bool)
		var retained []*messageRecord
		for _, msg := range m.Messages {
			info, err := a.root.Lstat(messagePath(name, msg))
			if os.IsNotExist(err) {
				modseq, err := bump(m)
				if err != nil {
					return err
				}
				m.Tombstones = append(m.Tombstones, tombstone{UID: msg.UID, ModSeq: modseq})
				changes[m.UIDValidity] = append(changes[m.UIDValidity], &imapserver.UpdateExpunge{UID: msg.UID})
				continue
			}
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() || info.Size() != msg.Size {
				return fmt.Errorf("maildir: stored message changed outside the backend")
			}
			known[msg.File] = true
			retained = append(retained, msg)
		}
		m.Messages = retained
		for _, sub := range []string{"cur", "new"} {
			dir := filepath.Join(mailboxPath(name), sub)
			entries, err := readDir(a.root, dir)
			if err != nil {
				return err
			}
			slices.SortFunc(entries, func(a, b os.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".") || sub == "cur" && known[entry.Name()] {
					continue
				}
				sourcePath := filepath.Join(dir, entry.Name())
				f, err := openRegular(a.root, sourcePath)
				if err != nil {
					if sub == "new" {
						a.skipDelivery(name, sourcePath, err)
						continue
					}
					return err
				}
				info, err := f.Stat()
				if err != nil {
					f.Close()
					if sub == "new" {
						a.skipDelivery(name, sourcePath, err)
						continue
					}
					return err
				}
				flags := flagsFromFilename(entry.Name())
				file := entry.Name()
				size, date := info.Size(), info.ModTime()
				if sub == "new" {
					stage, n, mtime, err := a.stage(ctx, f, date, true)
					f.Close()
					if err != nil {
						if ctx.Err() != nil {
							return ctx.Err()
						}
						a.skipDelivery(name, sourcePath, err)
						continue
					}
					defer a.discardStage(stage)
					file = filepath.Base(stage) + flagSuffix(flags)
					size, date = n, mtime
					actions = append(actions, fileAction{Kind: "rename", From: stage, To: filepath.Join(mailboxPath(name), "cur", file)}, fileAction{Kind: "remove", From: sourcePath})
				} else {
					f.Close()
				}
				uid, err := allocateUID(m)
				if err != nil {
					return err
				}
				modseq, err := bump(m)
				if err != nil {
					return err
				}
				m.Messages = append(m.Messages, &messageRecord{UID: uid, File: file, Size: size, Date: date, Flags: flags, ModSeq: modseq})
				changes[m.UIDValidity] = append(changes[m.UIDValidity], &imapserver.UpdateAdd{UIDs: []imap.UID{uid}})
				if ensureFlags(m, flags) {
					changes[m.UIDValidity] = withMailboxFlags(changes[m.UIDValidity], m.Flags)
				}
			}
		}
	}
	currentMailbox = ""
	if len(changes) == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return a.publishCommit(next, actions, changes, 0)
}

func (a *account) skipDelivery(mailbox, path string, err error) {
	// Bound diagnostics independently of the number of invalid entries. Leave
	// the original file for an operator to repair; never follow or delete it.
	if a.deliveryError == nil {
		a.deliveryError = &mailboxPollError{mailbox: mailbox, err: fmt.Errorf("maildir: skipped delivery %q: %w", path, err)}
	}
}
