package imapserver_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapclient"
)

func TestUIDBatchesLoopback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client, _ := newGroupAClient(t, ctx)
	data, err := client.UIDBatches(ctx, 500, nil)
	if err != nil || len(data.Batches) != 0 {
		t.Fatalf("empty: %#v, %v", data, err)
	}
	seedMessages(t, ctx, client, 1001) // Reselection resets the empty request.
	data, err = client.UIDBatches(ctx, 500, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []imap.UIDRange{{Start: 1001, Stop: 502}, {Start: 501, Stop: 2}, {Start: 1, Stop: 1}}
	if len(data.Batches) != len(want) {
		t.Fatalf("batches: %#v", data)
	}
	for i, b := range data.Batches {
		if b.Range != want[i] {
			t.Fatalf("batch %d: %v, want %v", i, b.Range, want[i])
		}
	}
	_, err = client.UIDBatches(ctx, 500, nil)
	var protocol *imap.Error
	if !errors.As(err, &protocol) || protocol.Code != imap.CodeLimit {
		t.Fatalf("repeat: %v", err)
	}
	page, err := client.UIDBatches(ctx, 500, &imapclient.UIDBatchesOptions{Indices: imap.UIDBatchIndexRange{First: 2, Last: 2}})
	if err != nil || len(page.Batches) != 1 || page.Batches[0].Range != want[1] {
		t.Fatalf("page: %#v, %v", page, err)
	}
}

func TestUIDBatchesWireStateAndErrors(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, conn, reader := newGroupARawSessionIn(t, ctx, true)
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	request := func(command, status string) []string {
		t.Helper()
		writeRawCommand(t, conn, "U1 "+command+"\r\n")
		lines, tagged := collectUntilTag(t, reader, "U1 ")
		if !strings.HasPrefix(tagged, "U1 "+status) {
			t.Fatalf("%s: %q", command, tagged)
		}
		return lines
	}
	request("UIDBATCHES 500", "BAD")
	request("ENABLE UIDONLY", "OK")
	request("SELECT INBOX", "OK")
	for _, tt := range []struct{ command, status string }{
		{"UIDBATCHES 499", "NO [TOOFEW]"},
		{"UIDBATCHES 500 2:1", "BAD [CLIENTBUG]"},
		{"UIDBATCHES 500 1:201", "NO [TOOMANY]"},
		{"UIDBATCHES 4294967295 1:4294967295", "NO [TOOMANY]"},
		{"UIDBATCHES 0", "BAD"},
		{"UIDBATCHES 500 0:1", "BAD"},
		{"UIDBATCHES 500 1:0", "BAD"},
		{"UIDBATCHES 500 *:1", "BAD"},
		{"UID UIDBATCHES 500", "BAD"},
	} {
		lines := request(tt.command, tt.status)
		for _, line := range lines {
			if strings.HasPrefix(line, "* UIDBATCHES") {
				t.Fatalf("failed command emitted data: %v", lines)
			}
		}
	}
	request("UID SEARCH RETURN (SAVE) UID 2:3", "OK")
	lines := request("UIDBATCHES 500", "OK")
	if got := findResponse(t, lines, "* UIDBATCHES"); got != "* UIDBATCHES (TAG U1) 3:1" {
		t.Fatalf("UIDONLY: %q", got)
	}
	request("UIDBATCHES 500", "NO [LIMIT]")
	lines = request("UID FETCH $ (UID)", "OK")
	fetched := 0
	for _, line := range lines {
		if strings.HasPrefix(line, "* 2 UIDFETCH ") || strings.HasPrefix(line, "* 3 UIDFETCH ") {
			fetched++
		}
	}
	if fetched != 2 {
		t.Fatalf("SEARCHRES changed: %v", lines)
	}
	lines = request("UIDBATCHES 500 4294967295:4294967295", "OK")
	if got := findResponse(t, lines, "* UIDBATCHES"); got != "* UIDBATCHES (TAG U1)" {
		t.Fatalf("beyond end: %q", got)
	}
	request("SELECT INBOX", "OK")
	request("UIDBATCHES 500 4294967295:4294967295", "OK")
}
