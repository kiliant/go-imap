//go:build !(linux || darwin || freebsd || netbsd || openbsd)

package main

import "fmt"

func readPasswordPrompt(string) (string, error) {
	return "", fmt.Errorf("interactive password entry is not supported on this OS; set -pass or IMAP_PASS")
}
