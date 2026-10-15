//go:build interop

package interop_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/kiliant/go-imap/imapclient"
	"github.com/kiliant/go-imap/interop/harness"
)

// TestListRecursiveMatch is our row of the RFC 5258 section 3.5 scenario the
// third-party servers run in imapclient's interop suite: a child is subscribed
// and its parent is not, and LIST (SUBSCRIBED RECURSIVEMATCH) returns the
// parent with CHILDINFO ("SUBSCRIBED"). Unlike theirs, this row may not omit
// it.
func TestListRecursiveMatch(t *testing.T) {
	for _, server := range harness.RunningServers() {
		t.Run(server.Profile.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			client, err := imapclient.Dial(ctx, server.Address, &imapclient.Options{AllowInsecureAuth: true})
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer client.Close()
			if err := client.Login(ctx, "interop@example.test", "interop-pw", nil); err != nil {
				t.Fatalf("login: %v", err)
			}
			harness.RequireCapabilities(t, client.Capabilities(), "LIST-EXTENDED")

			parent := harness.UniqueMailbox("recursivematch")
			child := parent + "/child"
			for _, mailbox := range []string{parent, child} {
				if err := client.Create(mailbox, nil).Wait(ctx); err != nil {
					t.Fatalf("CREATE %s: %v", mailbox, err)
				}
			}
			if err := client.Subscribe(child, nil).Wait(ctx); err != nil {
				t.Fatalf("SUBSCRIBE %s: %v", child, err)
			}
			data, err := client.List("", parent, &imapclient.ListOptions{
				SelectionOptions: []imapclient.ListSelectOption{imapclient.ListSelectSubscribed, imapclient.ListSelectRecursiveMatch},
			}).Wait(ctx)
			if err != nil {
				t.Fatalf("LIST (SUBSCRIBED RECURSIVEMATCH): %v", err)
			}
			if len(data) != 1 || data[0].Mailbox != parent || !reflect.DeepEqual(data[0].ChildInfo, []string{"SUBSCRIBED"}) {
				t.Fatalf("LIST (SUBSCRIBED RECURSIVEMATCH) %q = %#v, want the parent with CHILDINFO (\"SUBSCRIBED\")", parent, data)
			}
			if err := client.Logout(ctx, nil); err != nil {
				t.Fatalf("logout: %v", err)
			}
		})
	}
}
