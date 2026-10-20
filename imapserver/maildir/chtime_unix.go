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
	if !setUnixSeconds(&tv.Sec, date.Unix()) {
		return errUnrepresentableDate
	}
	if err := syscall.Futimes(int(file.Fd()), []syscall.Timeval{tv, tv}); err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.ModTime().Equal(date.Truncate(time.Microsecond)) {
		return errUnrepresentableDate
	}
	return nil
}

var errUnrepresentableDate = fmt.Errorf("maildir: INTERNALDATE cannot be represented by the filesystem")

// setUnixSeconds stores sec in a syscall seconds field, which is int32 on
// 32-bit Linux, and reports false instead of truncating to another date.
func setUnixSeconds[T int32 | int64](field *T, sec int64) bool {
	*field = T(sec)
	return int64(*field) == sec
}
