//go:build ignore

// STARTTLS and implicit TLS, and why AllowInsecureAuth is false everywhere here.
//
// The other examples set AllowInsecureAuth so they can be run without a
// certificate. This one is what a server anyone else can reach should look
// like: with a TLSConfig set and AllowInsecureAuth left false, the framework
// advertises LOGINDISABLED before TLS and withdraws it after — so a client is
// told, in the protocol rather than in documentation, that it must not send a
// password yet.
//
// Both forms are shown because deployments need both: STARTTLS on 143 upgrades
// in-band, implicit TLS on 993 is TLS from the first byte. They are the same
// server value serving two listeners.
//
// Run, after generating a self-signed pair:
//
//	go run ./examples/tls.go ./examples/config.go server.crt server.key
package main

import (
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"os/signal"

	"context"

	"github.com/kiliant/go-imap/imapserver"
	"github.com/kiliant/go-imap/imapserver/memory"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: tls.go <cert> <key>")
		os.Exit(2)
	}
	certificate, err := tls.LoadX509KeyPair(os.Args[1], os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, "certificate:", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	backend := memory.New(&memory.Options{
		Users: map[string]string{serverUser(): serverPassword()},
	})

	tlsConfig := &tls.Config{Certificates: []tls.Certificate{certificate}}
	server := imapserver.New(backend, &imapserver.Options{
		Greeting: "go-imap example server (TLS)",
		// New clones this and enforces a TLS 1.2 floor, so a config that
		// permits older versions cannot silently weaken the server.
		TLSConfig: tlsConfig,
		// Left false. This is the line that makes LOGINDISABLED appear before
		// STARTTLS: a cleartext client is refused rather than trusted.
		//
		// RequireTLS goes further and refuses cleartext authentication even if
		// something later sets AllowInsecureAuth. Set it when the deployment
		// has no reason ever to accept a cleartext password.
		AllowInsecureAuth: false,
	})

	// STARTTLS: a plain listener the client upgrades in-band.
	plain, err := net.Listen("tcp", serverAddr())
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		os.Exit(1)
	}

	// Implicit TLS: TLS is already established when Serve sees the connection.
	// The server needs no separate mode for this — it reads the negotiated
	// state off the connection either way, which is why one Options serves
	// both listeners.
	implicit, err := tls.Listen("tcp", implicitTLSAddr(), tlsConfig)
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen (implicit TLS):", err)
		os.Exit(1)
	}

	fmt.Println("STARTTLS on", plain.Addr(), "— implicit TLS on", implicit.Addr())

	errs := make(chan error, 2)
	go func() { errs <- server.Serve(ctx, plain, nil) }()
	go func() { errs <- server.Serve(ctx, implicit, nil) }()

	// Either listener failing takes the server down: a deployment that silently
	// loses its TLS port and keeps the plain one is worse than one that stops.
	if err := <-errs; err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "serve:", err)
		os.Exit(1)
	}
}

func implicitTLSAddr() string {
	if addr := os.Getenv("IMAP_LISTEN_TLS"); addr != "" {
		return addr
	}
	return "localhost:9993"
}
