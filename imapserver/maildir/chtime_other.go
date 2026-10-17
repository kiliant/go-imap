//go:build !darwin && !linux

package maildir

import (
	"fmt"
	"os"
	"time"
)

func chtimesFile(*os.File, time.Time) error {
	return fmt.Errorf("maildir: unsupported filesystem platform")
}
