package imapserver_test

import (
	"context"
	"errors"
	"io"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapclient"
	"github.com/kiliant/go-imap/imapserver"
	"github.com/kiliant/go-imap/imapserver/maildir"
	"github.com/kiliant/go-imap/imapserver/memory"
)

func TestRev2BinaryFetchReferenceBackends(t *testing.T) {
	for _, name := range []string{"memory", "maildir"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			var backend imapserver.Backend
			if name == "memory" {
				backend = memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}})
			} else {
				b, err := maildir.Open(ctx, t.TempDir(), &maildir.Options{Accounts: []maildir.AccountOptions{{Username: "alice", Password: "secret"}}})
				if err != nil {
					t.Fatal(err)
				}
				defer b.Close(ctx, nil)
				backend = b
			}
			server := imapserver.New(backend, &imapserver.Options{AllowInsecureAuth: true})
			defer server.Close(ctx, nil)
			transport, peer := net.Pipe()
			go func() { _ = server.ServeConn(ctx, transport, nil) }()
			client := imapclient.NewClient(peer, &imapclient.Options{AllowInsecureAuth: true})
			defer client.Close()
			if err := client.Login(ctx, "alice", "secret", nil); err != nil {
				t.Fatal(err)
			}
			if !client.Supports("IMAP4REV2") || client.Supports("BINARY-FETCH") || name == "maildir" && client.Supports("BINARY") {
				t.Fatalf("wrong binary/rev2 capabilities: %v", client.Capabilities())
			}
			enabled, err := client.Enable(nil, "IMAP4REV2").Wait(ctx)
			if err != nil || !slices.Contains(enabled, "IMAP4REV2") {
				t.Fatalf("ENABLE rev2 = %v, %v", enabled, err)
			}
			var uids []imap.UID
			for _, raw := range []string{
				"Content-Type: application/octet-stream\r\nContent-Transfer-Encoding: base64\r\n\r\nYWJjAGRlZg==",
				"Content-Type: application/octet-stream\r\nContent-Transfer-Encoding: x-future\r\n\r\nbody",
			} {
				data, err := client.Append(ctx, "INBOX", nil, int64(len(raw)), strings.NewReader(raw)).Wait(ctx)
				if err != nil {
					t.Fatal(err)
				}
				uids = append(uids, data.UID)
			}
			if _, err := client.Select("INBOX", nil).Wait(ctx); err != nil {
				t.Fatal(err)
			}
			fetch := func(uid imap.UID, items ...imap.FetchItem) (body string, size int64, flags []imap.Flag, origin int64, hasOrigin bool, failure error) {
				cmd := client.FetchUID(imap.UIDSetNum(uid), nil, items...)
				for {
					data, err := cmd.Next(ctx)
					if err == io.EOF {
						return
					}
					if err != nil {
						failure = err
						return
					}
					for _, values := range data.Items {
						for _, value := range values {
							switch v := value.(type) {
							case *imap.FetchDataBinarySection:
								b, err := io.ReadAll(v.Literal)
								if err != nil {
									failure = err
									return
								}
								body, origin, hasOrigin = string(b), v.Origin, v.HasOrigin
							case imap.FetchDataBinarySectionSize:
								size = int64(v)
							case imap.FetchDataFlags:
								flags = slices.Clone([]imap.Flag(v))
							}
						}
					}
				}
			}
			body, size, flags, _, _, err := fetch(uids[0], &imap.FetchItemBinarySection{Part: []int{1}, Peek: true}, &imap.FetchItemBinarySectionSize{Part: []int{1}}, imap.FetchItemFlags)
			if err != nil || body != "abc\x00def" || size != 7 || imap.ContainsFlag(flags, imap.FlagSeen) {
				t.Fatalf("PEEK/SIZE/flags = %q, %d, %v, %v", body, size, flags, err)
			}
			body, _, _, origin, hasOrigin, err := fetch(uids[0], &imap.FetchItemBinarySection{Part: []int{1}, Peek: true, Partial: &imap.SectionPartial{Offset: 2, Size: 4}})
			if err != nil || body != "c\x00de" || !hasOrigin || origin != 2 {
				t.Fatalf("decoded partial = %q, origin %d/%t, %v", body, origin, hasOrigin, err)
			}
			if _, _, _, _, _, err := fetch(uids[0], &imap.FetchItemBinarySection{Part: []int{1}}); err != nil {
				t.Fatal(err)
			}
			_, _, flags, _, _, err = fetch(uids[0], imap.FetchItemFlags)
			if err != nil || !imap.ContainsFlag(flags, imap.FlagSeen) {
				t.Fatalf("non-PEEK flags = %v, %v", flags, err)
			}
			_, _, _, _, _, err = fetch(uids[1], &imap.FetchItemBinarySection{Part: []int{1}})
			var protocolErr *imap.Error
			if !errors.As(err, &protocolErr) || protocolErr.Code != imap.CodeUnknownCTE || strings.Contains(err.Error(), "*imap.") {
				t.Fatalf("unknown CTE refusal = %v", err)
			}
			_, _, flags, _, _, err = fetch(uids[1], imap.FetchItemFlags)
			if err != nil || imap.ContainsFlag(flags, imap.FlagSeen) {
				t.Fatalf("failed BINARY set Seen: %v, %v", flags, err)
			}
		})
	}
}
