//go:build ignore

// A persistent IMAP server backed by Maildir, with polling diagnostics and
// orderly shutdown. It uses the same account and address settings as minimal.go.
// The default address, localhost:1143, is for local development; see tls.go for
// configuring TLS before accepting connections from other machines.
//
// Run from the imapserver directory:
//
//	IMAP_MAILDIR_ROOT=./maildir-data go run ./examples/maildir.go ./examples/config.go
//
// Stop with Ctrl-C. Restart with the same root and credentials to reopen the
// account's messages. Only one backend may own the root at a time.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"time"

	"github.com/kiliant/go-imap/imapserver"
	"github.com/kiliant/go-imap/imapserver/maildir"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := runMaildir(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runMaildir(ctx context.Context) (runErr error) {
	root := os.Getenv("IMAP_MAILDIR_ROOT")
	if root == "" {
		root = "./maildir-data"
	}
	backend, err := maildir.Open(ctx, root, &maildir.Options{
		Accounts: []maildir.AccountOptions{{Username: serverUser(), Password: serverPassword()}},
		OnPollFailure: func(_ context.Context, failure *maildir.PollFailure) {
			// Called outside backend locks. Return promptly and never call
			// backend.Close here: Close waits for this callback to return.
			log.Printf("Maildir account %q mailbox %q: %v", failure.Username, failure.Mailbox, failure.Err)
		},
	})
	if err != nil {
		return fmt.Errorf("open Maildir: %w", err)
	}
	defer func() {
		// Open's context controls construction only. Explicitly close the
		// backend after the server stops to release polling and the root lock.
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		runErr = errors.Join(runErr, backend.Close(closeCtx, nil))
	}()

	server := imapserver.New(backend, &imapserver.Options{
		Greeting:          "go-imap Maildir example server",
		AllowInsecureAuth: true,
	})
	defer func() {
		// Stop sessions before closing their storage. Shutdown uses a fresh
		// context because Ctrl-C has already canceled the serving context.
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		runErr = errors.Join(runErr, server.Close(closeCtx, nil))
	}()
	listener, err := net.Listen("tcp", serverAddr())
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer listener.Close()
	fmt.Printf("listening on %s as %s; Maildir root %s\n", listener.Addr(), serverUser(), root)
	if err := server.Serve(ctx, listener, nil); err != nil && ctx.Err() == nil {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}
