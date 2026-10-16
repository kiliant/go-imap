//go:build go1.25

package maildir

import "os"

func renameFile(root *os.Root, from, to string) error {
	if err := checkedPath(root, from); err != nil {
		return err
	}
	if err := checkedPath(root, to); err != nil {
		return err
	}
	return root.Rename(from, to)
}
