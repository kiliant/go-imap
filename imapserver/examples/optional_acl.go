//go:build ignore

// ACL (RFC 4314), and with it the reason an optional interface is sometimes
// three methods rather than one.
//
// ACLSession is witnessed structurally, exactly like QuotaSession in
// optional_quota.go. What it adds is a lesson in why the method set is shaped
// the way it is: MyRights is separate from GetACL because RFC 4314 §4 lets a
// user ask what they themselves may do without holding the "a" right needed to
// read the whole list. Folding the two together would make the cheap, common
// question require the rare, privileged answer.
//
// Run:
//
//	go run ./examples/optional_acl.go ./examples/config.go
package main

import (
	"context"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
)

type aclSession struct {
	imapserver.Session

	// The identity this session authenticated as. A real backend needs it for
	// MyRights, which is a question about the caller and not about the mailbox.
	user string
}

var _ imapserver.ACLSession = (*aclSession)(nil)

// The rights letters of RFC 4314 §2.1. Spelling them out beats a bare string
// literal at each use, where a missing letter is invisible.
const (
	rightLookup  = "l"
	rightRead    = "r"
	rightSeen    = "s"
	rightWrite   = "w"
	rightInsert  = "i"
	rightPost    = "p"
	rightCreate  = "k"
	rightDelete  = "x"
	rightExpunge = "e"
	rightAdmin   = "a"

	rightsOwner = rightLookup + rightRead + rightSeen + rightWrite + rightInsert +
		rightPost + rightCreate + rightDelete + rightExpunge + rightAdmin
	rightsReadOnly = rightLookup + rightRead + rightSeen
)

// GetACL answers the whole list. RFC 4314 §4 requires the "a" right to call it,
// and a backend that cannot check that should return an error rather than
// disclose the list — the framework does not know who may see what.
func (s *aclSession) GetACL(ctx context.Context, mailbox string, options *imapserver.ACLOptions) (*imap.ACLData, error) {
	return &imap.ACLData{
		Mailbox: mailbox,
		Entries: []imap.ACLEntry{
			{Identifier: s.user, Rights: imap.ACLRights(rightsOwner)},
			{Identifier: "anyone", Rights: imap.ACLRights(rightsReadOnly)},
		},
	}, nil
}

// MyRights answers only for the authenticated user, which is why it needs no
// privilege of its own.
func (s *aclSession) MyRights(ctx context.Context, mailbox string, options *imapserver.ACLOptions) (imap.ACLRights, error) {
	return imap.ACLRights(rightsOwner), nil
}

// ListRights answers which rights an identifier may hold, not which it does
// hold. Required is granted unconditionally; each element of Optional is a set
// that may be granted individually.
func (s *aclSession) ListRights(ctx context.Context, mailbox, identifier string, options *imapserver.ACLOptions) (*imap.ListRightsData, error) {
	return &imap.ListRightsData{
		Mailbox:    mailbox,
		Identifier: identifier,
		Required:   imap.ACLRights(rightLookup),
		Optional: []imap.ACLRights{
			imap.ACLRights(rightRead),
			imap.ACLRights(rightSeen),
			imap.ACLRights(rightWrite),
			imap.ACLRights(rightInsert),
		},
	}, nil
}

func main() {
	backend := newWrappedBackend(
		func(session imapserver.Session) imapserver.Session {
			return &aclSession{Session: session, user: serverUser()}
		},
		nil, // structural witness; nothing to declare by name
	)
	serveExample(backend, "ACL")
}
