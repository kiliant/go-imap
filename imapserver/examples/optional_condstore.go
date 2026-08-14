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
// It is also the example that shows how to climb out of the wrapper trap
// config.go describes: it forwards to the wrapped mailbox's own implementation
// rather than reimplementing the comparison, because a wrapper hides every
// optional interface it does not forward.
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
// This forwards to the wrapped mailbox, which is the second thing worth
// demonstrating: the wrapper hides the inner value's optional interfaces from
// the framework, so anything it means to keep it must forward explicitly. That
// is the trap config.go describes, and this is what climbing out of it looks
// like.
//
// **A conditional store must actually compare.** The contract is that Modified
// lists the messages left *unstored* because their modification sequence
// exceeded options.UnchangedSince; an empty set means everything was stored.
// Delegating to the unconditional Store and reporting an empty Modified set
// would satisfy the compiler and silently break the extension: RFC 7162's own
// Example 8 uses UNCHANGEDSINCE 0 as a probe that must always fail, which is
// how a client tests atomically for a keyword, and answering "stored, nothing
// modified" turns that probe into an unconditional store of exactly the
// messages it was meant to protect.
//
// Note options.HasUnchangedSince: zero is a real modification sequence and the
// one the probe uses, so presence is carried separately from value.
func (m *condStoreMailbox) StoreCondStore(ctx context.Context, writer *imapserver.FetchWriter, uids imap.UIDSet, flags *imapserver.StoreFlags, options *imapserver.StoreOptions) (*imapserver.CondStoreResult, error) {
	inner, ok := m.SelectedMailbox.(imapserver.CondStoreMailbox)
	if !ok {
		// Refuse rather than degrade. A backend that cannot compare
		// modification sequences has no business witnessing CONDSTORE, and
		// answering as though it had is the failure this whole example is
		// about.
		return nil, &imap.Error{
			Type: imap.ErrorTypeNo,
			Text: "conditional STORE is not supported by this mailbox",
		}
	}
	return inner.StoreCondStore(ctx, writer, uids, flags, options)
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
