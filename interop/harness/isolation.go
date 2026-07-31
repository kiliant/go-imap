package harness

import (
	"fmt"
	"strings"
	"sync/atomic"
)

var mailboxSequence atomic.Uint64

// UniqueMailbox returns a server-safe namespace unique within the process.
// Tests may run in parallel without sharing mailbox state.
func UniqueMailbox(testName string) string {
	testName = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		default:
			return '-'
		}
	}, testName)
	return fmt.Sprintf("interop-%s-%d", testName, mailboxSequence.Add(1))
}
