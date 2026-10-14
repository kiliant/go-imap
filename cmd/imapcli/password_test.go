package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolvePasswordProvided(t *testing.T) {
	got, err := resolvePassword(passwordSource{Value: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "secret" {
		t.Fatalf("resolvePassword = %q, want secret", got)
	}
}

func TestTrimPasswordLine(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"secret\n", "secret"},
		{"secret\r\n", "secret"},
		{"secret", "secret"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := trimPasswordLine(tt.in); got != tt.want {
			t.Fatalf("trimPasswordLine(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestResolvePasswordFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pw")
	if err := os.WriteFile(path, []byte("s3cret pass\r\nignored second line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := resolvePassword(passwordSource{File: path})
	if err != nil || got != "s3cret pass" {
		t.Fatalf("resolvePassword = %q, %v; want the first line", got, err)
	}

	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(empty, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := resolvePassword(passwordSource{File: empty}); err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Fatalf("empty file: err = %v", err)
	}
	if _, err := resolvePassword(passwordSource{File: filepath.Join(t.TempDir(), "missing")}); err == nil {
		t.Fatal("missing file: no error")
	}
}

func TestResolvePasswordCommand(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	got, err := resolvePassword(passwordSource{Command: "printf 'from cmd\\nsecond\\n'"})
	if err != nil || got != "from cmd" {
		t.Fatalf("resolvePassword = %q, %v; want the first output line", got, err)
	}
	_, err = resolvePassword(passwordSource{Command: "echo locked >&2; exit 3"})
	if err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("failing command: err = %v, want its stderr", err)
	}
	if _, err := resolvePassword(passwordSource{Command: "true"}); err == nil {
		t.Fatal("silent command: no error")
	}
}

func TestResolvePasswordOneSourceOnly(t *testing.T) {
	_, err := resolvePassword(passwordSource{Value: "a", Command: "echo b"})
	if err == nil || !strings.Contains(err.Error(), "use only one of") {
		t.Fatalf("err = %v", err)
	}
}
