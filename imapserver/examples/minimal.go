//go:build ignore

// A working IMAP server in about twenty lines, backed by imapserver/memory.
//
// This is the "hello world" a new backend author starts from: run it, point a
// real client at it, and watch the protocol work before writing any backend
// code of your own. memory is supported rather than a toy — it is the backend
// this project's own conformance and interoperability suites run against.
//
// Run:
//
//	go run ./examples/minimal.go ./examples/config.go
//
// Then, from another terminal:
//
//	IMAP_ADDR=localhost:1143 IMAP_USER=user IMAP_PASS=secret \
//	  go run ../examples/connect_auth_list.go ../examples/config.go
package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"

	"github.com/kiliant/go-imap/imapserver"
	"github.com/kiliant/go-imap/imapserver/memory"
)

func main() {
	// Ctrl-C cancels the context, which is what stops Serve. A server that can
	// only be stopped by killing the process is a server you cannot embed.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	backend := memory.New(&memory.Options{
		Users: map[string]string{serverUser(): serverPassword()},
	})

	server := imapserver.New(backend, &imapserver.Options{
		Greeting: "go-imap example server",
		// Cleartext LOGIN, because this example has no certificate. Any server
		// reachable by anyone else wants TLSConfig set instead; see tls.go,
		// where leaving this false is the entire point.
		AllowInsecureAuth: true,
	})

	listener, err := net.Listen("tcp", serverAddr())
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		os.Exit(1)
	}
	fmt.Println("listening on", listener.Addr(), "as", serverUser())

	// Serve returns when ctx is cancelled or the listener fails permanently.
	// A cancelled context is the ordinary way to stop, not an error.
	if err := server.Serve(ctx, listener, nil); err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "serve:", err)
		os.Exit(1)
	}
}
