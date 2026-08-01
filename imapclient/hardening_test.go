package imapclient

import (
	"bufio"
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
	"github.com/kiliant/go-imap/interop/harness/adversarial"
)

func TestClientReadDeadline(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		clientConn, serverConn := net.Pipe()
		defer serverConn.Close()
		client := NewClient(clientConn, nil)
		defer client.Close()
		wopts := client.dec.Options()
		if wopts.ReadTimeout != defaultReadTimeout {
			t.Fatalf("ReadTimeout = %v, want %v", wopts.ReadTimeout, defaultReadTimeout)
		}
		if wopts.MaxUntaggedPerCommand != imapwire.DefaultMaxUntaggedPerCommand {
			t.Fatalf("MaxUntaggedPerCommand = %d, want %d", wopts.MaxUntaggedPerCommand, imapwire.DefaultMaxUntaggedPerCommand)
		}
	})

	t.Run("stalled greeting", func(t *testing.T) {
		clientConn, serverConn := net.Pipe()
		defer serverConn.Close()
		client := NewClient(clientConn, &Options{ReadTimeout: 20 * time.Millisecond})
		defer client.Close()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		err := client.WaitGreeting(ctx, nil)
		var protocolErr *imap.Error
		if !errors.As(err, &protocolErr) || protocolErr.Type != imap.ErrorTypeProtocol {
			t.Fatalf("WaitGreeting() = %T %[1]v, want protocol timeout", err)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("WaitGreeting() reached context deadline instead of read deadline: %v", err)
		}
	})
}

func TestClientCapsBufferedUntaggedResponses(t *testing.T) {
	tests := []struct {
		name  string
		state State
		issue func(*Client) *Command
		wire  string
	}{
		{
			name:  "LIST",
			state: StateAuthenticated,
			issue: func(c *Client) *Command { return c.List("", "*", nil).Command },
			wire:  "* LIST () \"/\" one\r\n* LIST () \"/\" two\r\n",
		},
		{
			name:  "SEARCH",
			state: StateSelected,
			issue: func(c *Client) *Command {
				return c.Search(imap.SearchKeyword("ALL"), nil).Command
			},
			wire: "* SEARCH 1\r\n* SEARCH 2\r\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientConn, serverConn := net.Pipe()
			defer serverConn.Close()
			go func() {
				defer serverConn.Close()
				_, _ = serverConn.Write([]byte("* PREAUTH ready\r\n"))
				line, _ := bufio.NewReader(serverConn).ReadString('\n')
				tag := strings.Fields(line)[0]
				_, _ = serverConn.Write([]byte(tt.wire + tag + " OK done\r\n"))
			}()
			client := NewClient(clientConn, &Options{MaxUntaggedResponses: 1})
			defer client.Close()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := client.WaitGreeting(ctx, nil); err != nil {
				t.Fatal(err)
			}
			client.mu.Lock()
			client.state = tt.state
			client.mu.Unlock()
			err := tt.issue(client).Wait(ctx)
			var protocolErr *imap.Error
			if !errors.As(err, &protocolErr) || protocolErr.Err == nil || !strings.Contains(protocolErr.Err.Error(), "too many untagged responses") {
				t.Fatalf("Wait() = %T %[1]v, want buffered-response limit error", err)
			}
		})
	}
}

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
			if err := client.WaitGreeting(ctx, nil); err != nil {
				t.Fatal(err)
			}
			if err := client.Noop(nil).Wait(ctx); err == nil {
				t.Fatal("hostile response completed NOOP successfully")
			}
		})
	}
}
