package imapserver

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

type snapshotSession struct {
	stubSession
	snapshot SelectSnapshot
	mailbox  trackedSelectedMailbox
	updater  *Updater
}

func (s *snapshotSession) Select(_ context.Context, _ string, updater *Updater, _ *SelectOptions) (*SelectResult, error) {
	s.updater = updater
	return &SelectResult{Mailbox: &s.mailbox, Snapshot: s.snapshot}, nil
}

func snapshotConnection(t *testing.T, session *snapshotSession, wire *bytes.Buffer) *conn {
	t.Helper()
	state := newSessionState(true)
	if !state.authenticate(session) {
		t.Fatal("failed to authenticate fixture")
	}
	return &conn{
		ctx: context.Background(), state: state, server: New(nil, nil),
		encoder: imapwire.NewEncoder(wire, &imapwire.EncoderOptions{ServerResponse: true}),
	}
}

func TestSelectSnapshotStatusDrivesWireAndCheckpoint(t *testing.T) {
	session := &snapshotSession{snapshot: SelectSnapshot{
		UIDs: []imap.UID{3, 7}, Revision: "r1",
		Status: imap.MailboxStatus{
			Mailbox: "INBOX", NumMessages: 2, NumRecent: 1, UIDValidity: 5, UIDNext: 8, Unseen: 2,
			Flags: []imap.Flag{imap.FlagSeen}, PermanentFlags: []imap.Flag{imap.FlagSeen, imap.FlagWildcard},
			HighestModSeq: 10, ReadOnly: true,
		},
	}}
	var wire bytes.Buffer
	c := snapshotConnection(t, session, &wire)
	selected, snapshot, err := selectAtomic(context.Background(), c, "INBOX", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer selected.close()
	if selected.snapshotModSeq != 10 || !selected.readOnly || selected.noModSeq {
		t.Fatalf("retained checkpoint/access mode = %d/%t/%t", selected.snapshotModSeq, selected.readOnly, selected.noModSeq)
	}
	// The old snapshot carried a second, conflicting HIGHESTMODSEQ. Both the
	// wire value and the safe resynchronisation checkpoint now read Status.
	if err := writeSelectSnapshot(c, snapshot); err != nil {
		t.Fatal(err)
	}
	for _, response := range []string{
		"* FLAGS (\\Seen)\r\n", "* 2 EXISTS\r\n", "* 1 RECENT\r\n",
		"[PERMANENTFLAGS (\\Seen \\*)]", "[UIDVALIDITY 5]", "[UIDNEXT 8]", "[UNSEEN 2]", "[HIGHESTMODSEQ 10]",
	} {
		if !strings.Contains(wire.String(), response) {
			t.Errorf("SELECT missing %q:\n%s", response, wire.String())
		}
	}
	// Backend-owned slices cannot change either the retained selection or the
	// response snapshot after the atomic Select call has returned.
	session.snapshot.UIDs[0] = 99
	session.snapshot.Status.Flags[0] = imap.FlagDeleted
	session.snapshot.Status.PermanentFlags[0] = imap.FlagDeleted
	if !slices.Equal(selected.uids, []imap.UID{3, 7}) || !slices.Equal(snapshot.UIDs, selected.uids) ||
		!slices.Equal(selected.flags, []imap.Flag{imap.FlagSeen}) || !slices.Equal(snapshot.Status.Flags, selected.flags) ||
		!slices.Equal(snapshot.Status.PermanentFlags, []imap.Flag{imap.FlagSeen, imap.FlagWildcard}) {
		t.Fatal("backend mutation changed retained SELECT values")
	}
}

func TestSelectSnapshotCountMismatchClosesAttachment(t *testing.T) {
	session := &snapshotSession{snapshot: SelectSnapshot{
		UIDs: []imap.UID{1}, Revision: "r1",
		Status: imap.MailboxStatus{NumMessages: 2, UIDValidity: 1, UIDNext: 2},
	}}
	var wire bytes.Buffer
	c := snapshotConnection(t, session, &wire)
	if _, _, err := selectAtomic(context.Background(), c, "INBOX", nil); err == nil {
		t.Fatal("accepted mismatched message count")
	}
	if session.mailbox.unselected != 1 || c.state.selected != nil || c.state.state != stateAuthenticated {
		t.Fatal("rejected SELECT retained its backend handle or connection selection")
	}
	if err := session.updater.Push(&UpdateBatch{}); !errors.Is(err, ErrUpdaterClosed) {
		t.Fatalf("updater after rejected SELECT = %v", err)
	}
}

// A future SELECT item belongs in shared MailboxStatus, not in a second field
// whose precedence and update-checkpoint meaning differ from the shared one.
func TestSelectSnapshotDoesNotDuplicateStatusFields(t *testing.T) {
	status := reflect.TypeFor[imap.MailboxStatus]()
	snapshot := reflect.TypeFor[SelectSnapshot]()
	for i := range status.NumField() {
		field := status.Field(i)
		if field.IsExported() {
			if _, duplicated := snapshot.FieldByName(field.Name); duplicated {
				t.Errorf("SelectSnapshot.%s duplicates Status.%s", field.Name, field.Name)
			}
		}
	}
}
