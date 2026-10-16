//go:build darwin || linux

package maildir

import (
	"os"
	"syscall"
	"time"
)

func chtimesFile(file *os.File, date time.Time) error {
	tv := syscall.NsecToTimeval(date.UnixNano())
	return syscall.Futimes(int(file.Fd()), []syscall.Timeval{tv, tv})
}
