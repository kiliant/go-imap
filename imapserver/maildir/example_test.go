package maildir_test

import (
	"context"
	"fmt"
	"os"

	"github.com/kiliant/go-imap/imapserver"
	"github.com/kiliant/go-imap/imapserver/maildir"
)

func ExampleOpen() {
	ctx := context.Background()
	root, err := os.MkdirTemp("", "maildir-example-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(root)
	backend, err := maildir.Open(ctx, root, &maildir.Options{Accounts: []maildir.AccountOptions{{Username: "alice", Password: "example-only"}}})
	if err != nil {
		panic(err)
	}
	defer backend.Close(ctx, nil)
	server := imapserver.New(backend, nil)
	defer server.Close(ctx, nil)
	// Serve with server.Serve or ServeTLS; shut down server before backend.
	fmt.Println("persistent backend ready")
	// Output: persistent backend ready
}
