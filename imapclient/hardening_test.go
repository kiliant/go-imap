package imapclient

import (
	"context"
	"testing"
	"time"

	"github.com/kiliant/go-imap/interop/harness/adversarial"
)

// TestClientRejectsAdversarialResponses exercises the production reader
// goroutine against the hostile server catalogue. A malformed response must
// fail the in-flight command or its context; it must never be treated as a
// successful NOOP.
func TestClientRejectsAdversarialResponses(t *testing.T) {
	for _, scenario := range adversarial.Cases() {
		scenario := scenario
		t.Run(scenario.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			clientConn, server := adversarial.Pipe(ctx, scenario)
			defer func() { _ = server.Close() }()
			client := NewClient(clientConn, nil)
			defer client.Close()
			if err := client.WaitGreeting(ctx); err != nil {
				t.Fatal(err)
			}
			if err := client.Noop().Wait(ctx); err == nil {
				t.Fatal("hostile response completed NOOP successfully")
			}
		})
	}
}
