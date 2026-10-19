package maildir

import (
	"context"
	"encoding/json"
	"github.com/kiliant/go-imap/imapserver"
	"testing"
)

func TestRecentClaimSurvivesRestartAndExamineDoesNotConsume(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	b := openTest(t, root)
	s := loginTest(t, b)
	appendTest(t, s, "INBOX")
	if err := b.Close(ctx, nil); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		readOnly bool
		want     uint32
	}{{true, 1}, {false, 1}, {false, 0}} {
		b = openTest(t, root)
		s = loginTest(t, b)
		result, err := s.Select(ctx, "INBOX", &imapserver.Updater{PushFunc: func(*imapserver.UpdateBatch) error { return nil }}, &imapserver.SelectOptions{ReadOnly: tc.readOnly})
		if err != nil {
			t.Fatal(err)
		}
		if got := result.Snapshot.Status.NumRecent; got != tc.want {
			t.Fatalf("reopened SELECT(readOnly=%v) recent=%d, want %d", tc.readOnly, got, tc.want)
		}
		if err := result.Mailbox.Unselect(ctx, nil); err != nil {
			t.Fatal(err)
		}
		if err := b.Close(ctx, nil); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOldMessageStateWithoutRecentIsConservativelyRecent(t *testing.T) {
	var msg messageRecord
	if err := json.Unmarshal([]byte(`{"uid":1,"file":"old:2,","size":0,"modseq":1}`), &msg); err != nil {
		t.Fatal(err)
	}
	if !msg.Recent {
		t.Fatal("unknown RECENT state should be treated as recent")
	}
	raw, err := json.Marshal(&msg)
	if err != nil {
		t.Fatal(err)
	}
	var roundtrip messageRecord
	if err := json.Unmarshal(raw, &roundtrip); err != nil || !roundtrip.Recent {
		t.Fatalf("pending recent lost: %v", err)
	}
	msg.Recent = false
	raw, err = json.Marshal(&msg)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &roundtrip); err != nil || roundtrip.Recent {
		t.Fatalf("claimed state lost: %v", err)
	}
}
