// Command imapcli is a read-only IMAP client for browsing folders and messages.
//
// It connects, authenticates, and either lists mailboxes as a tree, lists
// messages in a mailbox, or prints one message by UID. Mailboxes are opened
// with EXAMINE and message bodies with BODY.PEEK[], so the session does not
// mutate server state (no \Seen, no STORE/APPEND/CREATE/DELETE/…).
//
//	imapcli -addr mail.example:993 -tls -user alice
//	imapcli -addr mail.example:993 -tls -user alice -mailbox INBOX
//	imapcli -addr mail.example:993 -tls -user alice -mailbox INBOX -limit 100
//	imapcli -addr mail.example:993 -tls -user alice -mailbox INBOX -limit 0
//	imapcli -addr mail.example:993 -tls -user alice -mailbox INBOX -uid 42
//
// Password may be passed with -pass / IMAP_PASS, or entered interactively
// (no echo) when omitted.
//
// Environment variables are used when the matching flag is omitted:
// IMAP_ADDR, IMAP_USER, IMAP_PASS, IMAP_TLS, IMAP_STARTTLS, IMAP_INSECURE.
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
	pass := fs.String("pass", envOr("IMAP_PASS", ""), "password, visible in the process list (prefer IMAP_PASS or the prompt); prompt if omitted")
	useTLS := fs.Bool("tls", envBool("IMAP_TLS"), "use implicit TLS (DialTLS); or IMAP_TLS=1")
	useStartTLS := fs.Bool("starttls", envBool("IMAP_STARTTLS"), "upgrade with STARTTLS; or IMAP_STARTTLS=1")
	plain := fs.Bool("plain", false, "cleartext dial (local/test servers only; implies insecure auth)")
	insecure := fs.Bool("insecure", envBool("IMAP_INSECURE"), "skip TLS verify (test servers only); or IMAP_INSECURE=1")
	subscribed := fs.Bool("subscribed", false, "list subscribed mailboxes only")
	mailbox := fs.String("mailbox", "", "mailbox to open read-only (EXAMINE)")
	limit := fs.Uint("limit", 50, "max recent messages to list (0 = all); ignored with -uid")
	offset := fs.Uint("offset", 0, "skip this many newest messages before -limit")
	uid := fs.Uint("uid", 0, "print one message by UID (requires -mailbox); uses BODY.PEEK[]")
	timeout := fs.Duration("timeout", 2*time.Minute, "overall operation timeout")

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

	password, err := resolvePassword(*pass)
	if err != nil {
		fmt.Fprintf(stderr, "imapcli: %v\n", err)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	opts := &imapclient.Options{
		InsecureSkipVerify: *insecure,
		AllowInsecureAuth:  *plain,
	}
	fmt.Fprintf(stderr, "imapcli: connecting to %s...\n", *addr)
	client, err := dial(ctx, *addr, *useTLS, *useStartTLS, *plain, opts)
	if err != nil {
		fmt.Fprintf(stderr, "imapcli: dial: %v\n", err)
		return 1
	}
	defer client.Close()

	fmt.Fprintf(stderr, "imapcli: authenticating as %s...\n", *user)
	if err := client.Authenticate(ctx, *user, password, nil); err != nil {
		fmt.Fprintf(stderr, "imapcli: auth: %v\n", err)
		return 1
	}

	if *mailbox != "" {
		if err := openMailbox(ctx, client, mailboxOpts{
			Name:   *mailbox,
			Limit:  *limit,
			Offset: *offset,
			UID:    *uid,
		}, stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "imapcli: mailbox: %v\n", err)
			return 1
		}
	} else {
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
