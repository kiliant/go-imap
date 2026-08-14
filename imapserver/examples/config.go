//go:build ignore

// Shared configuration for the server examples, so each program can be read as
// the one thing it demonstrates rather than as environment plumbing.
//
// Every example compiles as `go run ./examples/<name>.go ./examples/config.go`.
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

// serverAddr is where the example server listens. Port 1143 rather than 143:
// binding a privileged port needs root, and an example that has to be run as
// root is an example nobody runs.
func serverAddr() string {
	if addr := os.Getenv("IMAP_LISTEN"); addr != "" {
		return addr
	}
	return "localhost:1143"
}

func serverUser() string {
	if user := os.Getenv("IMAP_USER"); user != "" {
		return user
	}
	return "user"
}

func serverPassword() string {
	if password := os.Getenv("IMAP_PASS"); password != "" {
		return password
	}
	return "secret"
}

// The optional-interface examples share the two helpers below, so each of them
// can be read as the one interface it demonstrates.

// wrappedBackend delegates authentication to the memory backend and hands the
// session it returns to wrap. That is how an example adds one optional
// interface without reimplementing the ten mandatory ones.
//
// It also demonstrates the trap that comes with the technique, and it is worth
// stating plainly because it is the first thing that goes wrong for real:
// **a wrapper hides every optional interface the wrapped session implements.**
// The framework discovers support by type-asserting the value it holds, and it
// holds the wrapper. memory implements two dozen optional interfaces; a session
// wrapped in a type that implements one of them supports exactly one. Forward
// what you mean to keep, or embed the concrete type rather than the interface.
type wrappedBackend struct {
	inner imapserver.Backend
	wrap  func(imapserver.Session) imapserver.Session

	// capabilities is consulted by the spoken witness, CapabilitySupport, which
	// only some capabilities use. See optional_condstore.go.
	capabilities map[string]bool
}

func newWrappedBackend(wrap func(imapserver.Session) imapserver.Session, capabilities map[string]bool) *wrappedBackend {
	return &wrappedBackend{
		inner: memory.New(&memory.Options{
			Users: map[string]string{serverUser(): serverPassword()},
		}),
		wrap:         wrap,
		capabilities: capabilities,
	}
}

func (b *wrappedBackend) Authenticate(ctx context.Context, conn *imapserver.ConnInfo, credentials *imapserver.Credentials, options *imapserver.AuthenticateOptions) (imapserver.Session, error) {
	session, err := b.inner.Authenticate(ctx, conn, credentials, options)
	if err != nil {
		return nil, err
	}
	return b.wrap(session), nil
}

// SupportsCapability implements [imapserver.CapabilitySupport], the spoken
// witness. A backend that does not recognise a name must return false: the
// framework will not advertise a capability this declines, and must not
// advertise one it has never heard of.
func (b *wrappedBackend) SupportsCapability(name string) bool {
	return b.capabilities[name]
}

// serveExample runs backend until interrupted. Every optional-interface example
// ends with a call to it.
func serveExample(backend imapserver.Backend, what string) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	server := imapserver.New(backend, &imapserver.Options{
		Greeting:          "go-imap example server (" + what + ")",
		AllowInsecureAuth: true,
	})

	listener, err := net.Listen("tcp", serverAddr())
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		os.Exit(1)
	}
	fmt.Printf("listening on %s as %s — demonstrating %s\n", listener.Addr(), serverUser(), what)

	if err := server.Serve(ctx, listener); err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "serve:", err)
		os.Exit(1)
	}
}
