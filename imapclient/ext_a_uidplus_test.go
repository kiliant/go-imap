package imapclient

import (
	"errors"
	"testing"

	"github.com/kiliant/go-imap"
)

func TestUIDExpungeSendsUIDSet(t *testing.T) {
	var got string
	c, ctx := newExtATestClient(t, "* PREAUTH [CAPABILITY IMAP4REV1 UIDPLUS] ready", func(s *extAServer) {
		s.selectInbox("INBOX", "101")
		tag, rest := s.command()
		got = rest
		s.reply("* 3 EXPUNGE", tag+" OK expunged")
	})
	if _, err := c.Select("INBOX", nil).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.UIDExpunge(imap.UIDSetRange(3000, 3002), nil).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if got != "UID EXPUNGE 3000:3002" {
		t.Fatalf("UID EXPUNGE wire form = %q", got)
	}
}

// TestUIDExpungeWithoutUIDPLUSDoesNotWiden is the destructive-path guard: plain
// EXPUNGE is not an acceptable substitute, so nothing may reach the wire.
func TestUIDExpungeWithoutUIDPLUSDoesNotWiden(t *testing.T) {
	sawCommand := false
	c, ctx := newExtATestClient(t, "* PREAUTH [CAPABILITY IMAP4REV1] ready", func(s *extAServer) {
		s.selectInbox("INBOX", "101")
		if tag, _ := s.command(); tag != "" {
			sawCommand = true
			s.ok(tag)
		}
	})
	if _, err := c.Select("INBOX", nil).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	err := c.UIDExpunge(imap.UIDSetNum(1), nil).Wait(ctx)
	if !errors.Is(err, ErrCapabilityNotAdvertised) {
		t.Fatalf("UIDExpunge without UIDPLUS error = %v", err)
	}
	var ierr *imap.Error
	if !errors.As(err, &ierr) || ierr.Type != imap.ErrorTypeProtocol {
		t.Fatalf("error is not a protocol *imap.Error: %#v", err)
	}
	if sawCommand {
		t.Fatal("a command reached the wire although UIDPLUS is absent")
	}
}

func TestParseCopyUID(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    string
		wantErr bool
		want    *CopyData
	}{
		{
			name: "rfc 4315 example",
			args: "38505 304,319:320 3956:3958",
			want: &CopyData{
				UIDValidity:     38505,
				SourceUIDs:      imap.UIDSet{{Start: 304, Stop: 304}, {Start: 319, Stop: 320}},
				DestinationUIDs: imap.UIDSet{{Start: 3956, Stop: 3958}},
			},
		},
		{name: "mismatched lengths", args: "1 1:2 5", wantErr: true},
		{name: "wildcard source", args: "1 1:* 1:2", wantErr: true},
		{name: "zero uidvalidity", args: "0 1 2", wantErr: true},
		{name: "too few fields", args: "1 2", wantErr: true},
		{name: "not a number", args: "x 1 2", wantErr: true},
		{name: "empty", args: "", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseCopyUID(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseCopyUID(%q) = %#v, want error", tc.args, got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.UIDValidity != tc.want.UIDValidity ||
				!got.SourceUIDs.Equal(tc.want.SourceUIDs) ||
				!got.DestinationUIDs.Equal(tc.want.DestinationUIDs) {
				t.Fatalf("parseCopyUID(%q) = %#v, want %#v", tc.args, got, tc.want)
			}
			if !got.Received() {
				t.Fatal("Received() = false for a parsed response code")
			}
		})
	}
}

func TestParseAppendUID(t *testing.T) {
	got, err := parseAppendUID("38505 3955")
	if err != nil {
		t.Fatal(err)
	}
	if got.UIDValidity != 38505 || !got.DestinationUIDs.Equal(imap.UIDSetNum(3955)) || !got.SourceUIDs.IsEmpty() {
		t.Fatalf("parseAppendUID = %#v", got)
	}
	// MULTIAPPEND returns a set rather than a single UID.
	got, err = parseAppendUID("38505 3955:3957")
	if err != nil {
		t.Fatal(err)
	}
	if !got.DestinationUIDs.Equal(imap.UIDSetRange(3955, 3957)) {
		t.Fatalf("parseAppendUID multiappend = %#v", got)
	}
	if _, err := parseAppendUID("38505 3955 3956"); err == nil {
		t.Fatal("parseAppendUID accepted three fields")
	}
	if (*CopyData)(nil).Received() {
		t.Fatal("Received() = true for nil")
	}
}
