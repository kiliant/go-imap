package imapserver_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap/imapserver"
	"github.com/kiliant/go-imap/imapserver/maildir"
	"github.com/kiliant/go-imap/imapserver/memory"
)

func TestLoopbackSearchUnsupportedCharset(t *testing.T) {
	for _, storage := range []string{"memory", "maildir"} {
		t.Run(storage, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			var backend imapserver.Backend
			if storage == "memory" {
				backend = memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}})
			} else {
				b, err := maildir.Open(ctx, t.TempDir(), &maildir.Options{Accounts: []maildir.AccountOptions{{Username: "alice", Password: "secret"}}})
				if err != nil {
					t.Fatal(err)
				}
				defer b.Close(context.Background(), nil)
				backend = b
			}
			server := imapserver.New(backend, &imapserver.Options{AllowInsecureAuth: true})
			defer server.Close(context.Background(), nil)
			_, connection, reader := newRawSessionOn(t, ctx, server)
			defer connection.Close()
			if err := connection.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
				t.Fatal(err)
			}
			writeRawCommand(t, connection, "S SELECT INBOX\r\n")
			if _, tagged := collectUntilTag(t, reader, "S "); !strings.HasPrefix(tagged, "S OK ") {
				t.Fatal(tagged)
			}
			for _, populated := range []bool{false, true} {
				if populated {
					body := "Subject: charset\r\n\r\nbody\r\n"
					writeRawCommand(t, connection, fmt.Sprintf("A APPEND INBOX {%d+}\r\n%s\r\n", len(body), body))
					if _, tagged := collectUntilTag(t, reader, "A "); !strings.HasPrefix(tagged, "A OK ") {
						t.Fatal(tagged)
					}
				}
				for _, command := range []string{"SEARCH", "UID SEARCH"} {
					writeRawCommand(t, connection, "Q "+command+" CHARSET KOI8-R ALL\r\n")
					_, tagged := collectUntilTag(t, reader, "Q ")
					if !strings.HasPrefix(tagged, "Q NO [BADCHARSET (US-ASCII UTF-8 ISO-8859-1)] ") {
						t.Errorf("%s/populated=%t: %q", command, populated, tagged)
					}
				}
				for _, charset := range []string{"US-ASCII", "utf-8", "ISO-8859-1", "LATIN1"} {
					writeRawCommand(t, connection, "V SEARCH CHARSET "+charset+" ALL\r\n")
					_, tagged := collectUntilTag(t, reader, "V ")
					if !strings.HasPrefix(tagged, "V OK ") {
						t.Errorf("supported charset %s: %q", charset, tagged)
					}
				}
			}
		})
	}
}
