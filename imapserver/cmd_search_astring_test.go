package imapserver_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestLoopbackSearchAcceptsUnquotedStrings(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, connection, reader := newGroupARawSession(t, ctx)
	if err := connection.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for i, criteria := range []string{
		"SUBJECT needle", "FROM alice", "TO alice", "CC alice", "BCC alice",
		"BODY needle", "TEXT needle", "HEADER Subject needle",
		"OR SUBJECT needle NOT FROM alice", "(SUBJECT needle TEXT needle)",
	} {
		tag := fmt.Sprintf("S%d", i)
		writeRawCommand(t, connection, tag+" SEARCH "+criteria+"\r\n")
		_, tagged := collectUntilTag(t, reader, tag+" ")
		if !strings.HasPrefix(tagged, tag+" OK ") {
			t.Errorf("SEARCH %s: %q", criteria, tagged)
		}
	}
}
