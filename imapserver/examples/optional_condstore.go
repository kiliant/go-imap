//go:build ignore

// CONDSTORE (RFC 7162), and with it the spoken witness and the mailbox-level
// optional interface. This is the example to read if the other two left you
// thinking "implement the interface" is the whole story.
//
// Two things differ from optional_quota.go and optional_acl.go:
//
//  1. The interface is not enough. CONDSTORE is witnessed by *name*, through
//     CapabilitySupport, because the support is spread across data the backend
//     returns — per-message modification sequences — and no type can see whether
//     those are real. Implement CondStoreMailbox and forget the witness, and the
//     capability is never advertised. Return true from the witness without the
//     data being real, and every client that trusts a modification sequence is
//     silently wrong. The framework cannot check either half for you, which is
//     exactly why the claim has to be made deliberately.
//
//  2. The interface lives on the *selected mailbox*, not the session. So the
//     wrapping happens in Select, and the two layers wrap independently.
//
// Run:
//
//	go run ./examples/optional_condstore.go ./examples/config.go
package main

import (
	"context"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
)

type condStoreSession struct {
	imapserver.Session
}

// Select wraps the mailbox the inner session returned. Without this the session
// wrapper alone would change nothing: the framework type-asserts the mailbox
// handle, and it would be holding the unwrapped one.
func (s *condStoreSession) Select(ctx context.Context, mailbox string, updater *imapserver.Updater, options *imapserver.SelectOptions) (*imapserver.SelectResult, error) {
	result, err := s.Session.Select(ctx, mailbox, updater, options)
	if err != nil {
		return nil, err
	}
	result.Mailbox = &condStoreMailbox{SelectedMailbox: result.Mailbox}
	return result, nil
}

type condStoreMailbox struct {
	imapserver.SelectedMailbox
}

var _ imapserver.CondStoreMailbox = (*condStoreMailbox)(nil)

// StoreCondStore is called in place of Store only when the client supplied
// UNCHANGEDSINCE. An unconditional STORE still goes to Store, so a backend does
// not implement the same flag logic twice.
//
// The contract worth getting right is Modified: it lists the messages left
// *unstored* because their modification sequence exceeded UnchangedSince. An
// empty set means everything was stored. Reporting a message there that was in
// fact modified — or omitting one that was not — is how a client's cached view
// silently diverges, and RFC 7162 §3.1.3 is the failure it is guarding against.
func (m *condStoreMailbox) StoreCondStore(ctx context.Context, writer *imapserver.FetchWriter, uids imap.UIDSet, flags *imapserver.StoreFlags, options *imapserver.StoreOptions) (*imapserver.CondStoreResult, error) {
	// A real backend compares each message's modification sequence against
	// options.UnchangedSince, stores the ones that pass, and reports the rest.
	// This example stores everything unconditionally, which is only honest
	// because it also reports an empty Modified set.
	if err := m.SelectedMailbox.Store(ctx, writer, uids, flags, options); err != nil {
		return nil, err
	}
	return &imapserver.CondStoreResult{
		Modified: imap.UIDSet{},
		// Zero is legitimate and means "not reported". A wrong number is worse
		// than no number: a client that trusts it will skip a resynchronisation
		// it needed.
		HighestModSeq: 0,
	}, nil
}

func main() {
	backend := newWrappedBackend(
		func(session imapserver.Session) imapserver.Session {
			return &condStoreSession{Session: session}
		},
		// The spoken half. CONDSTORE only: RFC 7162 defines QRESYNC as implying
		// CONDSTORE, so claiming QRESYNC here would also claim a Resync
		// implementation this example does not have. See QResyncMailbox, and
		// note that the witness lets a backend ship one and not the other
		// rather than forcing it to claim both.
		map[string]bool{
			"CONDSTORE": true,
		},
	)
	serveExample(backend, "CONDSTORE")
}
