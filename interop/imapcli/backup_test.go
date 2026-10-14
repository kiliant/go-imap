//go:build interop

package imapcli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapclient"
	"github.com/kiliant/go-imap/interop/harness"
)

const (
	interopUser     = "interop@example.test"
	interopPassword = "interop-pw"
)

// serverMessage is what the server reports for one INBOX message.
type serverMessage struct {
	flags []imap.Flag
	date  time.Time
}

// TestBackupInterop backs up a live account three times — first, after
// changes, and once with nothing to do — and checks the Maildir against the
// server each time.
func TestBackupInterop(t *testing.T) {
	for _, server := range harness.RunningServers() {
		server := server
		t.Run(server.Profile.Name, func(t *testing.T) {
			t.Parallel()
			if server.Profile.TLSPort != 0 {
				t.Skip("profile requires implicit-TLS login, which this package does not wire up")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			c := login(ctx, t, server.Address)
			defer c.Close()

			prefix := server.Profile.MailboxPrefix
			delim := hierarchyDelimiter(ctx, t, c)
			parent := prefix + "imapcli-parent"
			child := parent + string(delim) + "child é"
			gone := prefix + "imapcli-gone"
			mutate(ctx, t, server.Address, func(m *imapclient.Client) {
				for _, name := range []string{parent, child, gone} {
					if err := m.Create(name, nil).Wait(ctx); err != nil {
						t.Fatalf("CREATE %s: %v", name, err)
					}
				}
				appendMessage(ctx, t, m, child, "Subject: child\r\n\r\nin a nested, non-ASCII mailbox\r\n")
				appendMessage(ctx, t, m, gone, "Subject: gone\r\n\r\ndeleted before the second run\r\n")
			})

			root := t.TempDir()
			passFile := filepath.Join(t.TempDir(), "password")
			if err := os.WriteFile(passFile, []byte(interopPassword+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			inboxDir := filepath.Join(root, "INBOX")
			childDir := filepath.Join(root, "imapcli-parent", "child é")

			// First run.
			uidValidity, before := inboxState(ctx, t, c)
			settle(ctx, t, server.Address, func(fresh *imapclient.Client) error {
				return listed(ctx, fresh, map[string]bool{child: true, gone: true})
			})
			runBackup(ctx, t, server.Address, passFile, root)
			files := maildir(t, inboxDir)
			if len(files) != len(before) {
				t.Fatalf("INBOX: %d files for %d server messages", len(files), len(before))
			}
			byContent := make(map[[32]byte]string)
			for name := range files {
				v, uid := parseName(t, name)
				if v != uidValidity {
					t.Fatalf("%s: UIDVALIDITY %d, server has %d", name, v, uidValidity)
				}
				msg, ok := before[uid]
				if !ok {
					t.Fatalf("%s: UID %d is not on the server", name, uid)
				}
				assertModTime(t, filepath.Join(inboxDir, "cur", name), msg.date)
				byContent[fileHash(t, filepath.Join(inboxDir, "cur", name))] = name
			}
			fixtureFile := make(map[string]string) // fixture name → file name
			for _, f := range harness.Fixtures() {
				name, ok := byContent[readerHash(t, f.Open())]
				if !ok {
					t.Fatalf("fixture %q is not in the backup byte for byte", f.Name)
				}
				fixtureFile[f.Name] = name
			}
			assertInfo(t, fixtureFile["seen"], 'S', true)
			assertInfo(t, fixtureFile["flagged"], 'F', true)
			assertInfo(t, fixtureFile["plain"], 'S', false)
			if got := maildir(t, childDir); len(got) != 1 {
				t.Fatalf("nested mailbox %q: %d files, want 1", child, len(got))
			}
			if prefix == "" {
				if _, err := os.Stat(filepath.Join(root, "Tëst", ".imapcli-mailbox.json")); err != nil {
					t.Fatalf("seeded mailbox Tëst was not backed up under its UTF-8 name: %v", err)
				}
			}
			if _, after := inboxState(ctx, t, c); !sameFlags(before, after) {
				t.Fatalf("the backup changed flags on the server:\nbefore %v\nafter  %v", before, after)
			}

			// Second run, after a new message, a flag change, an expunge and a
			// deleted mailbox.
			_, plainUID := parseName(t, fixtureFile["plain"])
			_, emptyUID := parseName(t, fixtureFile["empty-body"])
			mutate(ctx, t, server.Address, func(m *imapclient.Client) {
				appendMessage(ctx, t, m, "INBOX", "Subject: after the first run\r\n\r\nnew\r\n")
				if _, err := m.Select("INBOX", nil).Wait(ctx); err != nil {
					t.Fatal(err)
				}
				if err := m.StoreUID(imap.UIDSetNum(plainUID), []imap.Flag{imap.FlagAnswered}, &imapclient.StoreOptions{Op: imapclient.StoreFlagsAdd, Silent: true}).Wait(ctx); err != nil {
					t.Fatal(err)
				}
				if err := m.StoreUID(imap.UIDSetNum(emptyUID), []imap.Flag{imap.FlagDeleted}, &imapclient.StoreOptions{Op: imapclient.StoreFlagsAdd, Silent: true}).Wait(ctx); err != nil {
					t.Fatal(err)
				}
				if err := m.Expunge(nil).Wait(ctx); err != nil {
					t.Fatal(err)
				}
				if err := m.Delete(gone, nil).Wait(ctx); err != nil {
					t.Fatal(err)
				}
			})
			_, changed := inboxState(ctx, t, c)
			settle(ctx, t, server.Address, func(fresh *imapclient.Client) error {
				if err := listed(ctx, fresh, map[string]bool{gone: false}); err != nil {
					return err
				}
				if _, seen := inboxState(ctx, t, fresh); !sameFlags(changed, seen) {
					return fmt.Errorf("INBOX not yet as changed: %v", seen)
				}
				return nil
			})
			out := runBackup(ctx, t, server.Address, passFile, root)
			if !strings.Contains(out, "INBOX: "+strconv.Itoa(len(before))+" messages, 1 new, 1 flags changed, 1 removed") {
				t.Fatalf("second run did not report the INBOX changes:\n%s", out)
			}
			if !strings.Contains(out, "1 mailboxes pruned, 0 failed") {
				t.Fatalf("second run did not prune the deleted mailbox:\n%s", out)
			}
			files = maildir(t, inboxDir)
			if _, ok := files[fixtureFile["empty-body"]]; ok {
				t.Fatal("expunged message is still in the backup")
			}
			if _, ok := files[fixtureFile["plain"]]; ok {
				t.Fatal("flag change did not rename the file")
			}
			renamed := false
			for name := range files {
				if _, uid := parseName(t, name); uid == plainUID {
					assertInfo(t, name, 'R', true)
					renamed = true
				}
			}
			if !renamed {
				t.Fatal("message with the changed flag is missing")
			}
			if _, err := os.Stat(filepath.Join(root, "imapcli-gone")); !os.IsNotExist(err) {
				t.Fatalf("deleted mailbox is still in the backup: %v", err)
			}

			// Third run: nothing to do.
			out = runBackup(ctx, t, server.Address, passFile, root)
			if !strings.Contains(out, " 0 new, 0 flags changed, 0 removed, 0 mailboxes pruned, 0 failed") {
				t.Fatalf("no-op run changed something:\n%s", out)
			}
		})
	}
}

func login(ctx context.Context, t *testing.T, addr string) *imapclient.Client {
	t.Helper()
	c, err := imapclient.Dial(ctx, addr, &imapclient.Options{AllowInsecureAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Login(ctx, interopUser, interopPassword, nil); err != nil {
		_ = c.Close()
		t.Fatal(err)
	}
	return c
}

// mutate changes server state in a session of its own that logs out
// afterwards, as a mail client would, before the backup's session looks.
//
// Stalwart 0.11.8 needs this. When a session CREATEs a mailbox, APPENDs to it
// and then EXAMINEs or SELECTs a mailbox, other sessions do not list the new
// mailbox while that session stays open; the creating session does. Dovecot
// lists it everywhere at once.
func mutate(ctx context.Context, t *testing.T, addr string, change func(*imapclient.Client)) {
	t.Helper()
	m := login(ctx, t, addr)
	defer m.Close()
	change(m)
	if err := m.Logout(ctx, nil); err != nil {
		t.Fatalf("LOGOUT after changing the account: %v", err)
	}
}

// settle waits until a new session sees what mutate changed, and logs how
// long that took, so a server that applies changes lazily is measured rather
// than mistaken for a backup bug.
func settle(ctx context.Context, t *testing.T, addr string, check func(*imapclient.Client) error) {
	t.Helper()
	start := time.Now()
	for attempt := 1; ; attempt++ {
		fresh := login(ctx, t, addr)
		err := check(fresh)
		_ = fresh.Logout(ctx, nil)
		_ = fresh.Close()
		if err == nil {
			if attempt > 1 {
				t.Logf("a new session saw the change after %d attempts (%v)", attempt, time.Since(start).Round(time.Millisecond))
			}
			return
		}
		if time.Since(start) > 30*time.Second {
			t.Fatalf("a new session still does not see the change: %v", err)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// listed checks that each mailbox in want is listed (true) or not (false).
func listed(ctx context.Context, c *imapclient.Client, want map[string]bool) error {
	list, err := c.List("", "*", nil).Wait(ctx)
	if err != nil {
		return err
	}
	have := make(map[string]bool)
	var names []string
	for _, m := range list {
		have[m.Mailbox] = true
		names = append(names, fmt.Sprintf("%q", m.Mailbox))
	}
	for name, present := range want {
		if have[name] != present {
			return fmt.Errorf("mailbox %q listed = %v, want %v; LIST has %s", name, have[name], present, strings.Join(names, " "))
		}
	}
	return nil
}

func hierarchyDelimiter(ctx context.Context, t *testing.T, c *imapclient.Client) rune {
	t.Helper()
	list, err := c.List("", "INBOX", nil).Wait(ctx)
	if err != nil || len(list) == 0 {
		t.Fatalf("LIST INBOX: %v (%d results)", err, len(list))
	}
	if list[0].Delimiter == 0 {
		t.Skip("server has no hierarchy delimiter")
	}
	return list[0].Delimiter
}

func appendMessage(ctx context.Context, t *testing.T, c *imapclient.Client, mailbox, message string) {
	t.Helper()
	if _, err := c.Append(ctx, mailbox, nil, int64(len(message)), strings.NewReader(message)).Wait(ctx); err != nil {
		t.Fatalf("APPEND to %s: %v", mailbox, err)
	}
}

// inboxState reads INBOX's UIDVALIDITY and every message's flags and
// INTERNALDATE, without changing anything.
func inboxState(ctx context.Context, t *testing.T, c *imapclient.Client) (uint32, map[imap.UID]serverMessage) {
	t.Helper()
	status, err := c.Examine("INBOX", nil).Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	msgs := make(map[imap.UID]serverMessage)
	cmd := c.FetchUID(imap.UIDSet{{Start: 1}}, nil, imap.FetchItemUID, imap.FetchItemFlags, imap.FetchItemInternalDate)
	for {
		data, err := cmd.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		var uid imap.UID
		var m serverMessage
		for _, values := range data.Items {
			for _, v := range values {
				switch v := v.(type) {
				case imap.FetchDataUID:
					uid = imap.UID(v)
				case imap.FetchDataFlags:
					m.flags = []imap.Flag(v)
				case *imap.FetchDataInternalDate:
					m.date = v.Time
				}
			}
		}
		if uid != 0 {
			msgs[uid] = m
		}
	}
	return status.UIDValidity, msgs
}

func sameFlags(a, b map[imap.UID]serverMessage) bool {
	if len(a) != len(b) {
		return false
	}
	norm := func(flags []imap.Flag) []string {
		var out []string
		for _, f := range flags {
			if !f.Equal(imap.FlagRecent) {
				out = append(out, strings.ToLower(string(f)))
			}
		}
		slices.Sort(out)
		return out
	}
	for uid, m := range a {
		if !slices.Equal(norm(m.flags), norm(b[uid].flags)) {
			return false
		}
	}
	return true
}

func runBackup(ctx context.Context, t *testing.T, addr, passFile, root string) string {
	t.Helper()
	cmd := exec.CommandContext(ctx, binary, "-addr", addr, "-user", interopUser, "-pass-file", passFile, "-plain", "-backup", root)
	cmd.Env = append(os.Environ(), "IMAP_PASS=", "IMAP_PASS_CMD=", "IMAP_PASS_FILE=")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("imapcli -backup: %v\nstderr:\n%s\nstdout:\n%s", err, stderr.String(), stdout.String())
	}
	t.Logf("imapcli -backup:\n%s", stdout.String())
	return stdout.String()
}

// maildir returns the names of the files in dir/cur.
func maildir(t *testing.T, dir string) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "cur"))
	if err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, e := range entries {
		names[e.Name()] = true
	}
	return names
}

// parseName splits "V<uidvalidity>.U<uid>:2,<info>".
func parseName(t *testing.T, name string) (uint32, imap.UID) {
	t.Helper()
	base, _, _ := strings.Cut(name, ":2,")
	v, u, ok := strings.Cut(strings.TrimPrefix(base, "V"), ".U")
	vn, err1 := strconv.ParseUint(v, 10, 32)
	un, err2 := strconv.ParseUint(u, 10, 32)
	if !ok || err1 != nil || err2 != nil {
		t.Fatalf("unexpected file name %q", name)
	}
	return uint32(vn), imap.UID(un)
}

func assertInfo(t *testing.T, name string, letter byte, want bool) {
	t.Helper()
	_, info, _ := strings.Cut(name, ":2,")
	if got := strings.IndexByte(info, letter) >= 0; got != want {
		t.Fatalf("%s: flag %c present = %v, want %v", name, letter, got, want)
	}
}

func assertModTime(t *testing.T, path string, want time.Time) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(want) {
		t.Fatalf("%s: mtime %v, INTERNALDATE %v", path, info.ModTime(), want)
	}
}

func fileHash(t *testing.T, path string) [32]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	return readerHash(t, f)
}

func readerHash(t *testing.T, r io.Reader) [32]byte {
	t.Helper()
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		t.Fatal(err)
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum
}
