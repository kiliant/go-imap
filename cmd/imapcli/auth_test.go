package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapclient"
)

func TestAuthenticateFallbackUsesLocalSelectionRefusal(t *testing.T) {
	for _, tc := range []struct {
		name, caps, reply, want string
		insecure                bool
		wantErr                 bool
	}{
		{"OAuth-only", "AUTH=OAUTHBEARER AUTH=XOAUTH2", "", "LOGIN", true, false},
		{"unknown-mechanisms", "AUTH=GSSAPI AUTH=SCRAM-SHA-512", "", "LOGIN", true, false},
		{"no-mechanisms", "", "", "CAPABILITY LOGIN", true, false},
		{"password-SASL", "AUTH=OAUTHBEARER AUTH=PLAIN SASL-IR", "", "AUTHENTICATE", true, false},
		{"server-refusal", "AUTH=PLAIN SASL-IR", "NO [AUTHENTICATIONFAILED] rejected", "AUTHENTICATE", true, true},
		{"remote-code-collision", "AUTH=PLAIN SASL-IR", "NO [" + string(imapclient.CodeNoCompatibleAuthMechanism) + "] rejected", "AUTHENTICATE", true, true},
		{"privacy-refusal", "AUTH=PLAIN SASL-IR", "", "", false, true},
		{"binding-refusal", "AUTH=SCRAM-SHA-256-PLUS", "", "", true, true},
		{"LOGIN-disabled", "AUTH=XOAUTH2 LOGINDISABLED", "", "", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clientConn, serverConn := net.Pipe()
			commands := make(chan []string, 1)
			go func() {
				defer serverConn.Close()
				capabilities := strings.TrimSpace("IMAP4rev1 " + tc.caps)
				_, _ = fmt.Fprintf(serverConn, "* OK [CAPABILITY %s] ready\r\n", capabilities)
				r := bufio.NewReader(serverConn)
				var sent []string
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						commands <- sent
						return
					}
					fields := strings.Fields(line)
					if len(fields) < 2 {
						continue
					}
					tag, command := fields[0], fields[1]
					if command == "CAPABILITY" {
						// Post-auth CAPABILITY is required, but not an auth choice.
						if len(sent) == 0 {
							sent = append(sent, command)
						}
						_, _ = fmt.Fprintf(serverConn, "* CAPABILITY %s\r\n%s OK refreshed\r\n", capabilities, tag)
						continue
					}
					sent = append(sent, command)
					reply := tc.reply
					if reply == "" {
						reply = "OK authenticated"
					}
					_, _ = fmt.Fprintf(serverConn, "%s %s\r\n", tag, reply)
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			c := imapclient.NewClient(clientConn, &imapclient.Options{AllowInsecureAuth: tc.insecure})
			if err := c.WaitGreeting(ctx, nil); err != nil {
				c.Close()
				t.Fatal(err)
			}
			err := authenticate(ctx, c, "user", "secret")
			_ = c.Close()
			if got := strings.Join(<-commands, " "); got != tc.want || (err != nil) != tc.wantErr {
				t.Fatalf("commands = %q, error %v; want %q, failure %t", got, err, tc.want, tc.wantErr)
			}
			if tc.name == "remote-code-collision" {
				var protocol *imap.Error
				if !errors.As(err, &protocol) || protocol.Type != imap.ErrorTypeNo || protocol.Code != imapclient.CodeNoCompatibleAuthMechanism {
					t.Fatalf("server refusal was not retained: %v", err)
				}
			}
		})
	}
}
