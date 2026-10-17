package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapclient"
)

// fetchBatchSize bounds one UID FETCH of message bodies. It keeps a failure
// cheap to drain and progress visible, without costing a round trip per
// message.
const fetchBatchSize = 200

var errLocked = errors.New("another imapcli backup is running on this directory")

// backupDir is an opened backup directory, locked for this process.
type backupDir struct {
	path   string
	unlock func() error
}

// openBackupDir creates or opens path, locks it, and checks that it belongs to
// user on the host in addr. A directory that is neither empty nor an existing
// backup is refused, so a mistyped path cannot litter or prune anything.
func openBackupDir(path, addr, user string) (*backupDir, error) {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return nil, err
	}
	unlock, err := lockFile(filepath.Join(path, lockFileName))
	if err != nil {
		if errors.Is(err, errLocked) {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	bd := &backupDir{path: path, unlock: unlock}
	if err := bd.checkAccount(addrHost(addr), user); err != nil {
		_ = bd.Close()
		return nil, err
	}
	return bd, nil
}

func (bd *backupDir) checkAccount(host, user string) error {
	markerPath := filepath.Join(bd.path, accountMarkerName)
	data, err := os.ReadFile(markerPath)
	if err == nil {
		var got accountMarker
		if err := json.Unmarshal(data, &got); err != nil {
			return fmt.Errorf("%s: %w", markerPath, err)
		}
		if !strings.EqualFold(got.Host, host) || got.User != user {
			return fmt.Errorf("%s holds the backup of %s on %s, not %s on %s", bd.path, got.User, got.Host, user, host)
		}
		return nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	entries, err := os.ReadDir(bd.path)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name() != lockFileName {
			return fmt.Errorf("%s is not empty and is not an imapcli backup; use a new or empty directory", bd.path)
		}
	}
	marker, err := json.MarshalIndent(accountMarker{Version: 1, Host: host, User: user}, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(markerPath, append(marker, '\n'))
}

// Close releases the lock.
func (bd *backupDir) Close() error {
	if bd.unlock == nil {
		return nil
	}
	err := bd.unlock()
	bd.unlock = nil
	return err
}

func addrHost(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	return strings.ToLower(host)
}

// globList is a repeatable flag of mailbox-name patterns.
type globList []string

func (g *globList) String() string { return strings.Join(*g, ", ") }

func (g *globList) Set(v string) error {
	*g = append(*g, v)
	return nil
}

// compileGlobs turns patterns into anchored regular expressions in which '*'
// matches any run of characters, the hierarchy delimiter included, and '?'
// matches one character.
func compileGlobs(patterns []string) []*regexp.Regexp {
	out := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		var b strings.Builder
		b.WriteString(`(?s)^`)
		for _, r := range p {
			switch r {
			case '*':
				b.WriteString(`.*`)
			case '?':
				b.WriteString(`.`)
			default:
				b.WriteString(regexp.QuoteMeta(string(r)))
			}
		}
		b.WriteString(`$`)
		out = append(out, regexp.MustCompile(b.String()))
	}
	return out
}

// backupOpts configures one backup run.
type backupOpts struct {
	Exclude []*regexp.Regexp
}

// mailboxStats counts what one mailbox's sync did.
type mailboxStats struct {
	Messages int
	Added    int
	Renamed  int
	Removed  int
}

func (s *mailboxStats) add(o mailboxStats) {
	s.Messages += o.Messages
	s.Added += o.Added
	s.Renamed += o.Renamed
	s.Removed += o.Removed
}

// backupRun is the state of one backup over one connection.
type backupRun struct {
	root   string
	client *imapclient.Client
	out    io.Writer
	errOut io.Writer
}

// runBackup mirrors every selectable mailbox into bd. It keeps going after a
// mailbox fails, reports each failure on errOut, and returns an error if any
// did. Mailboxes that disappeared from the server are pruned only after a run
// in which nothing failed.
func runBackup(ctx context.Context, client *imapclient.Client, bd *backupDir, opts backupOpts, out, errOut io.Writer) error {
	fmt.Fprintln(errOut, "imapcli: listing mailboxes...")
	mailboxes, err := client.List("", "*", nil).Wait(ctx)
	if err != nil {
		return fmt.Errorf("list: %w", err)
	}
	sort.Slice(mailboxes, func(i, j int) bool { return mailboxes[i].Mailbox < mailboxes[j].Mailbox })

	run := &backupRun{root: bd.path, client: client, out: out, errOut: errOut}
	keep := make(map[string]string) // mailbox name → directory relative to the root
	var total mailboxStats
	failed, synced := 0, 0
	for _, m := range mailboxes {
		if m == nil || m.Mailbox == "" || keep[m.Mailbox] != "" {
			continue
		}
		if imap.ContainsAttr(m.Attrs, imap.MailboxAttrNoSelect) || imap.ContainsAttr(m.Attrs, imap.MailboxAttrNonExistent) {
			continue
		}
		if slices.ContainsFunc(opts.Exclude, func(re *regexp.Regexp) bool { return re.MatchString(m.Mailbox) }) {
			fmt.Fprintf(errOut, "imapcli: skipping %s (excluded)\n", m.Mailbox)
			continue
		}
		rel := mailboxPath(m.Mailbox, m.Delimiter)
		keep[m.Mailbox] = rel
		fmt.Fprintf(errOut, "imapcli: backing up %s...\n", m.Mailbox)
		st, err := run.syncMailbox(ctx, m.Mailbox, rel)
		total.add(st)
		if err != nil {
			failed++
			fmt.Fprintf(errOut, "imapcli: backup: %s: %v\n", m.Mailbox, err)
			if ctx.Err() != nil || client.State() == imapclient.StateLogout {
				break // every later mailbox would fail the same way
			}
			continue
		}
		synced++
		fmt.Fprintf(out, "%s: %d messages, %d new, %d flags changed, %d removed\n",
			m.Mailbox, st.Messages, st.Added, st.Renamed, st.Removed)
	}

	pruned := 0
	if failed == 0 {
		pruned, err = pruneMailboxes(bd.path, keep)
		if err != nil {
			fmt.Fprintf(errOut, "imapcli: backup: prune: %v\n", err)
			failed++
		}
	}
	fmt.Fprintf(out, "backup: %d mailboxes, %d messages, %d new, %d flags changed, %d removed, %d mailboxes pruned, %d failed\n",
		synced, total.Messages, total.Added, total.Renamed, total.Removed, pruned, failed)
	if failed > 0 {
		return fmt.Errorf("%d mailbox(es) failed; the backup is incomplete", failed)
	}
	return nil
}

// syncMailbox brings the Maildir at root/rel up to date with mailbox name.
func (r *backupRun) syncMailbox(ctx context.Context, name, rel string) (mailboxStats, error) {
	var st mailboxStats
	dir := filepath.Join(r.root, rel)
	state, err := readMailboxState(dir)
	if err != nil {
		return st, err
	}
	if state.Mailbox != "" && state.Mailbox != name {
		return st, fmt.Errorf("%s already holds mailbox %q; the file system does not tell the two names apart", dir, state.Mailbox)
	}

	status, err := r.client.Examine(name, nil).Wait(ctx)
	if err != nil {
		return st, fmt.Errorf("examine: %w", err)
	}
	for _, sub := range maildirSubdirs {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			return st, err
		}
	}
	// Claim the directory before writing any message into it, so a colliding
	// mailbox later in this run is refused rather than merged.
	state.Version, state.Mailbox, state.UIDValidity = 1, name, status.UIDValidity
	if err := writeMailboxState(dir, state); err != nil {
		return st, err
	}
	if err := cleanTmp(dir); err != nil {
		return st, err
	}
	local, stale, err := scanMaildir(dir, status.UIDValidity)
	if err != nil {
		return st, err
	}

	server := make(map[imap.UID][]imap.Flag)
	if status.NumMessages > 0 {
		if server, err = listServerMessages(ctx, r.client); err != nil {
			return st, fmt.Errorf("list messages: %w", err)
		}
	}
	st.Messages = len(server)

	// Flag changes are renames: restic sees the same content under a new name
	// and stores nothing new.
	for uid, flags := range server {
		lm, ok := local[uid]
		if !ok {
			continue
		}
		want := filepath.Join("cur", messageFileName(status.UIDValidity, uid, flags))
		if lm.path == want {
			continue
		}
		if err := os.Rename(filepath.Join(dir, lm.path), filepath.Join(dir, want)); err != nil {
			return st, err
		}
		lm.path = want
		st.Renamed++
	}

	var missing []imap.UID
	for uid := range server {
		if _, ok := local[uid]; !ok {
			missing = append(missing, uid)
		}
	}
	slices.Sort(missing)
	for len(missing) > 0 {
		n := min(len(missing), fetchBatchSize)
		added, err := r.fetchMessages(ctx, name, dir, status.UIDValidity, missing[:n], server)
		st.Added += added
		if err != nil {
			return st, err
		}
		missing = missing[n:]
	}

	// Removal runs last and only after everything above succeeded: a message
	// whose replacement failed to download keeps its old copy for this run.
	for uid, lm := range local {
		if _, ok := server[uid]; ok {
			continue
		}
		if err := os.Remove(filepath.Join(dir, lm.path)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return st, err
		}
		st.Removed++
	}
	for _, p := range stale {
		if err := os.Remove(filepath.Join(dir, p)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return st, err
		}
		st.Removed++
	}

	state.Keywords = nil
	for uid, flags := range server {
		if extra := extraFlags(flags); len(extra) > 0 {
			if state.Keywords == nil {
				state.Keywords = make(map[imap.UID][]string)
			}
			state.Keywords[uid] = extra
		}
	}
	if err := writeMailboxState(dir, state); err != nil {
		return st, err
	}
	for _, sub := range []string{"cur", "new"} {
		if err := syncDir(filepath.Join(dir, sub)); err != nil {
			return st, err
		}
	}
	return st, nil
}

// listServerMessages returns every message in the selected mailbox with its flags.
// This listing is the authority for one run: what it omits is removed locally.
func listServerMessages(ctx context.Context, client *imapclient.Client) (map[imap.UID][]imap.Flag, error) {
	cmd := client.FetchUID(imap.UIDSet{{Start: 1}}, nil, imap.FetchItemUID, imap.FetchItemFlags)
	msgs := make(map[imap.UID][]imap.Flag)
	for {
		data, err := cmd.Next(ctx)
		if err == io.EOF {
			return msgs, nil
		}
		if err != nil {
			return nil, err
		}
		uid, ok := fetchUID(data)
		if !ok {
			continue // an unsolicited FETCH without a UID says nothing usable
		}
		flags, hasFlags := fetchFlags(data)
		if !hasFlags {
			if _, seen := msgs[uid]; seen {
				continue
			}
		}
		msgs[uid] = flags
	}
}

// pendingMessage is a body already on disk in tmp/ whose UID and INTERNALDATE
// are not read yet.
//
// FetchCommand.Next yields immutable portions when a response contains a
// streamed literal. The backup consumer combines those portions by sequence
// number, then finalises the message once the next message arrives or FETCH ends.
type pendingMessage struct {
	data    *imap.FetchMessageData
	tmpPath string
	size    int64
}

// fetchMessages downloads uids into dir and returns how many it stored. After
// a local failure it keeps reading, and discards, the rest of the command, so
// the connection stays usable for the next mailbox.
func (r *backupRun) fetchMessages(ctx context.Context, mailbox, dir string, uidValidity uint32, uids []imap.UID, server map[imap.UID][]imap.Flag) (int, error) {
	wanted := make(map[imap.UID]bool, len(uids))
	for _, uid := range uids {
		wanted[uid] = true
	}
	cmd := r.client.FetchUID(imap.UIDSetNum(uids...), nil,
		imap.FetchItemUID,
		imap.FetchItemInternalDate,
		imap.FetchItemRFC822Size,
		&imap.FetchItemBodySection{Peek: true},
	)

	received := make(map[imap.UID]bool, len(uids))
	var pending *pendingMessage
	var current *imap.FetchMessageData
	var firstErr error
	fail := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}
	finish := func() {
		if pending == nil {
			return
		}
		if err := r.storeMessage(mailbox, dir, uidValidity, pending, wanted, received, server); err != nil {
			fail(err)
		}
		pending = nil
	}
	for {
		data, err := cmd.Next(ctx)
		if err != nil {
			if err == io.EOF {
				finish()
			} else {
				// The command did not end cleanly, so the pending message may
				// still be missing metadata after its body.
				if pending != nil {
					_ = os.Remove(pending.tmpPath)
					pending = nil
				}
				fail(err)
			}
			break
		}
		if current == nil || current.SeqNum != data.SeqNum {
			finish()
			current = &imap.FetchMessageData{SeqNum: data.SeqNum, Items: make(map[imap.FetchDataKey][]imap.FetchData)}
		}
		for key, values := range data.Items {
			current.Items[key] = append(current.Items[key], values...)
		}
		body := wholeMessageBody(data)
		if body == nil {
			continue
		}
		if firstErr != nil {
			_ = closeLiteral(body.Literal)
			continue
		}
		if pending != nil {
			// Keep the first body while later portions supply its metadata.
			// A repeated body must not replace its only temp-file reference.
			if err := closeLiteral(body.Literal); err != nil {
				fail(err)
			}
			continue
		}
		p, err := receiveBody(dir, current, body)
		if err != nil {
			fail(err)
			continue
		}
		pending = p
	}

	if firstErr == nil && len(received) < len(wanted) {
		fmt.Fprintf(r.errOut, "imapcli: backup: %s: %d of %d requested messages were not returned (expunged meanwhile?)\n",
			mailbox, len(wanted)-len(received), len(wanted))
	}
	if err := syncDir(filepath.Join(dir, "cur")); err != nil {
		fail(err)
	}
	return len(received), firstErr
}

// receiveBody streams one message body into a new file in dir/tmp.
func receiveBody(dir string, data *imap.FetchMessageData, body *imap.FetchDataBodySection) (*pendingMessage, error) {
	f, err := os.CreateTemp(filepath.Join(dir, "tmp"), tmpPrefix+"*")
	if err != nil {
		_ = closeLiteral(body.Literal)
		return nil, err
	}
	n, err := io.Copy(f, body.Literal)
	if closeErr := closeLiteral(body.Literal); err == nil {
		err = closeErr
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(f.Name())
		return nil, err
	}
	return &pendingMessage{data: data, tmpPath: f.Name(), size: n}, nil
}

// storeMessage moves a received body from tmp/ to its final name in cur/,
// with the message's INTERNALDATE as its modification time.
func (r *backupRun) storeMessage(mailbox, dir string, uidValidity uint32, p *pendingMessage, wanted, received map[imap.UID]bool, server map[imap.UID][]imap.Flag) error {
	uid, ok := fetchUID(p.data)
	if !ok || !wanted[uid] || received[uid] {
		_ = os.Remove(p.tmpPath)
		if !ok {
			return errors.New("server sent a message body without its UID")
		}
		if !wanted[uid] {
			return fmt.Errorf("server sent UID %d, which was not requested", uid)
		}
		return nil // a repeated body for a UID already stored
	}
	if t, ok := fetchInternalDate(p.data); ok {
		if err := os.Chtimes(p.tmpPath, t, t); err != nil {
			_ = os.Remove(p.tmpPath)
			return err
		}
	}
	if size, ok := fetchRFC822Size(p.data); ok && size != p.size {
		fmt.Fprintf(r.errOut, "imapcli: backup: %s: UID %d: server reported RFC822.SIZE %d, BODY[] had %d octets\n",
			mailbox, uid, size, p.size)
	}
	dest := filepath.Join(dir, "cur", messageFileName(uidValidity, uid, server[uid]))
	if err := os.Rename(p.tmpPath, dest); err != nil {
		_ = os.Remove(p.tmpPath)
		return err
	}
	received[uid] = true
	return nil
}

// pruneMailboxes removes the local copy of every mailbox not in keep, and the
// directories that leaves empty. It removes only what this tool wrote.
func pruneMailboxes(root string, keep map[string]string) (int, error) {
	var stale []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() || path == root {
			return nil
		}
		if slices.Contains(maildirSubdirs[:], d.Name()) {
			return fs.SkipDir
		}
		st, err := readMailboxState(path)
		if err != nil {
			return err
		}
		if st.Mailbox == "" {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if keep[st.Mailbox] != rel {
			stale = append(stale, path)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	for _, dir := range stale {
		if err := removeMailboxCopy(dir); err != nil {
			return 0, err
		}
		for d := dir; d != root && strings.HasPrefix(d, root); d = filepath.Dir(d) {
			if os.Remove(d) != nil {
				break // not empty: something else still lives here
			}
		}
	}
	return len(stale), nil
}

// removeMailboxCopy deletes the message files, temporary files, Maildir
// directories and state file this tool wrote into dir. Anything else stays.
func removeMailboxCopy(dir string) error {
	for _, sub := range maildirSubdirs {
		subdir := filepath.Join(dir, sub)
		entries, err := os.ReadDir(subdir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		for _, e := range entries {
			_, _, ours := parseMessageFileName(e.Name())
			if sub == "tmp" {
				ours = strings.HasPrefix(e.Name(), tmpPrefix)
			}
			if ours && e.Type().IsRegular() {
				if err := os.Remove(filepath.Join(subdir, e.Name())); err != nil {
					return err
				}
			}
		}
		_ = os.Remove(subdir) // fails, harmlessly, if anything foreign is left
	}
	return os.Remove(filepath.Join(dir, mailboxStateName))
}

// wholeMessageBody returns the BODY[] section of data, or nil.
func wholeMessageBody(data *imap.FetchMessageData) *imap.FetchDataBodySection {
	for _, values := range data.Items {
		for _, v := range values {
			if s, ok := v.(*imap.FetchDataBodySection); ok && len(s.Part) == 0 && s.Specifier == imap.PartSpecifierNone && !s.HasOrigin {
				return s
			}
		}
	}
	return nil
}

func closeLiteral(r io.Reader) error {
	if c, ok := r.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

func fetchUID(data *imap.FetchMessageData) (imap.UID, bool) {
	for _, values := range data.Items {
		for _, v := range values {
			if uid, ok := v.(imap.FetchDataUID); ok && uid != 0 {
				return imap.UID(uid), true
			}
		}
	}
	return 0, false
}

func fetchFlags(data *imap.FetchMessageData) ([]imap.Flag, bool) {
	for _, values := range data.Items {
		for _, v := range values {
			if flags, ok := v.(imap.FetchDataFlags); ok {
				return []imap.Flag(flags), true
			}
		}
	}
	return nil, false
}

func fetchInternalDate(data *imap.FetchMessageData) (t time.Time, ok bool) {
	for _, values := range data.Items {
		for _, v := range values {
			if d, isDate := v.(*imap.FetchDataInternalDate); isDate && d != nil && !d.Time.IsZero() {
				return d.Time, true
			}
		}
	}
	return t, false
}

func fetchRFC822Size(data *imap.FetchMessageData) (int64, bool) {
	for _, values := range data.Items {
		for _, v := range values {
			if size, ok := v.(imap.FetchDataRFC822Size); ok {
				return int64(size), true
			}
		}
	}
	return 0, false
}
