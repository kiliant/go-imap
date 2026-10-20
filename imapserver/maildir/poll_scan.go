package maildir

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Directory change stamps index externally delivered/removed names. Stored
// message contents are immutable by contract; a rotating bounded audit still
// checks for unsupported in-place edits without restatting the whole account.
// A changing poll rescans under the same gate as SELECT/update attachment.
type directorySnapshot struct {
	cur, new os.FileInfo
	audit    int
}

func (a *account) directorySnapshot(name string) (directorySnapshot, error) {
	var out directorySnapshot
	var err error
	out.cur, err = a.root.Lstat(filepath.Join(mailboxPath(name), "cur"))
	if err == nil {
		out.new, err = a.root.Lstat(filepath.Join(mailboxPath(name), "new"))
	}
	if err == nil && (!out.cur.IsDir() || !out.new.IsDir()) {
		err = fmt.Errorf("maildir: stored mailbox directory changed")
	}
	return out, err
}

func sameDirectory(a, b os.FileInfo) bool {
	return a != nil && b != nil && os.SameFile(a, b) && a.ModTime() == b.ModTime() && directoryChangeTime(a) == directoryChangeTime(b) && !directoryChangeTime(a).IsZero()
}
func sameDirectories(a, b directorySnapshot) bool {
	return sameDirectory(a.cur, b.cur) && sameDirectory(a.new, b.new)
}

func (a *account) reconciliationNeeded(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if a.scanCache == nil {
		a.scanCache = make(map[string]directorySnapshot)
	}
	for name := range a.scanCache {
		if a.state.Mailboxes[name] == nil {
			delete(a.scanCache, name)
		}
	}
	for name, m := range a.state.Mailboxes {
		stamp, err := a.directorySnapshot(name)
		if err != nil {
			return false, &mailboxPollError{mailbox: name, err: err}
		}
		cached, valid := a.scanCache[name]
		delete(a.scanCache, name)
		if valid && sameDirectories(cached, stamp) {
			// new/ is normally empty. Read it even on a cache hit: local
			// filesystems may coalesce directory timestamps, and delivery
			// must remain visible on the next poll on those filesystems too.
			entries, err := readDir(a.root, filepath.Join(mailboxPath(name), "new"))
			if err != nil {
				return false, &mailboxPollError{mailbox: name, err: err}
			}
			for _, entry := range entries {
				if !strings.HasPrefix(entry.Name(), ".") {
					return true, nil
				}
			}
			for range min(16, len(m.Messages)) {
				if err := ctx.Err(); err != nil {
					return false, err
				}
				msg := m.Messages[cached.audit%len(m.Messages)]
				cached.audit = (cached.audit + 1) % len(m.Messages)
				info, err := a.root.Lstat(messagePath(name, msg))
				if os.IsNotExist(err) {
					return true, nil
				}
				if err == nil && (!info.Mode().IsRegular() || info.Size() != msg.Size) {
					err = fmt.Errorf("maildir: stored message changed outside the backend")
				}
				if err != nil {
					return false, &mailboxPollError{mailbox: name, err: err}
				}
			}
			a.scanCache[name] = cached
			continue
		}
		known := make(map[string]bool, len(m.Messages))
		for _, msg := range m.Messages {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			info, err := a.root.Lstat(messagePath(name, msg))
			if os.IsNotExist(err) {
				return true, nil
			}
			if err == nil && (!info.Mode().IsRegular() || info.Size() != msg.Size) {
				err = fmt.Errorf("maildir: stored message changed outside the backend")
			}
			if err != nil {
				return false, &mailboxPollError{mailbox: name, err: err}
			}
			known[msg.File] = true
		}
		for _, sub := range []string{"cur", "new"} {
			entries, err := readDir(a.root, filepath.Join(mailboxPath(name), sub))
			if err != nil {
				return false, &mailboxPollError{mailbox: name, err: err}
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".") || sub == "cur" && known[entry.Name()] {
					continue
				}
				return true, nil
			}
		}
		after, err := a.directorySnapshot(name)
		if err != nil {
			return false, &mailboxPollError{mailbox: name, err: err}
		}
		// Never index an arrival raced with the scan as already consumed.
		if sameDirectories(stamp, after) {
			a.scanCache[name] = stamp
		}
	}
	return false, nil
}
