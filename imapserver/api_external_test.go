package imapserver_test

import (
	"context"
	"io"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
)

type externalBackend struct{}

func (externalBackend) Authenticate(context.Context, *imapserver.ConnInfo, *imapserver.Credentials, *imapserver.AuthenticateOptions) (imapserver.Session, error) {
	return &externalSession{}, nil
}

func (externalBackend) SupportsCapability(context.Context, string, *imapserver.CapabilitySupportOptions) bool {
	return true
}

type externalSession struct{}

func (*externalSession) List(context.Context, *imapserver.ListWriter, string, []string, *imapserver.ListOptions) error {
	return nil
}
func (*externalSession) Status(context.Context, string, *imapserver.StatusOptions) (*imap.StatusData, error) {
	return &imap.StatusData{}, nil
}
func (*externalSession) Create(context.Context, string, *imapserver.CreateOptions) error {
	return nil
}
func (*externalSession) Delete(context.Context, string, *imapserver.DeleteOptions) error {
	return nil
}
func (*externalSession) Rename(context.Context, string, string, *imapserver.RenameOptions) error {
	return nil
}
func (*externalSession) Subscribe(context.Context, string, *imapserver.SubscribeOptions) error {
	return nil
}
func (*externalSession) Unsubscribe(context.Context, string, *imapserver.UnsubscribeOptions) error {
	return nil
}
func (*externalSession) Append(context.Context, string, io.Reader, *imapserver.AppendOptions) (*imap.AppendData, error) {
	return &imap.AppendData{}, nil
}
func (*externalSession) Select(context.Context, string, *imapserver.Updater, *imapserver.SelectOptions) (*imapserver.SelectResult, error) {
	return &imapserver.SelectResult{
		Mailbox: &externalSelectedMailbox{},
		Snapshot: imapserver.SelectSnapshot{
			Status:   imap.MailboxStatus{UIDNext: 1},
			Revision: "initial",
		},
	}, nil
}
func (*externalSession) Close(context.Context, *imapserver.SessionCloseOptions) error { return nil }

type externalSelectedMailbox struct{}

func (*externalSelectedMailbox) Status(context.Context, *imapserver.StatusOptions) (*imap.MailboxStatus, error) {
	return &imap.MailboxStatus{}, nil
}
func (*externalSelectedMailbox) Fetch(context.Context, *imapserver.FetchWriter, imap.UIDSet, *imapserver.FetchOptions) error {
	return nil
}
func (*externalSelectedMailbox) Search(context.Context, *imapserver.SearchQuery, *imapserver.SearchOptions) (*imapserver.SearchResult, error) {
	return &imapserver.SearchResult{}, nil
}
func (*externalSelectedMailbox) Store(context.Context, *imapserver.FetchWriter, imap.UIDSet, *imapserver.StoreFlags, *imapserver.StoreOptions) error {
	return nil
}
func (*externalSelectedMailbox) Copy(context.Context, imap.UIDSet, string, *imapserver.CopyOptions) (*imap.CopyData, error) {
	return &imap.CopyData{}, nil
}
func (*externalSelectedMailbox) Expunge(context.Context, *imapserver.ExpungeWriter, *imap.UIDSet, *imapserver.ExpungeOptions) error {
	return nil
}
func (*externalSelectedMailbox) Unselect(context.Context, *imapserver.UnselectOptions) error {
	return nil
}
func (*externalSelectedMailbox) Move(context.Context, imap.UIDSet, string, *imapserver.MoveOptions) (*imap.CopyData, error) {
	return &imap.CopyData{}, nil
}

var (
	_ imapserver.Backend           = externalBackend{}
	_ imapserver.CapabilitySupport = externalBackend{}
	_ imapserver.Session           = (*externalSession)(nil)
	_ imapserver.SelectedMailbox   = (*externalSelectedMailbox)(nil)
	_ imapserver.MoveMailbox       = (*externalSelectedMailbox)(nil)

	_ = imapserver.ConnInfo{}
	_ = imapserver.Credentials{}
	_ = imapserver.UpdateBatch{
		Before: "before",
		After:  "after",
		Origin: 1,
		Changes: []imapserver.Update{
			&imapserver.UpdateAdd{UIDs: []imap.UID{1}},
			&imapserver.UpdateFlags{UID: 1},
			&imapserver.UpdateMailboxFlags{},
			&imapserver.UpdateExpunge{UID: 1},
			&imapserver.UpdateVanished{UIDs: imap.UIDSetNum(1)},
		},
	}
)
