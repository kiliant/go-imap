package imapserver_test

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap/imapserver"
	"github.com/kiliant/go-imap/imapserver/maildir"
	"github.com/kiliant/go-imap/imapserver/memory"
)

func TestMetadataMaxSizeReportsLongestOmittedValue(t *testing.T) {
	for _, kind := range []string{"memory", "maildir"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			var backend imapserver.Backend = memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}})
			if kind == "maildir" {
				b, err := maildir.Open(ctx, t.TempDir(), &maildir.Options{Accounts: []maildir.AccountOptions{{Username: "alice", Password: "secret"}}})
				if err != nil {
					t.Fatal(err)
				}
				defer b.Close(context.Background(), nil)
				backend = b
			}
			server := imapserver.New(backend, &imapserver.Options{AllowInsecureAuth: true})
			defer server.Close(context.Background(), nil)
			client, peer := net.Pipe()
			defer client.Close()
			go func() { _ = server.ServeConn(ctx, peer, nil) }()
			if err := client.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
				t.Fatal(err)
			}
			reader := bufio.NewReader(client)
			if _, err := reader.ReadString('\n'); err != nil {
				t.Fatal(err)
			}
			writeRawCommand(t, client, "L LOGIN alice secret\r\n")
			if _, tagged := collectUntilTag(t, reader, "L "); !strings.HasPrefix(tagged, "L OK") {
				t.Fatal(tagged)
			}
			for _, scope := range []string{"INBOX", `""`} {
				writeRawCommand(t, client, "S SETMETADATA "+scope+" (/private/zero \"\" /private/tree \"tiny\" /private/tree/leaf \"1234567\" /private/outside \"12345678901\")\r\n")
				if _, tagged := collectUntilTag(t, reader, "S "); !strings.HasPrefix(tagged, "S OK") {
					t.Fatal(tagged)
				}
				for i, tt := range []struct {
					options, requested string
					zero, tree, leaf   bool
					longest            int
				}{
					{"(DEPTH infinity)", "(/private/zero /private/tree)", true, true, true, 0},
					{"(MAXSIZE 0 DEPTH infinity)", "(/private/zero /private/tree)", true, false, false, 7},
					{"(MAXSIZE 4 DEPTH infinity)", "(/private/zero /private/tree)", true, true, false, 7},
					{"(MAXSIZE 7 DEPTH infinity)", "(/private/zero /private/tree)", true, true, true, 0},
					{"(MAXSIZE 4 DEPTH 0)", "/private/tree", false, true, false, 0},
					{"(MAXSIZE 4 DEPTH 1)", "/private/tree", false, true, false, 7},
					{"(MAXSIZE 0)", "/private/missing", false, false, false, 0},
				} {
					tag := fmt.Sprintf("G%d", i)
					writeRawCommand(t, client, tag+" GETMETADATA "+tt.options+" "+scope+" "+tt.requested+"\r\n")
					untagged, tagged := collectUntilTag(t, reader, tag+" ")
					if !strings.HasPrefix(tagged, tag+" OK") {
						t.Fatal(tagged)
					}
					if tt.longest > 0 {
						if want := fmt.Sprintf("[METADATA LONGENTRIES %d]", tt.longest); !strings.Contains(tagged, want) {
							t.Errorf("%s %s: tagged = %q, want %s", scope, tt.options, tagged, want)
						}
					} else if strings.Contains(tagged, "LONGENTRIES") {
						t.Errorf("unexpected LONGENTRIES: %q", tagged)
					}
					wire := strings.Join(untagged, "\n")
					for entry, want := range map[string]bool{"/private/zero": tt.zero, "/private/tree": tt.tree, "/private/tree/leaf": tt.leaf, "/private/outside": false} {
						if strings.Contains(wire, entry) != want {
							t.Errorf("%s %s: presence of %s in %q, want %v", scope, tt.options, entry, wire, want)
						}
					}
				}
			}
		})
	}
}
