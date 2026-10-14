//go:build interop

// Package imapcli runs the real cmd/imapcli binary against live servers. It is
// the end-to-end check of the backup mode: the binary is built once, pointed
// at each server with a password file, and the resulting Maildir is compared
// with what the server reports through the production client.
//
// It owns Dovecot and Stalwart only — the servers behind the backup mode's
// first two real accounts (mailbox.org and a self-hosted Stalwart) — so the
// rest of the matrix is not started for it.
package imapcli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/kiliant/go-imap/interop/definition"
	"github.com/kiliant/go-imap/interop/harness"
	"github.com/kiliant/go-imap/interop/servers/dovecot"
	"github.com/kiliant/go-imap/interop/servers/stalwart"
)

// binary is the path of the imapcli executable built for this run.
var binary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "imapcli-interop-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	binary = filepath.Join(dir, "imapcli")
	build := exec.Command("go", "build", "-o", binary, "github.com/kiliant/go-imap/cmd/imapcli")
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "build imapcli: %v\n", err)
		_ = os.RemoveAll(dir)
		os.Exit(1)
	}
	code := harness.Run(m, []definition.Profile{dovecot.Profile, stalwart.Profile})
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
