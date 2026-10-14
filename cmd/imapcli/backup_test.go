package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
)

func TestMailboxPath(t *testing.T) {
	tests := []struct {
		name  string
		delim rune
		want  string
	}{
		{"INBOX", '/', "INBOX"},
		{"Archive/2024", '/', filepath.Join("Archive", "2024")},
		{"INBOX.Sent", '.', filepath.Join("INBOX", "Sent")},
		{"a/b", 0, "a%2Fb"},
		{"a/b", '.', "a%2Fb"},
		{"100%", '/', "100%25"},
		{".hidden", '/', "%2Ehidden"},
		{"..", '/', "%2E."},
		{"cur", '/', "%63ur"},
		{"x/NEW/tmp", '/', filepath.Join("x", "%4EEW", "%74mp")},
		{"a//b", '/', filepath.Join("a", "%", "b")},
		{"tab\there", '/', "tab%09here"},
		{"Grüße", '/', "Grüße"},
	}
	for _, tt := range tests {
		if got := mailboxPath(tt.name, tt.delim); got != tt.want {
			t.Errorf("mailboxPath(%q, %q) = %q, want %q", tt.name, tt.delim, got, tt.want)
		}
	}
}

func TestMessageFileName(t *testing.T) {
	flags := []imap.Flag{"\\seen", imap.FlagDraft, imap.FlagRecent, "$Forwarded", imap.FlagAnswered, "\\Junk"}
	name := messageFileName(7, 42, flags)
	if name != "V7.U42:2,DRS" {
		t.Fatalf("messageFileName = %q", name)
	}
	if got := extraFlags(flags); !reflect.DeepEqual(got, []string{"$Forwarded", "\\Junk"}) {
		t.Fatalf("extraFlags = %q", got)
	}
	v, uid, ok := parseMessageFileName(name)
	if !ok || v != 7 || uid != 42 {
		t.Fatalf("parseMessageFileName(%q) = %d, %d, %v", name, v, uid, ok)
	}
	for _, foreign := range []string{"1700000000.M1P2.host:2,S", "V7.U0:2,", "V7.U42", "Vx.U1:2,", "notes.txt"} {
		if _, _, ok := parseMessageFileName(foreign); ok {
			t.Errorf("parseMessageFileName(%q) accepted a name this tool does not write", foreign)
		}
	}
}

func TestCompileGlobs(t *testing.T) {
	res := compileGlobs([]string{"Junk", "Archive/*", "Lists.?"})
	match := func(name string) bool {
		for _, re := range res {
			if re.MatchString(name) {
				return true
			}
		}
		return false
	}
	for name, want := range map[string]bool{
		"Junk": true, "Junk2": false, "Archive/2024": true, "Archive/2024/Q1": true,
		"Archive": false, "Lists.a": true, "Lists.ab": false, "Listsxa": false,
	} {
		if got := match(name); got != want {
			t.Errorf("match(%q) = %v, want %v", name, got, want)
		}
	}
}

var (
	day1 = time.Date(2026, 3, 1, 9, 30, 0, 0, time.UTC)
	day2 = time.Date(2026, 3, 2, 18, 5, 7, 0, time.FixedZone("", 2*3600))
)

func msg(subject string) string {
	return "From: alice@example.com\r\nSubject: " + subject + "\r\n\r\nbody of " + subject + "\r\n"
}

// newTestAccount returns the account most tests start from.
func newTestAccount() *fakeAccount {
	a := &fakeAccount{}
	a.addMailbox("INBOX", 1000)
	a.deliver("INBOX", day1, msg("one"), `\Seen`)
	a.deliver("INBOX", day2, msg("two"), `\Flagged`, `$Forwarded`)
	a.deliver("INBOX", day1, msg("three"))
	a.addMailbox("Archive", 1).noSelect = true
	a.addMailbox("Archive/2024", 2024)
	a.deliver("Archive/2024", day1, msg("archived"), `\Seen`, `\Answered`)
	a.addMailbox("cur", 5)
	a.deliver("cur", day2, msg("reserved name"))
	a.addMailbox("Empty", 9)
	return a
}

func runBackupCLI(t *testing.T, addr, dir string, extra ...string) (code int, stdout, stderr string) {
	t.Helper()
	clearIMAPEnv(t)
	var out, errOut bytes.Buffer
	args := append([]string{"-addr", addr, "-user", "user", "-pass", "secret", "-plain", "-backup", dir}, extra...)
	code = runIO(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func mustBackup(t *testing.T, addr, dir string, extra ...string) string {
	t.Helper()
	code, stdout, stderr := runBackupCLI(t, addr, dir, extra...)
	if code != 0 {
		t.Fatalf("backup exit = %d\nstderr:\n%s\nstdout:\n%s", code, stderr, stdout)
	}
	return stdout
}

// maildirFiles returns the message files in dir/cur keyed by name.
func maildirFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "cur"))
	if err != nil {
		t.Fatal(err)
	}
	files := make(map[string]string)
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, "cur", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		files[e.Name()] = string(data)
	}
	return files
}

func assertFiles(t *testing.T, dir string, want map[string]string) {
	t.Helper()
	got := maildirFiles(t, dir)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s/cur:\n got %q\nwant %q", dir, got, want)
	}
}

func assertModTime(t *testing.T, path string, want time.Time) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(want) {
		t.Fatalf("%s mtime = %v, want INTERNALDATE %v", path, info.ModTime(), want)
	}
}

func assertNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("%s still exists (err=%v)", path, err)
	}
}

func TestBackupMirrorsAccountIncrementally(t *testing.T) {
	a := newTestAccount()
	addr := a.serve(t)
	root := filepath.Join(t.TempDir(), "backup")
	inbox := filepath.Join(root, "INBOX")

	// First run: everything is new.
	mustBackup(t, addr, root)
	assertFiles(t, inbox, map[string]string{
		"V1000.U1:2,S": msg("one"),
		"V1000.U2:2,F": msg("two"),
		"V1000.U3:2,":  msg("three"),
	})
	assertFiles(t, filepath.Join(root, "Archive", "2024"), map[string]string{"V2024.U1:2,RS": msg("archived")})
	assertFiles(t, filepath.Join(root, "%63ur"), map[string]string{"V5.U1:2,": msg("reserved name")})
	assertFiles(t, filepath.Join(root, "Empty"), map[string]string{})
	assertModTime(t, filepath.Join(inbox, "cur", "V1000.U2:2,F"), day2)
	assertModTime(t, filepath.Join(inbox, "cur", "V1000.U3:2,"), day1)
	if _, err := os.Stat(filepath.Join(root, "Archive", mailboxStateName)); !os.IsNotExist(err) {
		t.Fatalf("a \\Noselect mailbox must not get a Maildir: %v", err)
	}
	state, err := readMailboxState(inbox)
	if err != nil {
		t.Fatal(err)
	}
	if state.Mailbox != "INBOX" || state.UIDValidity != 1000 || !reflect.DeepEqual(state.Keywords, map[imap.UID][]string{2: {"$Forwarded"}}) {
		t.Fatalf("INBOX state = %+v", state)
	}
	unchanged, err := os.Stat(filepath.Join(inbox, "cur", "V1000.U3:2,"))
	if err != nil {
		t.Fatal(err)
	}

	// Second run: one new message, one flag change, one expunge, one mailbox
	// deleted on the server, one mailbox renumbered.
	a.deliver("INBOX", day2, msg("four"))
	a.setFlags("INBOX", 1, `\Seen`, `\Answered`)
	a.expunge("INBOX", 2)
	a.removeMailbox("Archive/2024")
	a.removeMailbox("Archive")
	a.renumber("cur", 6)
	out := mustBackup(t, addr, root)
	assertFiles(t, inbox, map[string]string{
		"V1000.U1:2,RS": msg("one"),
		"V1000.U3:2,":   msg("three"),
		"V1000.U4:2,":   msg("four"),
	})
	assertFiles(t, filepath.Join(root, "%63ur"), map[string]string{"V6.U1:2,": msg("reserved name")})
	assertNotExist(t, filepath.Join(root, "Archive"))
	if !strings.Contains(out, "INBOX: 3 messages, 1 new, 1 flags changed, 1 removed") {
		t.Fatalf("second run report:\n%s", out)
	}
	if !strings.Contains(out, "1 mailboxes pruned, 0 failed") {
		t.Fatalf("second run summary:\n%s", out)
	}
	again, err := os.Stat(filepath.Join(inbox, "cur", "V1000.U3:2,"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(unchanged, again) {
		t.Fatal("an unchanged message was rewritten; restic would see a new file")
	}
	state, err = readMailboxState(inbox)
	if err != nil {
		t.Fatal(err)
	}
	if state.Keywords != nil {
		t.Fatalf("keywords of an expunged message survived: %+v", state.Keywords)
	}

	// Third run: nothing to do.
	out = mustBackup(t, addr, root)
	if !strings.Contains(out, "backup: 3 mailboxes, 4 messages, 0 new, 0 flags changed, 0 removed, 0 mailboxes pruned, 0 failed") {
		t.Fatalf("no-op run summary:\n%s", out)
	}
	if f := a.forbiddenCommands(); len(f) != 0 {
		t.Fatalf("backup sent commands that are not read-only: %q", f)
	}
}

func TestBackupUIDAfterBody(t *testing.T) {
	a := newTestAccount()
	a.uidAfterBody = true
	addr := a.serve(t)
	root := t.TempDir()

	mustBackup(t, addr, root)
	inbox := filepath.Join(root, "INBOX")
	assertFiles(t, inbox, map[string]string{
		"V1000.U1:2,S": msg("one"),
		"V1000.U2:2,F": msg("two"),
		"V1000.U3:2,":  msg("three"),
	})
	assertModTime(t, filepath.Join(inbox, "cur", "V1000.U2:2,F"), day2)
}

func TestBackupManyMessagesSpanBatches(t *testing.T) {
	a := &fakeAccount{}
	a.addMailbox("INBOX", 1)
	n := fetchBatchSize*2 + 3
	for i := 0; i < n; i++ {
		a.deliver("INBOX", day1, msg(strings.Repeat("x", i%7)))
	}
	addr := a.serve(t)
	root := t.TempDir()
	out := mustBackup(t, addr, root)
	if got := len(maildirFiles(t, filepath.Join(root, "INBOX"))); got != n {
		t.Fatalf("stored %d messages, want %d\n%s", got, n, out)
	}
}

func TestBackupFailureSkipsPruning(t *testing.T) {
	a := newTestAccount()
	addr := a.serve(t)
	root := t.TempDir()
	mustBackup(t, addr, root)

	a.removeMailbox("Empty")
	a.mu.Lock()
	a.failExamine = map[string]bool{"cur": true}
	a.mu.Unlock()
	a.deliver("INBOX", day2, msg("four"))
	code, stdout, stderr := runBackupCLI(t, addr, root)
	if code != 1 {
		t.Fatalf("exit = %d, want 1\nstderr:\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "backup: cur: examine:") {
		t.Fatalf("stderr does not name the failed mailbox:\n%s", stderr)
	}
	if _, err := os.Stat(filepath.Join(root, "Empty", mailboxStateName)); err != nil {
		t.Fatalf("a run with a failure pruned a mailbox: %v", err)
	}
	// The mailboxes that worked were still backed up.
	if _, ok := maildirFiles(t, filepath.Join(root, "INBOX"))["V1000.U4:2,"]; !ok {
		t.Fatalf("INBOX was not backed up after another mailbox failed\n%s", stdout)
	}
}

func TestBackupRefusesMailboxCollision(t *testing.T) {
	a := newTestAccount()
	addr := a.serve(t)
	root := t.TempDir()
	mustBackup(t, addr, root)

	// Stand in for a case-insensitive file system: the directory "Other" maps
	// to already holds a different mailbox.
	other := filepath.Join(root, "Other")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeMailboxState(other, mailboxState{Version: 1, Mailbox: "other", UIDValidity: 1}); err != nil {
		t.Fatal(err)
	}
	a.addMailbox("Other", 1)
	a.deliver("Other", day1, msg("collides"))
	code, _, stderr := runBackupCLI(t, addr, root)
	if code != 1 || !strings.Contains(stderr, `already holds mailbox "other"`) {
		t.Fatalf("exit = %d, want 1 with a collision error\nstderr:\n%s", code, stderr)
	}
	if files := maildirFilesIfAny(t, other); len(files) != 0 {
		t.Fatalf("collision wrote messages: %q", files)
	}
}

func maildirFilesIfAny(t *testing.T, dir string) map[string]string {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, "cur")); os.IsNotExist(err) {
		return nil
	}
	return maildirFiles(t, dir)
}

func TestBackupExclude(t *testing.T) {
	a := newTestAccount()
	addr := a.serve(t)
	root := t.TempDir()
	mustBackup(t, addr, root)
	if _, err := os.Stat(filepath.Join(root, "Archive", "2024")); err != nil {
		t.Fatal(err)
	}

	_, stdout, stderr := runBackupCLI(t, addr, root, "-exclude", "Arch*", "-exclude", "Empty")
	if !strings.Contains(stderr, "skipping Archive/2024 (excluded)") {
		t.Fatalf("stderr:\n%s", stderr)
	}
	assertNotExist(t, filepath.Join(root, "Archive"))
	assertNotExist(t, filepath.Join(root, "Empty"))
	if !strings.Contains(stdout, "2 mailboxes pruned") {
		t.Fatalf("stdout:\n%s", stdout)
	}
}

func TestBackupLeavesForeignFiles(t *testing.T) {
	a := newTestAccount()
	addr := a.serve(t)
	root := t.TempDir()
	mustBackup(t, addr, root)

	notes := filepath.Join(root, "Empty", "cur", "notes.txt")
	if err := os.WriteFile(notes, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.removeMailbox("Empty")
	mustBackup(t, addr, root)
	if _, err := os.Stat(notes); err != nil {
		t.Fatalf("pruning removed a file the tool did not write: %v", err)
	}
	assertNotExist(t, filepath.Join(root, "Empty", mailboxStateName))
}

func TestBackupDirectoryChecks(t *testing.T) {
	a := newTestAccount()
	addr := a.serve(t)

	t.Run("not empty", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "x"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		code, _, stderr := runBackupCLI(t, addr, root)
		if code != 1 || !strings.Contains(stderr, "is not empty and is not an imapcli backup") {
			t.Fatalf("exit = %d\nstderr:\n%s", code, stderr)
		}
	})
	t.Run("other account", func(t *testing.T) {
		root := t.TempDir()
		mustBackup(t, addr, root)
		marker := `{"version": 1, "host": "127.0.0.1", "user": "bob"}`
		if err := os.WriteFile(filepath.Join(root, accountMarkerName), []byte(marker), 0o600); err != nil {
			t.Fatal(err)
		}
		code, _, stderr := runBackupCLI(t, addr, root)
		if code != 1 || !strings.Contains(stderr, "holds the backup of bob on 127.0.0.1") {
			t.Fatalf("exit = %d\nstderr:\n%s", code, stderr)
		}
	})
	t.Run("locked", func(t *testing.T) {
		root := t.TempDir()
		unlock, err := lockFile(filepath.Join(root, lockFileName))
		if err != nil {
			t.Fatal(err)
		}
		defer unlock()
		code, _, stderr := runBackupCLI(t, addr, root)
		if code != 1 || !strings.Contains(stderr, "another imapcli backup is running") {
			t.Fatalf("exit = %d\nstderr:\n%s", code, stderr)
		}
	})
}

func TestBackupFlagValidation(t *testing.T) {
	clearIMAPEnv(t)
	base := []string{"-addr", "127.0.0.1:1", "-user", "u", "-pass", "p", "-plain"}
	tests := []struct {
		args []string
		err  string
	}{
		{[]string{"-backup", "x", "-mailbox", "INBOX"}, "-backup cannot be combined"},
		{[]string{"-backup", "x", "-subscribed"}, "-backup cannot be combined"},
		{[]string{"-exclude", "Junk"}, "-exclude requires -backup"},
		{[]string{"-pass-file", "f"}, "use only one of -pass, -pass-file and -pass-cmd"},
	}
	for _, tt := range tests {
		var stdout, stderr bytes.Buffer
		args := append(append([]string(nil), base...), tt.args...)
		if tt.err == "use only one of -pass, -pass-file and -pass-cmd" {
			args = append(args, "-backup", t.TempDir())
		}
		if code := runIO(args, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), tt.err) {
			t.Errorf("%q: exit = %d, stderr = %q; want 2 and %q", tt.args, code, stderr.String(), tt.err)
		}
	}
}

func TestPasswordAuthOptions(t *testing.T) {
	tests := []struct {
		caps string
		want string // "" means imapclient's own choice
	}{
		{"AUTH=OAUTHBEARER AUTH=PLAIN", "PLAIN"},
		{"AUTH=XOAUTH2 AUTH=LOGIN", "LOGIN"},
		{"AUTH=OAUTHBEARER AUTH=CRAM-MD5 AUTH=PLAIN", "CRAM-MD5"},
		{"AUTH=SCRAM-SHA-256 AUTH=OAUTHBEARER AUTH=PLAIN", ""},
		{"AUTH=OAUTHBEARER", ""},
	}
	for _, tt := range tests {
		caps := make(map[string]bool)
		for _, c := range strings.Fields(tt.caps) {
			caps[c] = true
		}
		got := ""
		if opts := passwordAuthOptions(caps); opts != nil {
			got = opts.Mechanism
		}
		if got != tt.want {
			t.Errorf("passwordAuthOptions(%s) = %q, want %q", tt.caps, got, tt.want)
		}
	}
}

// TestBackupPrefersPasswordOverOAuth is the Stalwart case: a server offering
// OAUTHBEARER and PLAIN must get the password through PLAIN.
func TestBackupPrefersPasswordOverOAuth(t *testing.T) {
	a := newTestAccount()
	a.extraCaps = "AUTH=OAUTHBEARER"
	addr := a.serve(t)
	mustBackup(t, addr, t.TempDir())
}
