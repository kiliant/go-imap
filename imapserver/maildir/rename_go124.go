//go:build !go1.25

package maildir

import (
	"os"
	"path/filepath"
)

// Go 1.24's Root has no Rename. Structural directories must remain owned by
// this backend; delivery may add/rename regular files inside tmp/ and new/.
func renameFile(root *os.Root, from, to string) error {
	if err := checkedPath(root, from); err != nil {
		return err
	}
	if err := checkedPath(root, to); err != nil {
		return err
	}
	return os.Rename(filepath.Join(root.Name(), from), filepath.Join(root.Name(), to))
}
