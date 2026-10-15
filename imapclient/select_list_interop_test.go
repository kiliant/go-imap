//go:build interop

package imapclient_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kiliant/go-imap/imapclient"
	"github.com/kiliant/go-imap/interop/harness"
)

// selectListOnRev1 records the servers observed sending a LIST during SELECT
// on an IMAP4rev1 session. Only RFC 9051 section 6.3.2 asks for that LIST,
// and only once IMAP4rev2 is enabled, so the client forwards it to
// UnilateralDataHandler.List as an event; a server listed here produces a
// spurious event on every SELECT.
//
// Empty as of 2026-10-14: Dovecot, Stalwart, GreenMail, Cyrus and Courier send
// none on rev1, and Dovecot, Stalwart and Cyrus send exactly the RFC 9051 one
// after ENABLE IMAP4rev2.
var selectListOnRev1 = map[string]bool{}

// TestSelectListInterop records, per server, which LIST responses a SELECT
// produces and which reach the List handler, on a rev1 session and, where
// the server offers it, after ENABLE IMAP4rev2.
func TestSelectListInterop(t *testing.T) {
	for _, server := range harness.RunningServers() {
		server := server
		t.Run(server.Profile.Name, func(t *testing.T) {
			t.Parallel()
			t.Run("rev1", func(t *testing.T) {
				sent, delivered := selectListProbe(t, server, false)
				if sent != delivered {
					t.Fatalf("server sent %d LIST responses during SELECT on rev1, handler got %d; rev1 claims none", sent, delivered)
				}
				switch {
				case sent > 0 && !selectListOnRev1[server.Profile.Name]:
					t.Errorf("server sent %d LIST responses during SELECT on rev1, a spurious List event the matrix has not recorded", sent)
				case sent == 0 && selectListOnRev1[server.Profile.Name]:
					t.Logf("recorded deviation not observed: server sent no LIST during SELECT on rev1")
				case sent > 0:
					t.Logf("recorded deviation: server sends LIST during SELECT on rev1")
				}
			})
			t.Run("rev2", func(t *testing.T) {
				sent, delivered := selectListProbe(t, server, true)
				if sent == 0 {
					t.Logf("server sent no LIST during SELECT on rev2 (RFC 9051 section 6.3.2 asks for one)")
				}
				if want := max(sent-1, 0); delivered != want {
					t.Fatalf("server sent %d LIST responses during SELECT on rev2, handler got %d, want %d", sent, delivered, want)
				}
			})
		})
	}
}

// selectListProbe selects INBOX and returns how many LIST responses the
// server sent while the SELECT was in flight and how many reached the List
// handler. With rev2 it first enables IMAP4rev2, skipping a server that will
// not.
func selectListProbe(t *testing.T, server *harness.Server, rev2 bool) (sent, delivered int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	conn, err := dialRecording(ctx, server)
	if err != nil {
		server.LogDiagnostics(context.Background(), t, nil)
		t.Fatal(err)
	}
	var mu sync.Mutex
	client := imapclient.NewClient(conn, &imapclient.Options{
		// The recording wrapper hides any TLS from the client.
		AllowInsecureAuth: true,
		UnilateralData: &imapclient.UnilateralDataHandler{
			List: func(*imapclient.ListData) {
				mu.Lock()
				delivered++
				mu.Unlock()
			},
		},
	})
	defer client.Close()
	if err := client.WaitGreeting(ctx, nil); err != nil {
		failMailboxInterop(t, server, client, "greeting", err)
	}
	if err := client.Login(ctx, authInteropUsername, authInteropPassword, nil); err != nil {
		failMailboxInterop(t, server, client, "LOGIN", err)
	}
	if rev2 {
		if !client.Capabilities()["IMAP4REV2"] {
			t.Skip("server does not advertise IMAP4rev2")
		}
		if _, err := client.Enable(nil, "IMAP4rev2").Wait(ctx); err != nil {
			failMailboxInterop(t, server, client, "ENABLE IMAP4rev2", err)
		}
		if !client.EnabledCapabilities()["IMAP4REV2"] {
			t.Skip("server did not enable IMAP4rev2")
		}
	}

	mark := conn.mark()
	if _, err := client.Select("INBOX", nil).Wait(ctx); err != nil {
		failMailboxInterop(t, server, client, "SELECT INBOX", err)
	}
	sent = conn.countSince(mark, "* LIST ")
	if err := client.Logout(ctx, nil); err != nil {
		failMailboxInterop(t, server, client, "LOGOUT", err)
	}
	mu.Lock()
	defer mu.Unlock()
	return sent, delivered
}

// recordingConn keeps every byte read from the server, so a test can see
// responses the client claims without tracing them.
type recordingConn struct {
	net.Conn
	mu   sync.Mutex
	read bytes.Buffer
}

func dialRecording(ctx context.Context, server *harness.Server) (*recordingConn, error) {
	addr := server.Address
	if server.Profile.TLSPort != 0 {
		if tlsAddr, ok := server.AddressForPort(server.Profile.TLSPort); ok {
			addr = tlsAddr
		}
	}
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	if addr != server.Address {
		conn = tls.Client(conn, &tls.Config{InsecureSkipVerify: true})
	}
	return &recordingConn{Conn: conn}, nil
}

func (c *recordingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.mu.Lock()
	c.read.Write(p[:n])
	c.mu.Unlock()
	return n, err
}

func (c *recordingConn) mark() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.read.Len()
}

// countSince counts the response lines after offset mark that begin with
// prefix.
func (c *recordingConn) countSince(mark int, prefix string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	count := 0
	for _, line := range strings.Split(c.read.String()[mark:], "\r\n") {
		if strings.HasPrefix(line, prefix) {
			count++
		}
	}
	return count
}
