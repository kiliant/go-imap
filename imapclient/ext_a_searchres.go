package imapclient

import (
	"fmt"

	"github.com/kiliant/go-imap"
)

// SavedSearchResult refers to the server-side "$" marker holding the result of
// the most recent SEARCH RETURN (SAVE) on this connection. SEARCHRES,
// RFC 5182.
//
// Obtain one from [ESearchCommand.SavedResult] after the command has completed
// successfully; this type is never constructed by callers. Pass it to a command
// that accepts it — [Client.Move], [Client.MoveUID], [Client.UIDExpunge] — and
// the "$" marker is sent in place of the message set, so the server never has to
// be told a set the client would otherwise have had to download first.
//
// # Statefulness
//
// "$" is one variable per connection, not per command. RFC 5182 section 2.1
// makes that variable change under the client's feet:
//
//   - a later SEARCH RETURN (SAVE) replaces it, including one issued by another
//     goroutine sharing this [Client];
//   - a successful SELECT or EXAMINE resets it to the empty sequence, and so
//     does a new UIDVALIDITY announced while the mailbox stays open;
//   - a SEARCH RETURN (SAVE) answered NO sets it to the empty sequence, and one
//     answered [imap.CodeNotSaved] means the server refused to save at all;
//   - EXPUNGE removes the expunged messages from it.
//
// An empty "$" is not an error: RFC 5182 section 2.1 requires every command
// accepting a message set to treat it as a valid, non-matching list. A MOVE of
// an unexpectedly empty "$" therefore succeeds and moves nothing.
//
// # What this type can and cannot detect
//
// [SavedSearchResult.Valid] rejects the dangerous cases this client can observe:
// a handle belonging to another connection, a connection that has left the
// selected state, a different mailbox now being selected, and a UIDVALIDITY
// change for the saved mailbox. It cannot see a re-SELECT of the *same* mailbox
// at the same UIDVALIDITY, nor a SEARCH RETURN (SAVE) issued through some other
// path, because "$" lives on the server and the client is not told when it
// changes. Treat a handle as short-lived: save, then use, without an intervening
// mailbox command.
//
// # Pipelining
//
// RFC 5182 section 2.3 lets a client pipeline SEARCH RETURN (SAVE) with the
// commands that consume "$", because RFC 3501 section 5.5 obliges the server to
// execute them in order. This client does not exploit that: the command that
// consumes the marker is issued only after the saving command has completed, so
// the two cannot be reordered by a caller that ignores the returned error.
type SavedSearchResult struct {
	client      *Client
	mailbox     string
	uidValidity uint32
	uid         bool
}

// Mailbox reports the mailbox that was selected when the result was saved.
func (r *SavedSearchResult) Mailbox() string {
	if r == nil {
		return ""
	}
	return r.mailbox
}

// UID reports whether the saved result was produced by a UID SEARCH. RFC 5182
// section 2.1 lets "$" be consumed in either address space regardless, because
// the server reinterprets it for the consuming command, so this is
// informational rather than a constraint.
func (r *SavedSearchResult) UID() bool { return r != nil && r.uid }

// Valid reports whether this handle still refers to a marker the client has no
// reason to believe has been invalidated. See the type documentation for the
// cases it cannot see.
func (r *SavedSearchResult) Valid() bool { return r.validate() == nil }

func (r *SavedSearchResult) validate() error {
	if r == nil || r.client == nil {
		return fmt.Errorf("nil saved search result")
	}
	c := r.client
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return fmt.Errorf("the connection that saved the search result is closed")
	}
	if c.state != StateSelected {
		return fmt.Errorf("the saved search result was invalidated by leaving the selected state")
	}
	if c.selectedMailbox != r.mailbox {
		return fmt.Errorf("the saved search result belongs to mailbox %q but %q is selected", r.mailbox, c.selectedMailbox)
	}
	if r.uidValidity != 0 {
		if current := c.mailboxUIDValidity[r.mailbox]; current != 0 && current != r.uidValidity {
			return fmt.Errorf("the saved search result was invalidated by a UIDVALIDITY change for %q", r.mailbox)
		}
	}
	return nil
}

// newSavedSearchResult captures the connection state a later validity check
// compares against. It is called while the saving command is being issued, so
// the mailbox recorded is the one the command will run against.
func (c *Client) newSavedSearchResult(uid bool) *SavedSearchResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	mailbox := c.selectedMailbox
	return &SavedSearchResult{
		client:      c,
		mailbox:     mailbox,
		uidValidity: c.mailboxUIDValidity[mailbox],
		uid:         uid,
	}
}

// savedResultArgument resolves the message-set argument of a command that
// accepts either an explicit set or the "$" marker. Exactly one of them must be
// supplied: silently preferring one over the other would address a different set
// of messages than the caller asked for.
func savedResultArgument(c *Client, operation, set string, saved *SavedSearchResult) (string, error) {
	if saved == nil {
		if set == "" {
			return "", &imap.Error{Type: imap.ErrorTypeProtocol, Text: operation + " requires a non-empty message set"}
		}
		return set, nil
	}
	if set != "" {
		return "", &imap.Error{Type: imap.ErrorTypeProtocol, Text: operation + " accepts either a message set or a saved search result, not both"}
	}
	if !c.supportsAny("SEARCHRES") {
		return "", capabilityError(operation+" with a saved search result", "SEARCHRES")
	}
	if saved.client != c {
		return "", &imap.Error{Type: imap.ErrorTypeProtocol, Text: operation + " was given a saved search result from another connection"}
	}
	if err := saved.validate(); err != nil {
		return "", &imap.Error{Type: imap.ErrorTypeProtocol, Text: operation + ": " + err.Error()}
	}
	return "$", nil
}
