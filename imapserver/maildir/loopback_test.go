package maildir_test

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapclient"
	"github.com/kiliant/go-imap/imapserver"
	"github.com/kiliant/go-imap/imapserver/maildir"
)

func TestMaildirClientLoopback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	b, err := maildir.Open(ctx, t.TempDir(), &maildir.Options{Accounts: []maildir.AccountOptions{{Username: "alice", Password: "secret"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(context.Background(), nil)
	server := imapserver.New(b, &imapserver.Options{AllowInsecureAuth: true})
	defer server.Close(context.Background(), nil)
	clientSide, serverSide := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- server.ServeConn(ctx, serverSide, nil) }()
	client := imapclient.NewClient(clientSide, &imapclient.Options{AllowInsecureAuth: true})
	defer client.Close()
	if err := client.Login(ctx, "alice", "secret", nil); err != nil {
		t.Fatal(err)
	}
	for _, cap := range []string{"UIDPLUS", "CONDSTORE", "QRESYNC", "MOVE", "UIDBATCHES", "METADATA"} {
		if !client.Supports(cap) {
			t.Fatalf("capability missing: %s", cap)
		}
	}
	raw := "Subject: streamed\r\n\r\nbody\r\n"
	appended, err := client.Append(ctx, "INBOX", nil, int64(len(raw)), strings.NewReader(raw)).Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Select("INBOX", nil).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	command := client.FetchUID(imap.UIDSetNum(appended.UID), nil, &imap.FetchItemBodySection{Peek: true}, imap.FetchItemEnvelope, imap.FetchItemRFC822Size)
	count := 0
	for {
		data, err := command.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		count++
		// The client resumes decoding after the literal drains; inspect its
		// known key before reading, then leave the response map alone.
		values := data.Items["BODY[]"]
		if len(values) != 1 {
			t.Fatal("missing body literal")
		}
		body := values[0].(*imap.FetchDataBodySection)
		got, err := io.ReadAll(body.Literal)
		if err != nil || string(got) != raw {
			t.Fatalf("streamed body: %q, %v", got, err)
		}
	}
	if count != 1 {
		t.Fatalf("FETCH returned %d messages", count)
	}
	if _, err := client.UIDBatches(ctx, 500, nil); err != nil {
		t.Fatal(err)
	}
	if err := client.Logout(ctx, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
