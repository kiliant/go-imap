package maildir

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestInterruptedInitialSetupCanResume(t *testing.T) {
	for _, kind := range []string{"lock", "root-temp", "lost+found", "identity-temp"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, ".imap-lock"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			id, err := uniqueName()
			if err != nil {
				t.Fatal(err)
			}
			var leftover string
			switch kind {
			case "root-temp":
				leftover = filepath.Join(root, ".imap-root.json.tmp-"+id)
			case "lost+found":
				if err := os.Mkdir(filepath.Join(root, kind), 0700); err != nil {
					t.Fatal(err)
				}
			case "identity-temp":
				b, err := Open(context.Background(), root, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := b.Close(context.Background(), nil); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(accountPath(root), 0700); err != nil {
					t.Fatal(err)
				}
				leftover = filepath.Join(accountPath(root), identityFile+".tmp-"+id)
			}
			if leftover != "" {
				if err := os.WriteFile(leftover, []byte("partial"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			b := openTest(t, root)
			appendTest(t, loginTest(t, b), "INBOX")
			if leftover != "" {
				if _, err := os.Lstat(leftover); !os.IsNotExist(err) {
					t.Errorf("interrupted atomic write not cleaned up: %v", err)
				}
			}
		})
	}
}
