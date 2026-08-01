package imapclient

import (
	"errors"
	"testing"

	"github.com/kiliant/go-imap"
)

func TestMoveUIDCollectsUntaggedCopyUID(t *testing.T) {
	var sent string
	c, ctx := newExtATestClient(t, "* PREAUTH [CAPABILITY IMAP4REV1 MOVE UIDPLUS] ready", func(s *extAServer) {
		s.selectInbox("INBOX", "101")
		tag, rest := s.command()
		sent = rest
		// RFC 6851 section 4.3: COPYUID arrives in an untagged OK before the
		// EXPUNGE responses. Observed in this form on Dovecot, Stalwart,
		// Cyrus and GreenMail.
		s.reply("* OK [COPYUID 432432 42:69 1202:1229] moved", "* 22 EXPUNGE", tag+" OK done")
	})
	if _, err := c.Select("INBOX", nil).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	data, err := c.MoveUID(ctx, imap.UIDSetRange(42, 69), "Archive", nil)
	if err != nil {
		t.Fatal(err)
	}
	if sent != "UID MOVE 42:69 Archive" {
		t.Fatalf("wire form = %q", sent)
	}
	if data.Emulated || data.ExpungedEveryDeletedMessage {
		t.Fatalf("native MOVE reported as emulated: %#v", data)
	}
	if !data.UIDPlus.Received() || data.UIDPlus.UIDValidity != 432432 {
		t.Fatalf("COPYUID = %#v", data.UIDPlus)
	}
	if !data.UIDPlus.SourceUIDs.Equal(imap.UIDSetRange(42, 69)) ||
		!data.UIDPlus.DestinationUIDs.Equal(imap.UIDSetRange(1202, 1229)) {
		t.Fatalf("COPYUID sets = %#v", data.UIDPlus)
	}
}

func TestMoveWithoutCopyUIDLeavesUIDDataZero(t *testing.T) {
	c, ctx := newExtATestClient(t, "* PREAUTH [CAPABILITY IMAP4REV1 MOVE] ready", func(s *extAServer) {
		s.selectInbox("INBOX", "101")
		tag, _ := s.command()
		s.reply("* 1 EXPUNGE", tag+" OK done")
	})
	if _, err := c.Select("INBOX", nil).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	data, err := c.Move(ctx, imap.SeqSetNum(1), "Archive", nil)
	if err != nil {
		t.Fatal(err)
	}
	if data.UIDPlus.Received() {
		t.Fatalf("UIDPlus reported without a COPYUID response code: %#v", data.UIDPlus)
	}
}

// TestMoveWithoutMOVERequiresOptIn keeps the non-atomic emulation behind an
// explicit choice.
func TestMoveWithoutMOVERequiresOptIn(t *testing.T) {
	sawCommand := false
	c, ctx := newExtATestClient(t, "* PREAUTH [CAPABILITY IMAP4REV1 UIDPLUS] ready", func(s *extAServer) {
		s.selectInbox("INBOX", "101")
		if tag, _ := s.command(); tag != "" {
			sawCommand = true
			s.ok(tag)
		}
	})
	if _, err := c.Select("INBOX", nil).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	_, err := c.MoveUID(ctx, imap.UIDSetNum(1), "Archive", nil)
	if !errors.Is(err, ErrCapabilityNotAdvertised) {
		t.Fatalf("MoveUID without MOVE error = %v", err)
	}
	if sawCommand {
		t.Fatal("a command reached the wire although MOVE is absent and no fallback was requested")
	}
}

func TestMoveUIDEmulationUsesUIDExpunge(t *testing.T) {
	var commands []string
	c, ctx := newExtATestClient(t, "* PREAUTH [CAPABILITY IMAP4REV1 UIDPLUS] ready", func(s *extAServer) {
		s.selectInbox("INBOX", "101")
		for range 3 {
			tag, rest := s.command()
			commands = append(commands, rest)
			s.ok(tag)
		}
	})
	if _, err := c.Select("INBOX", nil).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	data, err := c.MoveUID(ctx, imap.UIDSetRange(42, 69), "Archive", &MoveOptions{AllowNonAtomicFallback: true})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"UID COPY 42:69 Archive",
		`UID STORE 42:69 +FLAGS.SILENT (\Deleted)`,
		"UID EXPUNGE 42:69",
	}
	assertCommands(t, commands, want)
	if !data.Emulated {
		t.Fatal("Emulated not set on the fallback path")
	}
	if data.ExpungedEveryDeletedMessage {
		t.Fatal("UID EXPUNGE was used but the wide-expunge warning was set")
	}
	if data.UIDPlus.Received() {
		t.Fatal("emulated MOVE reported COPYUID though the scripted COPY OK carried none")
	}
}

// TestMoveEmulationResolvesSequenceNumbersToUIDs covers the path where the
// server has UIDPLUS but not MOVE and the caller addressed by sequence number:
// UID EXPUNGE needs UIDs, and they must be read before anything renumbers.
func TestMoveEmulationResolvesSequenceNumbersToUIDs(t *testing.T) {
	var commands []string
	c, ctx := newExtATestClient(t, "* PREAUTH [CAPABILITY IMAP4REV1 UIDPLUS] ready", func(s *extAServer) {
		s.selectInbox("INBOX", "101")
		tag, rest := s.command()
		commands = append(commands, rest)
		s.reply("* 1 FETCH (UID 300)", "* 2 FETCH (UID 301)", tag+" OK fetched")
		for range 3 {
			tag, rest := s.command()
			commands = append(commands, rest)
			s.ok(tag)
		}
	})
	if _, err := c.Select("INBOX", nil).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	data, err := c.Move(ctx, imap.SeqSetRange(1, 2), "Archive", &MoveOptions{AllowNonAtomicFallback: true})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"FETCH 1:2 (UID)",
		"COPY 1:2 Archive",
		`STORE 1:2 +FLAGS.SILENT (\Deleted)`,
		"UID EXPUNGE 300:301",
	}
	assertCommands(t, commands, want)
	if data.ExpungedEveryDeletedMessage {
		t.Fatal("wide-expunge warning set although UID EXPUNGE was used")
	}
}

// TestMoveEmulationWithoutUIDPLUSWidensAndSaysSo is the most destructive path
// in this task: a bare EXPUNGE can remove messages this client never selected.
func TestMoveEmulationWithoutUIDPLUSWidensAndSaysSo(t *testing.T) {
	var commands []string
	c, ctx := newExtATestClient(t, "* PREAUTH [CAPABILITY IMAP4REV1] ready", func(s *extAServer) {
		s.selectInbox("INBOX", "101")
		for range 3 {
			tag, rest := s.command()
			commands = append(commands, rest)
			s.ok(tag)
		}
	})
	if _, err := c.Select("INBOX", nil).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	data, err := c.Move(ctx, imap.SeqSetNum(4), "Archive", &MoveOptions{AllowNonAtomicFallback: true})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"COPY 4 Archive",
		`STORE 4 +FLAGS.SILENT (\Deleted)`,
		"EXPUNGE",
	}
	assertCommands(t, commands, want)
	if !data.ExpungedEveryDeletedMessage {
		t.Fatal("a bare EXPUNGE was issued without reporting that it may have widened")
	}
}

// TestMoveEmulationStopsOnCopyFailure verifies the emulation does not flag
// messages \Deleted when the copy that was supposed to preserve them failed.
func TestMoveEmulationStopsOnCopyFailure(t *testing.T) {
	var commands []string
	c, ctx := newExtATestClient(t, "* PREAUTH [CAPABILITY IMAP4REV1] ready", func(s *extAServer) {
		s.selectInbox("INBOX", "101")
		tag, rest := s.command()
		commands = append(commands, rest)
		s.reply(tag + " NO [TRYCREATE] no such mailbox")
		if tag, rest := s.command(); tag != "" {
			commands = append(commands, rest)
			s.ok(tag)
		}
	})
	if _, err := c.Select("INBOX", nil).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	_, err := c.Move(ctx, imap.SeqSetNum(4), "Missing", &MoveOptions{AllowNonAtomicFallback: true})
	var ierr *imap.Error
	if !errors.As(err, &ierr) || ierr.Code != imap.CodeTryCreate {
		t.Fatalf("emulated MOVE error = %v", err)
	}
	assertCommands(t, commands, []string{"COPY 4 Missing"})
}

func TestMoveWithSavedSearchResult(t *testing.T) {
	var sent string
	c, ctx := newExtATestClient(t, "* PREAUTH [CAPABILITY IMAP4REV1 MOVE ESEARCH SEARCHRES] ready", func(s *extAServer) {
		s.selectInbox("INBOX", "101")
		tag, _ := s.command()
		s.reply(`* ESEARCH (TAG "`+tag+`") COUNT 2`, tag+" OK saved")
		tag, rest := s.command()
		sent = rest
		s.ok(tag)
	})
	if _, err := c.Select("INBOX", nil).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	search := c.SearchExtended(imap.SearchDeleted, &ESearchOptions{
		ReturnOptions: []SearchReturnOption{SearchReturnSave, SearchReturnCount},
	})
	if _, err := search.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Move(ctx, nil, "Archive", &MoveOptions{SavedSearchResult: search.SavedResult()}); err != nil {
		t.Fatal(err)
	}
	if sent != "MOVE $ Archive" {
		t.Fatalf("saved-result MOVE wire form = %q", sent)
	}
}

func TestMoveRejectsSetAndSavedResultTogether(t *testing.T) {
	c, ctx := newExtATestClient(t, "* PREAUTH [CAPABILITY IMAP4REV1 MOVE ESEARCH SEARCHRES] ready", func(s *extAServer) {
		s.selectInbox("INBOX", "101")
		tag, _ := s.command()
		s.reply(`* ESEARCH (TAG "`+tag+`") COUNT 2`, tag+" OK saved")
	})
	if _, err := c.Select("INBOX", nil).Wait(ctx); err != nil {
		t.Fatal(err)
	}
	search := c.SearchExtended(imap.SearchDeleted, &ESearchOptions{
		ReturnOptions: []SearchReturnOption{SearchReturnSave, SearchReturnCount},
	})
	if _, err := search.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	_, err := c.Move(ctx, imap.SeqSetNum(1), "Archive", &MoveOptions{SavedSearchResult: search.SavedResult()})
	if err == nil {
		t.Fatal("a message set and a saved search result were accepted together")
	}
	if _, err := c.Move(ctx, nil, "Archive", nil); err == nil {
		t.Fatal("an empty set with no saved result was accepted")
	}
	//lint:ignore SA1012 a nil context is precisely the input under test here
	if _, err := c.Move(nil, imap.SeqSetNum(1), "Archive", nil); err == nil {
		t.Fatal("a nil context was accepted")
	}
	if _, err := c.Move(ctx, imap.SeqSetNum(1), "", nil); err == nil {
		t.Fatal("an empty destination was accepted")
	}
}

func assertCommands(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("commands = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("command %d = %q, want %q (all: %q)", i, got[i], want[i], got)
		}
	}
}
