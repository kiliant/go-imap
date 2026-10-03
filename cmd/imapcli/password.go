package main

import (
	"fmt"
	"strings"
)

// resolvePassword returns provided when non-empty; otherwise prompts on the
// controlling terminal with echo disabled.
func resolvePassword(provided string) (string, error) {
	if provided != "" {
		return provided, nil
	}
	pass, err := readPasswordPrompt("Password (input hidden): ")
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	return pass, nil
}

func trimPasswordLine(line string) string {
	return strings.TrimRight(line, "\r\n")
}
