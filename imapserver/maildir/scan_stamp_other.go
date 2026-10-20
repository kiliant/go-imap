//go:build !darwin && !linux

package maildir

import (
	"os"
	"time"
)

func directoryChangeTime(os.FileInfo) time.Time { return time.Time{} }
