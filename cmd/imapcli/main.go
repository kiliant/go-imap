// Command imapcli is a read-only IMAP client for browsing and backing up
// folders and messages.
//
// It connects, authenticates, and either lists mailboxes as a tree, lists
// messages in a mailbox, prints one message by UID, or mirrors the whole
// account to a local directory. Mailboxes are opened with EXAMINE and message
// bodies with BODY.PEEK[], so the session does not mutate server state (no
// \Seen, no STORE/APPEND/CREATE/DELETE/…).
//
//	imapcli -addr mail.example:993 -tls -user alice
//	imapcli -addr mail.example:993 -tls -user alice -mailbox INBOX
//	imapcli -addr mail.example:993 -tls -user alice -mailbox INBOX -limit 100
//	imapcli -addr mail.example:993 -tls -user alice -mailbox INBOX -limit 0
//	imapcli -addr mail.example:993 -tls -user alice -mailbox INBOX -uid 42
//	imapcli -addr mail.example:993 -tls -user alice -backup ~/MailBackup/alice
//
// The password is taken from exactly one of -pass / IMAP_PASS, -pass-file /
// IMAP_PASS_FILE (the file's first line) or -pass-cmd / IMAP_PASS_CMD (the
// first line a /bin/sh command prints), or entered interactively (no echo)
// when none is given.
//
// Environment variables are used when the matching flag is omitted:
// IMAP_ADDR, IMAP_USER, IMAP_PASS, IMAP_PASS_FILE, IMAP_PASS_CMD, IMAP_TLS,
// IMAP_STARTTLS, IMAP_INSECURE.
//
// # Backup
//
// -backup DIR keeps DIR an exact copy of every selectable mailbox, in Maildir
// format, for a file-level backup tool such as restic to snapshot:
//
//	DIR/.imapcli-backup.json       which account DIR belongs to
//	DIR/.imapcli-backup.lock       held while a backup runs
//	DIR/INBOX/.imapcli-mailbox.json  mailbox name, UIDVALIDITY, keywords
//	DIR/INBOX/{cur,new,tmp}/
//	DIR/INBOX/cur/V<uidvalidity>.U<uid>:2,<flags>
//	DIR/Archive/2024/…               one directory per hierarchy level
//
// Each message is stored once, byte for byte as the server sends BODY[], with
// its INTERNALDATE as the file's modification time. Later runs download only
// new messages, rename files whose flags changed, and delete files for
// messages and mailboxes that are gone from the server, so the directory is a
// mirror and the backup tool's snapshots are the history. The flags D, F, R, S
// and T in a file name stand for \Draft, \Flagged, \Answered, \Seen and
// \Deleted; every other flag is kept in the mailbox's .imapcli-mailbox.json.
//
// A message is written to tmp/ and renamed into cur/ only once complete, so an
// interrupted run never leaves a partial message behind. Mailboxes that no
// longer exist are pruned only after a run in which every mailbox succeeded.
// DIR must be new, empty, or an earlier backup of the same account.
//
// -exclude PATTERN (repeatable) skips mailboxes whose full name matches;
// '*' matches any run of characters, the hierarchy delimiter included, and '?'
// one character. A skipped mailbox is not part of the backup, so an existing
// local copy of it is pruned.
//
// In backup mode -timeout defaults to none, since a first backup can take
// hours; a connection that stops delivering data still fails after five
// minutes. The exit status is 0 when every mailbox was backed up, 1 when
// anything failed, and 2 for a usage error.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/kiliant/go-imap/imapclient"
)

// backupReadTimeout bounds how long a backup waits for a server that has
// stopped sending. Every command in a backup streams data steadily, so silence
// this long means a dead connection, not a slow one.
const backupReadTimeout = 5 * time.Minute

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	return runIO(args, os.Stdout, os.Stderr)
}

func runIO(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("imapcli", flag.ContinueOnError)
	fs.SetOutput(stderr)

	addr := fs.String("addr", envOr("IMAP_ADDR", ""), "server address (host:port); or IMAP_ADDR")
	user := fs.String("user", envOr("IMAP_USER", ""), "username; or IMAP_USER")
	pass := fs.String("pass", envOr("IMAP_PASS", ""), "password, visible in the process list (prefer -pass-file, -pass-cmd or the prompt); or IMAP_PASS")
	passFile := fs.String("pass-file", envOr("IMAP_PASS_FILE", ""), "read the password from the first line of this file; or IMAP_PASS_FILE")
	passCmd := fs.String("pass-cmd", envOr("IMAP_PASS_CMD", ""), "use the first line printed by this /bin/sh command as the password; or IMAP_PASS_CMD")
	useTLS := fs.Bool("tls", envBool("IMAP_TLS"), "use implicit TLS (DialTLS); or IMAP_TLS=1")
	useStartTLS := fs.Bool("starttls", envBool("IMAP_STARTTLS"), "upgrade with STARTTLS; or IMAP_STARTTLS=1")
	plain := fs.Bool("plain", false, "cleartext dial (local/test servers only; implies insecure auth)")
	insecure := fs.Bool("insecure", envBool("IMAP_INSECURE"), "skip TLS verify (test servers only); or IMAP_INSECURE=1")
	subscribed := fs.Bool("subscribed", false, "list subscribed mailboxes only")
	mailbox := fs.String("mailbox", "", "mailbox to open read-only (EXAMINE)")
	limit := fs.Uint("limit", 50, "max recent messages to list (0 = all); ignored with -uid")
	offset := fs.Uint("offset", 0, "skip this many newest messages before -limit")
	uid := fs.Uint("uid", 0, "print one message by UID (requires -mailbox); uses BODY.PEEK[]")
	backup := fs.String("backup", "", "mirror every mailbox into this directory as Maildir (read-only on the server)")
	var excludes globList
	fs.Var(&excludes, "exclude", "with -backup: skip mailboxes matching this pattern (* and ?); repeatable")
	compress := fs.Bool("compress", true, "with -backup: use COMPRESS=DEFLATE when the server offers it")
	timeout := fs.Duration("timeout", 2*time.Minute, "overall operation timeout, 0 for none (default none with -backup)")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *addr == "" || *user == "" {
		fmt.Fprintln(stderr, "imapcli: -addr and -user are required (or IMAP_ADDR / IMAP_USER)")
		fs.Usage()
		return 2
	}
	if *uid != 0 && *mailbox == "" {
		fmt.Fprintln(stderr, "imapcli: -uid requires -mailbox")
		return 2
	}
	if *backup != "" && (*mailbox != "" || *subscribed) {
		fmt.Fprintln(stderr, "imapcli: -backup cannot be combined with -mailbox, -uid or -subscribed")
		return 2
	}
	if *backup == "" && len(excludes) > 0 {
		fmt.Fprintln(stderr, "imapcli: -exclude requires -backup")
		return 2
	}
	modes := 0
	if *useTLS {
		modes++
	}
	if *useStartTLS {
		modes++
	}
	if *plain {
		modes++
	}
	if modes != 1 {
		fmt.Fprintln(stderr, "imapcli: exactly one of -tls, -starttls, or -plain is required")
		return 2
	}
	timeoutSet := false
	fs.Visit(func(f *flag.Flag) { timeoutSet = timeoutSet || f.Name == "timeout" })
	if *backup != "" && !timeoutSet {
		*timeout = 0
	}

	// Lock the backup directory before asking for a password, so a second run
	// fails at once instead of after a prompt nobody is there to answer.
	var bd *backupDir
	if *backup != "" {
		var err error
		if bd, err = openBackupDir(*backup, *addr, *user); err != nil {
			fmt.Fprintf(stderr, "imapcli: backup: %v\n", err)
			return 1
		}
		defer bd.Close()
	}

	password, err := resolvePassword(passwordSource{Value: *pass, File: *passFile, Command: *passCmd})
	if err != nil {
		fmt.Fprintf(stderr, "imapcli: %v\n", err)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if *timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *timeout)
		defer cancel()
	}

	opts := &imapclient.Options{
		InsecureSkipVerify: *insecure,
		AllowInsecureAuth:  *plain,
	}
	if bd != nil {
		opts.ReadTimeout = backupReadTimeout
	}
	fmt.Fprintf(stderr, "imapcli: connecting to %s...\n", *addr)
	client, err := dial(ctx, *addr, *useTLS, *useStartTLS, *plain, opts)
	if err != nil {
		fmt.Fprintf(stderr, "imapcli: dial: %v\n", err)
		return 1
	}
	defer client.Close()

	fmt.Fprintf(stderr, "imapcli: authenticating as %s...\n", *user)
	if err := authenticate(ctx, client, *user, password); err != nil {
		fmt.Fprintf(stderr, "imapcli: auth: %v\n", err)
		return 1
	}

	switch {
	case bd != nil:
		if *compress && client.Supports("COMPRESS=DEFLATE") {
			if err := client.Compress(ctx, nil); err != nil {
				fmt.Fprintf(stderr, "imapcli: compress: %v (continuing uncompressed)\n", err)
			}
		}
		if err := runBackup(ctx, client, bd, backupOpts{Exclude: compileGlobs(excludes)}, stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "imapcli: backup: %v\n", err)
			return 1
		}
	case *mailbox != "":
		if err := openMailbox(ctx, client, mailboxOpts{
			Name:   *mailbox,
			Limit:  *limit,
			Offset: *offset,
			UID:    *uid,
		}, stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "imapcli: mailbox: %v\n", err)
			return 1
		}
	default:
		fmt.Fprintln(stderr, "imapcli: listing mailboxes...")
		var listCmd *imapclient.ListCommand
		if *subscribed {
			// Lsub picks LIST (SUBSCRIBED) on LIST-EXTENDED/rev2 servers and
			// legacy LSUB elsewhere.
			listCmd = client.Lsub("", "*", nil)
		} else {
			listCmd = client.List("", "*", nil)
		}
		mailboxes, err := listCmd.Wait(ctx)
		if err != nil {
			fmt.Fprintf(stderr, "imapcli: list: %v\n", err)
			return 1
		}
		if err := writeFolderTree(stdout, buildFolderTree(mailboxes)); err != nil {
			fmt.Fprintf(stderr, "imapcli: write: %v\n", err)
			return 1
		}
	}

	logoutCtx, logoutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer logoutCancel()
	if err := client.Logout(logoutCtx, nil); err != nil {
		fmt.Fprintf(stderr, "imapcli: logout: %v\n", err)
		return 1
	}
	return 0
}

func dial(ctx context.Context, addr string, useTLS, useStartTLS, plain bool, opts *imapclient.Options) (*imapclient.Client, error) {
	switch {
	case useTLS:
		return imapclient.DialTLS(ctx, addr, opts)
	case useStartTLS:
		return imapclient.DialStartTLS(ctx, addr, opts)
	case plain:
		return imapclient.Dial(ctx, addr, opts)
	default:
		return nil, fmt.Errorf("exactly one of -tls, -starttls, or -plain is required")
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envBool(key string) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	return v == "1" || v == "true" || v == "yes"
}
