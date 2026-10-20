//go:build darwin || linux

package maildir

import (
	"math"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// 64-bit CI runners never instantiate the int32 field, so test it directly.
func TestSetUnixSecondsRefusesTruncation(t *testing.T) {
	var narrow int32
	for _, sec := range []int64{math.MinInt32, 0, math.MaxInt32} {
		if !setUnixSeconds(&narrow, sec) || int64(narrow) != sec {
			t.Errorf("int32 field: setUnixSeconds(%d) = %d, refused or changed", sec, narrow)
		}
	}
	for _, sec := range []int64{math.MinInt32 - 1, math.MaxInt32 + 1, math.MaxInt64} {
		if setUnixSeconds(&narrow, sec) {
			t.Errorf("int32 field: setUnixSeconds(%d) accepted %d", sec, narrow)
		}
	}
	var wide int64
	for _, sec := range []int64{math.MinInt64, math.MaxInt32 + 1, math.MaxInt64} {
		if !setUnixSeconds(&wide, sec) || wide != sec {
			t.Errorf("int64 field: setUnixSeconds(%d) = %d, refused or changed", sec, wide)
		}
	}
}

func TestFileTimeOutsideUnixNanoRange(t *testing.T) {
	for _, year := range []int{1600, 2000, 2400} {
		date := time.Date(year, 6, 15, 12, 34, 56, 123456000, time.UTC)
		t.Run(date.Format("2006"), func(t *testing.T) {
			file, err := os.Create(filepath.Join(t.TempDir(), "message"))
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			// Some filesystems clamp dates to their own timestamp range. Use
			// the path syscall as an oracle: unsupported dates must be refused
			// instead of silently stored with a different INTERNALDATE. Avoid
			// os.Chtimes, which itself converts through overflowing UnixNano.
			probe := filepath.Join(t.TempDir(), "probe")
			if err := os.WriteFile(probe, nil, 0600); err != nil {
				t.Fatal(err)
			}
			ts := syscall.NsecToTimespec(int64(date.Nanosecond()))
			var probeErr error = syscall.ERANGE
			if setUnixSeconds(&ts.Sec, date.Unix()) {
				probeErr = syscall.UtimesNano(probe, []syscall.Timespec{ts, ts})
			}
			probeInfo, err := os.Stat(probe)
			if err != nil {
				t.Fatal(err)
			}
			err = chtimesFile(file, date)
			if probeErr != nil || !probeInfo.ModTime().Equal(date) {
				if err == nil {
					t.Fatal("filesystem cannot represent date but chtimesFile accepted it")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			info, err := file.Stat()
			if err != nil {
				t.Fatal(err)
			}
			if !info.ModTime().Equal(date) {
				t.Errorf("file time = %s, want %s", info.ModTime(), date)
			}
		})
	}
}
