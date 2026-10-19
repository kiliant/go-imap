package imapserver_test

import (
	"context"
	"encoding/hex"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap/imapclient"
	"github.com/kiliant/go-imap/imapserver"
	"github.com/kiliant/go-imap/imapserver/memory"
)

func TestURLAuthClientRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	server := imapserver.New(memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}}), &imapserver.Options{AllowInsecureAuth: true})
	serverSide, clientSide := net.Pipe()
	served := make(chan error, 1)
	go func() { served <- server.ServeConn(ctx, serverSide, nil) }()
	client := imapclient.NewClient(clientSide, &imapclient.Options{AllowInsecureAuth: true})
	defer client.Close()
	if err := client.WaitGreeting(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := client.Login(ctx, "alice", "secret", nil); err != nil {
		t.Fatal(err)
	}
	if client.Supports("URLAUTH=BINARY") {
		t.Fatal("unimplemented binary URLFETCH advertised")
	}
	bodies := []string{"Subject: first\r\n\r\none", "Subject: second\r\n\r\ntwo"}
	for _, raw := range bodies {
		if _, err := client.Append(ctx, "INBOX", nil, int64(len(raw)), strings.NewReader(raw)).Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	generated, err := client.GenURLAuth(ctx, []imapclient.GenURLAuthRequest{
		{RumpURL: "imap://alice@example.com/INBOX/;UID=1;URLAUTH=anonymous"},
		{RumpURL: "imap://alice@example.com/INBOX/;UID=2;URLAUTH=anonymous"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(generated.URLs) != 2 {
		t.Fatalf("GENURLAUTH returned %v", generated.URLs)
	}
	for _, url := range generated.URLs {
		_, token, ok := strings.Cut(url, ":internal:")
		if decoded, err := hex.DecodeString(token); !ok || err != nil || len(decoded) < 16 {
			t.Fatalf("invalid URLAUTH token: %q", url)
		}
	}
	items, err := client.URLFetch(ctx, append(append([]string(nil), generated.URLs...), "imap://invalid"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 || items[0].Body == nil || *items[0].Body != bodies[0] || items[1].Body == nil || *items[1].Body != bodies[1] || items[2].Body != nil {
		t.Fatalf("URLFETCH results = %#v", items)
	}
	if err := client.ResetKey(ctx, &imapclient.ResetKeyOptions{Mailbox: "INBOX", Mechanisms: []imapclient.URLAuthMechanism{imapclient.URLAuthInternal, "UNKNOWN"}}); err == nil {
		t.Fatal("unsupported RESETKEY mechanism accepted")
	}
	items, err = client.URLFetch(ctx, generated.URLs, nil)
	if err != nil || len(items) != 2 || items[0].Body == nil {
		t.Fatalf("failed RESETKEY revoked valid keys: %#v, %v", items, err)
	}
	if err := client.ResetKey(ctx, &imapclient.ResetKeyOptions{Mailbox: "inbox", Mechanisms: []imapclient.URLAuthMechanism{imapclient.URLAuthInternal}}); err != nil {
		t.Fatal(err)
	}
	items, err = client.URLFetch(ctx, generated.URLs, nil)
	if err != nil || len(items) != 2 || items[0].Body != nil || items[1].Body != nil {
		t.Fatalf("RESETKEY did not revoke both URLs: %#v, %v", items, err)
	}
	if err := client.Logout(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := <-served; err != nil {
		t.Fatal(err)
	}
}
