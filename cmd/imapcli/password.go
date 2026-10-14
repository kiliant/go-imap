package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// passwordSource names where the password comes from. At most one field may
// be set; with none set, the password is prompted for on the terminal.
type passwordSource struct {
	Value   string // -pass / IMAP_PASS
	File    string // -pass-file / IMAP_PASS_FILE
	Command string // -pass-cmd / IMAP_PASS_CMD
}

// resolvePassword returns the password from the one configured source, or
// prompts on the controlling terminal with echo disabled when none is set.
func resolvePassword(src passwordSource) (string, error) {
	set := 0
	for _, v := range []string{src.Value, src.File, src.Command} {
		if v != "" {
			set++
		}
	}
	if set > 1 {
		return "", errors.New("use only one of -pass, -pass-file and -pass-cmd (or IMAP_PASS, IMAP_PASS_FILE and IMAP_PASS_CMD)")
	}
	switch {
	case src.Value != "":
		return src.Value, nil
	case src.File != "":
		return readPasswordFile(src.File)
	case src.Command != "":
		return runPasswordCommand(src.Command)
	}
	pass, err := readPasswordPrompt("Password (input hidden): ")
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	return pass, nil
}

// readPasswordFile returns the first line of path.
func readPasswordFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read password file: %w", err)
	}
	pass := firstLine(data)
	if pass == "" {
		return "", fmt.Errorf("password file %s is empty", path)
	}
	return pass, nil
}

// runPasswordCommand runs command with /bin/sh and returns the first line of
// its standard output, so a keychain or password-manager lookup can supply the
// password without it ever appearing in the process list or the environment.
func runPasswordCommand(command string) (string, error) {
	cmd := exec.Command("/bin/sh", "-c", command)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("password command: %w: %s", err, msg)
		}
		return "", fmt.Errorf("password command: %w", err)
	}
	pass := firstLine(out)
	if pass == "" {
		return "", errors.New("password command printed nothing")
	}
	return pass, nil
}

func firstLine(data []byte) string {
	line, _, _ := strings.Cut(string(data), "\n")
	return trimPasswordLine(line)
}

func trimPasswordLine(line string) string {
	return strings.TrimRight(line, "\r\n")
}
