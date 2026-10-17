package imapserver_test

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap/imapserver"
	"github.com/kiliant/go-imap/imapserver/memory"
)

type bearerAuthBackend struct {
	inner     imapserver.Backend
	mechanism string
	received  chan imapserver.Credentials
}

func (b *bearerAuthBackend) SupportsCapability(_ context.Context, name string, _ *imapserver.CapabilitySupportOptions) bool {
	return name == "AUTH="+b.mechanism
}

func (b *bearerAuthBackend) Authenticate(ctx context.Context, conn *imapserver.ConnInfo, credentials *imapserver.Credentials, options *imapserver.AuthenticateOptions) (imapserver.Session, error) {
	b.received <- *credentials
	if credentials.Mechanism == b.mechanism && credentials.Token == "test-token" && credentials.Username == "user" {
		credentials = &imapserver.Credentials{Mechanism: "PLAIN", Username: "user", Password: "secret"}
	}
	return b.inner.Authenticate(ctx, conn, credentials, options)
}

func TestBearerAuthenticationWitnessOnWire(t *testing.T) {
	for _, mechanism := range []string{"", "XOAUTH2", "OAUTHBEARER"} {
		t.Run("witness="+mechanism, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			backend := &bearerAuthBackend{
				inner:     memory.New(&memory.Options{Users: map[string]string{"user": "secret"}}),
				mechanism: mechanism,
				received:  make(chan imapserver.Credentials, 4),
			}
			certificate := testCertificate(t)
			serverSide, clientSide := net.Pipe()
			defer clientSide.Close()
			_ = clientSide.SetDeadline(time.Now().Add(5 * time.Second))
			server := imapserver.New(backend, nil)
			done := make(chan error, 1)
			go func() {
				done <- server.ServeConn(ctx, tls.Server(serverSide, serverTLSConfig(t, certificate)), nil)
			}()
			client := tls.Client(clientSide, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}) // controlled test endpoint
			if err := client.HandshakeContext(ctx); err != nil {
				t.Fatal(err)
			}
			reader := bufio.NewReader(client)
			greeting, err := reader.ReadString('\n')
			if err != nil || !strings.HasPrefix(greeting, "* OK ") {
				t.Fatalf("greeting = %q, %v", greeting, err)
			}
			writeRawCommand(t, client, "c CAPABILITY\r\n")
			lines, tagged := collectUntilTag(t, reader, "c ")
			if !strings.HasPrefix(tagged, "c OK") {
				t.Fatal(tagged)
			}
			for _, name := range []string{"XOAUTH2", "OAUTHBEARER"} {
				want := name == mechanism
				for _, response := range []string{greeting, strings.Join(lines, " ")} {
					if got := slices.Contains(strings.Fields(response), "AUTH="+name); got != want {
						t.Errorf("%s advertised=%t, want %t: %q", name, got, want, response)
					}
				}
				if !want {
					// No initial response: refusing here must not start a SASL exchange.
					writeRawCommand(t, client, "r AUTHENTICATE "+name+"\r\n")
					lines, tagged := collectUntilTag(t, reader, "r ")
					if len(lines) != 0 || !strings.HasPrefix(tagged, "r NO [CANNOT]") {
						t.Fatalf("unwitnessed AUTHENTICATE: %v %q", lines, tagged)
					}
				}
			}
			if len(backend.received) != 0 {
				t.Fatal("unwitnessed mechanism reached the credential backend")
			}
			selected, response := mechanism, ""
			switch mechanism {
			case "XOAUTH2":
				response = "user=user\x01auth=Bearer test-token\x01\x01"
			case "OAUTHBEARER":
				response = "n,a=user,\x01auth=Bearer test-token\x01\x01"
			default:
				selected, response = "PLAIN", "\x00user\x00secret"
			}
			writeRawCommand(t, client, fmt.Sprintf("a AUTHENTICATE %s %s\r\n", selected, base64.StdEncoding.EncodeToString([]byte(response))))
			if _, tagged := collectUntilTag(t, reader, "a "); !strings.HasPrefix(tagged, "a OK") {
				t.Fatal(tagged)
			}
			credentials := <-backend.received
			if credentials.Mechanism != selected || credentials.Username != "user" {
				t.Fatalf("unexpected extracted credentials: mechanism=%s user=%s", credentials.Mechanism, credentials.Username)
			}
			if mechanism != "" && (credentials.Token != "test-token" || credentials.Password != "") {
				t.Error("bearer credentials lost their token or acquired a password")
			}
			writeRawCommand(t, client, "l LOGOUT\r\n")
			if _, tagged := collectUntilTag(t, reader, "l "); !strings.HasPrefix(tagged, "l OK") {
				t.Fatal(tagged)
			}
			// Read TLS close_notify so the server can finish closing the pipe.
			if _, err := io.Copy(io.Discard, reader); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}
