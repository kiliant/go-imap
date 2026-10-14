//go:build !(linux || darwin || freebsd || netbsd || openbsd)

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// lockFile creates path exclusively and removes it on unlock. Unlike flock,
// a crashed run leaves the file behind; the error says so.
func lockFile(path string) (unlock func() error, err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("%w (remove %s if no backup is running)", errLocked, path)
		}
		return nil, err
	}
	_ = f.Close()
	return func() error { return os.Remove(path) }, nil
}
