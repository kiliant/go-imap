// Package interop puts this repository's own server into the interoperability
// matrix, as a first-class entry alongside Dovecot, Stalwart and the rest.
//
// # Why it lives here and not under interop/servers/
//
// Every other profile describes a container, so it can be a data literal in the
// root module and name nothing. This one has to construct an
// [imapserver.Server], which means importing imapserver — and T25 makes
// imapserver a nested module that requires the root module back. A profile
// under interop/servers/goimap would therefore put the root module in a
// requirement cycle with its own submodule, and make each release depend on the
// other having been published first.
//
// Hosting it inside the imapserver tree keeps the dependency one-directional:
// imapserver imports the root module's harness, and the root module imports
// nothing of imapserver's. Nothing in the harness registry references this
// package; [harness.Run] takes profiles as an argument, so main_test.go passes
// this one in directly.
//
// # What the entry buys
//
// Loopback tests already prove our client and our server agree. They cannot
// catch the two agreeing on a misreading of the RFC, because the same
// assumption is compiled into both halves. Running our server through the same
// seeding, the same capability assertions and the same table as five
// third-party servers is what makes that comparison possible.
package interop

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/kiliant/go-imap/imapserver"
	"github.com/kiliant/go-imap/imapserver/memory"
	"github.com/kiliant/go-imap/interop/definition"
)

// The credentials and mailbox layout the harness seeds with. They are the
// harness' own constants; a mismatch here shows up as a failed LOGIN during
// provisioning rather than as a skipped row.
const (
	interopUser     = "interop@example.test"
	interopPassword = "interop-pw"
)

// Profile is this server, run in the test process.
//
// ExpectedCapabilities is deliberately a floor rather than the full advertised
// set. The harness fails a profile that promises something the live server does
// not advertise, so listing everything would turn every capability change into
// an unrelated interop failure; what matters is that the base protocol and the
// extensions other entries are compared on never silently disappear.
//
// The list holds what this server advertises to an authenticated session, which
// is the state the harness measures. Four things a reader might expect are
// absent, and none of them is absent by oversight:
//
//   - AUTH=PLAIN and STARTTLS are pre-authentication capabilities. They are in
//     the greeting and gone from the post-LOGIN set, which is correct.
//     STARTTLS additionally needs a TLSConfig this profile does not set.
//   - UIDPLUS (RFC 4315) is never advertised. The server emits APPENDUID and
//     COPYUID response codes, but UID EXPUNGE is not in the UID subcommand
//     table, so the capability would be a false claim. RFC-COVERAGE.md records
//     the server side as done; that overstates it. Reported by T24.
//   - IMAP4REV2 is never advertised: frameworkRev2 is hardcoded false and
//     nothing sets it, so no client can ENABLE the rev2 behaviour the rest of
//     the package implements. Reported by T24.
//
// THREAD is listed under the bare token the server actually sends. RFC 5256
// defines the capability as THREAD=<algorithm> and has no bare form, so this
// entry is pinning a known-wrong string on purpose: changing it belongs with
// the fix, and until then the profile must describe what is really on the wire.
var Profile = definition.Profile{
	Name:       "goimap",
	Tier:       definition.TierInProcess,
	FirstParty: true,
	Native:     start,
	ExpectedCapabilities: []string{
		"IMAP4rev1",
		"NAMESPACE",
		"UNSELECT",
		"ESEARCH",
		"SEARCHRES",
		"LIST-EXTENDED",
		"LIST-STATUS",
		"SPECIAL-USE",
		"CHILDREN",
		"CONDSTORE",
		"QRESYNC",
		"MOVE",
		"IDLE",
		"ENABLE",
		"SORT",
		"THREAD",
		"QUOTA",
		"ACL",
		"METADATA",
		"BINARY",
		"MULTIAPPEND",
		"CATENATE",
		"OBJECTID",
		"SAVEDATE",
		"PREVIEW",
		"LITERAL-",
	},
}

// start runs a server on an ephemeral loopback port.
func start(ctx context.Context) (*definition.NativeServer, error) {
	backend := memory.New(&memory.Options{
		Users: map[string]string{interopUser: interopPassword},
	})
	server := imapserver.New(backend, &imapserver.Options{
		// The harness authenticates in cleartext, as it does against every
		// other profile's plain IMAP port.
		AllowInsecureAuth: true,
		Greeting:          "go-imap interop",
		Limits: imapserver.Limits{
			// The canonical corpus includes a 5 MiB message and the default
			// literal bound is 64 KiB. Raising it here is configuration, not a
			// test routing around a limit it is validating: the bound under
			// test in this suite is protocol conformance, and every other
			// server in the matrix accepts the same fixture.
			MaxLiteralBytes: 8 << 20,
		},
	})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}

	// Serve runs until the listener closes. Its error is kept rather than
	// dropped so a failure that happens after startup still reaches the
	// diagnostics the harness prints, instead of vanishing into a goroutine.
	var (
		mu       sync.Mutex
		serveErr error
	)
	serveCtx, cancelServe := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := server.Serve(serveCtx, listener); err != nil {
			mu.Lock()
			serveErr = err
			mu.Unlock()
		}
	}()

	stop := func(stopCtx context.Context) error {
		cancelServe()
		_ = listener.Close()
		err := server.Close(stopCtx)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			return fmt.Errorf("goimap: Serve did not return")
		}
		return err
	}
	logs := func() string {
		mu.Lock()
		defer mu.Unlock()
		if serveErr == nil {
			return "(no serve error)"
		}
		return "serve: " + serveErr.Error()
	}

	return &definition.NativeServer{
		Address: listener.Addr().String(),
		Stop:    stop,
		Logs:    logs,
	}, nil
}
