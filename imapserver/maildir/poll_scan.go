package maildir

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The common idle poll validates files but does not serialize and parse the
// entire account. A changing poll takes the existing transactional path and
// rescans under the same account gate, preserving the SELECT/update boundary.
func (a *account) reconciliationNeeded(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	for name, m := range a.state.Mailboxes {
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
	}
	return false, nil
}
