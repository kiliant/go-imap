package imapserver_test

import (
	"bytes"
	"context"
	"slices"
	"testing"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
	"github.com/kiliant/go-imap/imapserver/memory"
)

// spellingBackend answers FETCH with keys in the spellings a conforming
// backend may use, since item keys are case-insensitive.
type spellingBackend struct{ *memory.Backend }

type spellingSession struct{ imapserver.Session }

type spellingMailbox struct{ imapserver.SelectedMailbox }

func (b *spellingBackend) Authenticate(ctx context.Context, info *imapserver.ConnInfo, credentials *imapserver.Credentials, options *imapserver.AuthenticateOptions) (imapserver.Session, error) {
	session, err := b.Backend.Authenticate(ctx, info, credentials, options)
	if err != nil {
		return nil, err
	}
	return &spellingSession{session}, nil
}

func (s *spellingSession) Select(ctx context.Context, name string, updater *imapserver.Updater, options *imapserver.SelectOptions) (*imapserver.SelectResult, error) {
	result, err := s.Session.Select(ctx, name, updater, options)
	if err == nil {
		result.Mailbox = &spellingMailbox{result.Mailbox}
	}
	return result, err
}

func (m *spellingMailbox) Fetch(ctx context.Context, writer *imapserver.FetchWriter, uids imap.UIDSet, options *imapserver.FetchOptions) error {
	return writer.WriteMessage(ctx, &imap.FetchMessageData{Items: map[imap.FetchDataKey][]imap.FetchData{
		"uid":         {imap.FetchDataUID(1)},
		"flags":       {imap.FetchDataFlags{imap.FlagSeen}},
		"Flags":       {imap.FetchDataFlags{imap.FlagFlagged}},
		"rfc822.Size": {imap.FetchDataRFC822Size(42)},
		// The section keyword is upper-cased; the field list keeps its
		// spelling, since it is the backend's echo of the client's names.
		"body[header.fields (Subject x-Mixed)]": {&imap.FetchDataBodySection{Literal: bytes.NewReader([]byte("Subject: x\r\n\r\n"))}},
		"Body[1.mime]<0>":                       {&imap.FetchDataBodySection{Literal: bytes.NewReader([]byte("abc"))}},
		"binary.size[1]":                        {imap.FetchDataBinarySectionSize(3)},
	}})
}

// Backend item keys are written in upper case up to their first space, and
// values under keys that differ only in case are all sent (#200).
func TestFetchKeysAreWrittenInUpperCase(t *testing.T) {
	w := r4Open(t, &spellingBackend{r4Memory()}, nil)
	r4Append(w, "INBOX", "", "one")
	w.cmd("S", "SELECT INBOX")
	lines := w.cmd("F", "UID FETCH 1 (FLAGS)")
	want := `* 1 FETCH (BINARY.SIZE[1] 3 BODY[1.MIME]<0> {3}<3 octets> ` +
		`BODY[HEADER.FIELDS (Subject x-Mixed)] {14}<14 octets> ` +
		`FLAGS (\Flagged) FLAGS (\Seen) RFC822.SIZE 42 UID 1)`
	if !slices.Contains(lines, want) || !r4Has(lines, `^F OK`) {
		t.Fatalf("UID FETCH = %q\nwant line %q", lines, want)
	}
}
