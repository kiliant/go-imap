package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/kiliant/go-imap"
)

// The on-disk layout of a backup. See the package documentation.
const (
	accountMarkerName = ".imapcli-backup.json"
	lockFileName      = ".imapcli-backup.lock"
	mailboxStateName  = ".imapcli-mailbox.json"

	// tmpPrefix marks the files this tool writes into a Maildir's tmp/
	// directory, so a later run can clean up after an interrupted one without
	// touching anything else.
	tmpPrefix = "imapcli-"
)

var maildirSubdirs = [...]string{"cur", "new", "tmp"}

// accountMarker records which account a backup directory belongs to, so two
// accounts can never be mirrored into, and pruned from, the same tree.
type accountMarker struct {
	Version int    `json:"version"`
	Host    string `json:"host"`
	User    string `json:"user"`
}

// mailboxState is the per-mailbox record next to its Maildir.
type mailboxState struct {
	Version int `json:"version"`
	// Mailbox is the exact server-side name. It is what detects two mailboxes
	// that a case- or normalisation-insensitive file system maps to the same
	// directory.
	Mailbox     string `json:"mailbox"`
	UIDValidity uint32 `json:"uidvalidity"`
	// Keywords holds, per UID, every flag the Maildir file name cannot carry:
	// keywords such as $Forwarded and any non-standard system flag.
	Keywords map[imap.UID][]string `json:"keywords,omitempty"`
}

// mailboxPath maps a mailbox name to a directory relative to the backup root:
// one path segment per level of the server's hierarchy, each made safe by
// escapeSegment.
func mailboxPath(name string, delim rune) string {
	segments := []string{name}
	if delim != 0 {
		segments = strings.Split(name, string(delim))
	}
	parts := make([]string, len(segments))
	for i, s := range segments {
		parts[i] = escapeSegment(s)
	}
	return filepath.Join(parts...)
}

// escapeSegment percent-escapes what a path segment cannot hold: '%' itself,
// '/', and control characters. A segment that would be hidden, be "." or "..",
// or collide with a Maildir subdirectory has its first character escaped too.
// An empty segment, which only a doubled delimiter produces, becomes a lone
// "%" — a form no other segment can take, because '%' is always escaped.
func escapeSegment(s string) string {
	if s == "" {
		return "%"
	}
	reserved := false
	for _, sub := range maildirSubdirs {
		if strings.EqualFold(s, sub) {
			reserved = true
		}
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		escape := c == '%' || c == '/' || c < 0x20 || c == 0x7f
		if i == 0 && (c == '.' || reserved) {
			escape = true
		}
		if escape {
			fmt.Fprintf(&b, "%%%02X", c)
		} else {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// maildirFlags maps the IMAP system flags with a Maildir info letter. The
// letters are in ASCII order, which is the order Maildir requires.
var maildirFlags = []struct {
	flag   imap.Flag
	letter byte
}{
	{imap.FlagDraft, 'D'},
	{imap.FlagFlagged, 'F'},
	{imap.FlagAnswered, 'R'},
	{imap.FlagSeen, 'S'},
	{imap.FlagDeleted, 'T'},
}

// maildirInfo returns the info letters for flags.
func maildirInfo(flags []imap.Flag) string {
	var b []byte
	for _, m := range maildirFlags {
		for _, f := range flags {
			if f.Equal(m.flag) {
				b = append(b, m.letter)
				break
			}
		}
	}
	return string(b)
}

// extraFlags returns the flags maildirInfo cannot represent, sorted. \Recent
// is a session flag and is never backed up.
func extraFlags(flags []imap.Flag) []string {
	var out []string
	for _, f := range flags {
		if f.Equal(imap.FlagRecent) {
			continue
		}
		mapped := false
		for _, m := range maildirFlags {
			if f.Equal(m.flag) {
				mapped = true
				break
			}
		}
		if !mapped {
			out = append(out, string(f))
		}
	}
	sort.Strings(out)
	return out
}

func messageFileName(uidValidity uint32, uid imap.UID, flags []imap.Flag) string {
	return fmt.Sprintf("V%d.U%d:2,%s", uidValidity, uid, maildirInfo(flags))
}

// parseMessageFileName reverses messageFileName. ok is false for any name this
// tool did not write.
func parseMessageFileName(name string) (uidValidity uint32, uid imap.UID, ok bool) {
	base, _, hasInfo := strings.Cut(name, ":2,")
	if !hasInfo || !strings.HasPrefix(base, "V") {
		return 0, 0, false
	}
	v, u, found := strings.Cut(base[1:], ".U")
	if !found {
		return 0, 0, false
	}
	vn, err := strconv.ParseUint(v, 10, 32)
	if err != nil {
		return 0, 0, false
	}
	un, err := strconv.ParseUint(u, 10, 32)
	if err != nil || un == 0 {
		return 0, 0, false
	}
	return uint32(vn), imap.UID(un), true
}

// localMessage is one message file found in a Maildir.
type localMessage struct {
	uid  imap.UID
	path string // relative to the mailbox directory, e.g. "cur/V1.U2:2,S"
}

// scanMaildir lists the message files this tool wrote into dir. Files for the
// current uidValidity are returned by UID; files for any other UIDVALIDITY,
// and duplicates of a UID already seen, are returned as stale.
func scanMaildir(dir string, uidValidity uint32) (current map[imap.UID]*localMessage, stale []string, err error) {
	current = make(map[imap.UID]*localMessage)
	for _, sub := range []string{"cur", "new"} {
		entries, err := os.ReadDir(filepath.Join(dir, sub))
		if err != nil {
			return nil, nil, err
		}
		for _, e := range entries {
			if !e.Type().IsRegular() {
				continue
			}
			v, uid, ok := parseMessageFileName(e.Name())
			if !ok {
				continue
			}
			rel := filepath.Join(sub, e.Name())
			if _, dup := current[uid]; v != uidValidity || dup {
				stale = append(stale, rel)
				continue
			}
			current[uid] = &localMessage{uid: uid, path: rel}
		}
	}
	return current, stale, nil
}

// cleanTmp removes files an interrupted run left in dir/tmp.
func cleanTmp(dir string) error {
	tmp := filepath.Join(dir, "tmp")
	entries, err := os.ReadDir(tmp)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Type().IsRegular() && strings.HasPrefix(e.Name(), tmpPrefix) {
			if err := os.Remove(filepath.Join(tmp, e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func readMailboxState(dir string) (mailboxState, error) {
	var st mailboxState
	data, err := os.ReadFile(filepath.Join(dir, mailboxStateName))
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return st, fmt.Errorf("%s: %w", filepath.Join(dir, mailboxStateName), err)
	}
	return st, nil
}

func writeMailboxState(dir string, st mailboxState) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, mailboxStateName), append(data, '\n'))
}

// writeFileAtomic replaces path with data so that a reader — restic included —
// sees either the old content or the new, never a partial file.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".imapcli-*.tmp")
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return err
	}
	return syncDir(dir)
}

// syncDir makes the renames and removals in dir durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if closeErr := d.Close(); err == nil {
		err = closeErr
	}
	return err
}
