package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunFlagValidation(t *testing.T) {
	var stdout, stderr bytes.Buffer
	tests := []struct {
		name string
		args []string
		want int
		err  string
	}{
		{name: "missing addr/user", args: nil, want: 2, err: "-addr and -user"},
		{name: "missing transport", args: []string{"-addr", "127.0.0.1:1", "-user", "u", "-pass", "p"}, want: 2, err: "exactly one of -tls"},
		{name: "tls and plain", args: []string{"-addr", "127.0.0.1:1", "-user", "u", "-pass", "p", "-tls", "-plain"}, want: 2, err: "exactly one of -tls"},
		{name: "uid without mailbox", args: []string{"-addr", "127.0.0.1:1", "-user", "u", "-pass", "p", "-plain", "-uid", "1"}, want: 2, err: "-uid requires -mailbox"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdout.Reset()
			stderr.Reset()
			got := runIO(tt.args, &stdout, &stderr)
			if got != tt.want {
				t.Fatalf("exit = %d, want %d; stderr=%q", got, tt.want, stderr.String())
			}
			if !strings.Contains(stderr.String(), tt.err) {
				t.Fatalf("stderr=%q, want substring %q", stderr.String(), tt.err)
			}
		})
	}
}

func TestRunListFolders(t *testing.T) {
	clearIMAPEnv(t)
	addr, closeFn := startFakeIMAP(t)
	defer closeFn()

	var stdout, stderr bytes.Buffer
	code := runIO([]string{
		"-addr", addr,
		"-user", "user",
		"-pass", "secret",
		"-plain",
		"-timeout", "5s",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d\nstderr:\n%s\nstdout:\n%s", code, stderr.String(), stdout.String())
	}
	out := stdout.String()
	for _, want := range []string{"INBOX", "Archive", "2024", "\\Archive"} {
		if !strings.Contains(out, want) {
			t.Fatalf("folder tree missing %q:\n%s", want, out)
		}
	}
}

func TestRunListMessages(t *testing.T) {
	clearIMAPEnv(t)
	addr, closeFn := startFakeIMAP(t)
	defer closeFn()

	var stdout, stderr bytes.Buffer
	code := runIO([]string{
		"-addr", addr,
		"-user", "user",
		"-pass", "secret",
		"-plain",
		"-mailbox", "INBOX",
		"-limit", "0",
		"-timeout", "5s",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d\nstderr:\n%s\nstdout:\n%s", code, stderr.String(), stdout.String())
	}
	out := stdout.String()
	for _, want := range []string{"Mailbox: INBOX", "read-only", "Messages: 2", "10", "Older", "11", "Newer", "Alice", "Carol"} {
		if !strings.Contains(out, want) {
			t.Fatalf("message list missing %q:\n%s", want, out)
		}
	}
}

func TestRunReadMessageUID(t *testing.T) {
	clearIMAPEnv(t)
	addr, closeFn := startFakeIMAP(t)
	defer closeFn()

	var stdout, stderr bytes.Buffer
	code := runIO([]string{
		"-addr", addr,
		"-user", "user",
		"-pass", "secret",
		"-plain",
		"-mailbox", "INBOX",
		"-uid", "11",
		"-timeout", "5s",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d\nstderr:\n%s\nstdout:\n%s", code, stderr.String(), stdout.String())
	}
	out := stdout.String()
	for _, want := range []string{"11", "Newer", "Carol", "hello from fake server"} {
		if !strings.Contains(out, want) {
			t.Fatalf("message body missing %q:\n%s", want, out)
		}
	}
}

func TestRunAuthFailure(t *testing.T) {
	clearIMAPEnv(t)
	addr, closeFn := startFakeIMAP(t)
	defer closeFn()

	var stdout, stderr bytes.Buffer
	code := runIO([]string{
		"-addr", addr,
		"-user", "user",
		"-pass", "wrong",
		"-plain",
		"-timeout", "5s",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "auth:") {
		t.Fatalf("stderr=%q, want auth error", stderr.String())
	}
}

func clearIMAPEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"IMAP_ADDR", "IMAP_USER", "IMAP_PASS", "IMAP_TLS", "IMAP_STARTTLS", "IMAP_INSECURE"} {
		t.Setenv(key, "")
	}
}
