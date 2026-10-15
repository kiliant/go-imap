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
	// The fake server sends UID, FLAGS and ENVELOPE after the body, so the
	// summary line is only right if it does not come from the body's response.
	summary := ""
	for line := range strings.SplitSeq(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "11 ") {
			summary = line
		}
	}
	if !strings.Contains(summary, "Carol") || !strings.Contains(summary, "Newer") {
		t.Fatalf("summary line for UID 11 missing or incomplete: %q\n%s", summary, out)
	}
	if !strings.Contains(out, "\r\n\r\nhello from fake server\r\n") {
		t.Fatalf("message not printed byte for byte:\n%s", out)
	}
	if strings.Index(out, summary) > strings.Index(out, "hello from fake server") {
		t.Fatalf("summary printed after the message:\n%s", out)
	}
}

func TestRunReadMessageUIDNotFound(t *testing.T) {
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
		"-uid", "99",
		"-timeout", "5s",
	}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "no message with UID 99") {
		t.Fatalf("exit = %d, want 1 with a not-found error\nstderr:\n%s", code, stderr.String())
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
	for _, key := range []string{"IMAP_ADDR", "IMAP_USER", "IMAP_PASS", "IMAP_PASS_FILE", "IMAP_PASS_CMD", "IMAP_TLS", "IMAP_STARTTLS", "IMAP_INSECURE"} {
		t.Setenv(key, "")
	}
}
