//go:build !darwin && !linux

package maildir

import (
	"fmt"
	"os"
)

func lockFile(*os.File) error {
	return fmt.Errorf("maildir: advisory root locking is supported only on Linux and macOS")
}

func readOpenFlags() int { return os.O_RDONLY }
