//go:build ignore

// QUOTA (RFC 9208), and with it the structural witness.
//
// QuotaSession is discovered by type assertion and nothing else. Implement the
// interface and the framework advertises QUOTA; do not, and it does not. There
// is no registration call and no capability string to remember, which is the
// point: the type system decides, so a backend cannot advertise a capability it
// has not implemented.
//
// Compare optional_condstore.go, where the interface is not enough.
//
// Run:
//
//	go run ./examples/optional_quota.go ./examples/config.go
package main

import (
	"context"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
)

// quotaSession is a Session that also answers QUOTA. Embedding the Session
// interface supplies the ten mandatory methods; the two below are the whole of
// the addition.
type quotaSession struct {
	imapserver.Session
}

// Compile-time proof that the interface is satisfied. Without it, a signature
// that drifts produces a server that silently stops advertising QUOTA rather
// than a build failure — the assertion is not decoration.
var _ imapserver.QuotaSession = (*quotaSession)(nil)

// QuotaRoots answers which roots apply to a mailbox. RFC 9208 §4.1.2 allows the
// answer to be none, so returning an empty slice is a legitimate reply and not
// an error.
func (s *quotaSession) QuotaRoots(ctx context.Context, mailbox string, options *imapserver.QuotaOptions) ([]string, error) {
	return []string{""}, nil
}

// GetQuota answers one named root. The numbers here are fixed for the sake of
// the example; a real backend reports what it is actually enforcing, because a
// client that is told it has room and then gets NO on APPEND has been lied to.
func (s *quotaSession) GetQuota(ctx context.Context, root string, options *imapserver.QuotaOptions) (*imap.QuotaData, error) {
	return &imap.QuotaData{
		Root: root,
		Resources: []imap.QuotaResource{
			{Name: imap.QuotaResourceStorage, Usage: 512, Limit: 1024 * 1024},
			{Name: imap.QuotaResourceMessage, Usage: 3, Limit: 10000},
		},
	}, nil
}

func main() {
	backend := newWrappedBackend(
		func(session imapserver.Session) imapserver.Session {
			return &quotaSession{Session: session}
		},
		// No entry needed: QUOTA is witnessed structurally, by the type
		// assertion above rather than by name.
		nil,
	)
	serveExample(backend, "QUOTA")
}
