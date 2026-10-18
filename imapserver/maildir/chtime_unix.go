//go:build darwin || linux

package maildir

import (
	"fmt"
	"os"
	"syscall"
	"time"
)

func chtimesFile(file *os.File, date time.Time) error {
	// UnixNano wraps outside 1678–2262 even when the filesystem can represent
	// the date. Convert whole seconds and the microsecond fraction separately.
	tv := syscall.NsecToTimeval(int64(date.Nanosecond()/1000) * 1000)
	tv.Sec = date.Unix()
	if err := syscall.Futimes(int(file.Fd()), []syscall.Timeval{tv, tv}); err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.ModTime().Equal(date.Truncate(time.Microsecond)) {
		return fmt.Errorf("maildir: INTERNALDATE cannot be represented by the filesystem")
	}
	return nil
}
