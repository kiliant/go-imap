package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestBackupRepeatedBodyLeavesNoTemporaryFiles(t *testing.T) {
	for _, uidAfterBody := range []bool{false, true} {
		t.Run(fmt.Sprintf("uidAfterBody=%t", uidAfterBody), func(t *testing.T) {
			a := &fakeAccount{uidAfterBody: uidAfterBody, repeatBody: true}
			a.addMailbox("INBOX", 7)
			a.deliver("INBOX", day1, msg("repeated"))
			root := filepath.Join(t.TempDir(), "backup")
			mustBackup(t, a.serve(t), root)
			inbox := filepath.Join(root, "INBOX")
			assertFiles(t, inbox, map[string]string{"V7.U1:2,": msg("repeated")})
			entries, err := os.ReadDir(filepath.Join(inbox, "tmp"))
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("temporary body files left after successful backup: %v", entries)
			}
		})
	}
}
