//go:build darwin || linux

package maildir

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

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
			// the standard path API as an oracle: unsupported dates must be
			// refused instead of silently stored with a different INTERNALDATE.
			probe := filepath.Join(t.TempDir(), "probe")
			if err := os.WriteFile(probe, nil, 0600); err != nil {
				t.Fatal(err)
			}
			probeErr := os.Chtimes(probe, date, date)
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
