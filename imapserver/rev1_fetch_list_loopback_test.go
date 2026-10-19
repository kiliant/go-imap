package imapserver_test

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestLoopbackRev1StandaloneFetchAndHierarchy(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, client, reader := newGroupARawSession(t, ctx)
	for _, reference := range []string{"", "INBOX", "missing/"} {
		writeRawCommand(t, client, "L LIST \""+reference+"\" \"\"\r\n")
		lines, tagged := collectUntilTag(t, reader, "L ")
		if !strings.HasPrefix(tagged, "L OK") || len(lines) != 1 || !strings.Contains(lines[0], `"/" ""`) {
			t.Fatalf("hierarchy response: %v %q", lines, tagged)
		}
	}
	for _, item := range []string{"UID", "FLAGS", "RFC822.SIZE", "BODY.PEEK[HEADER.FIELDS (SUBJECT)]"} {
		writeRawCommand(t, client, "F FETCH 1 "+item+"\r\n")
		lines, tagged := collectUntilTag(t, reader, "F ")
		if !strings.HasPrefix(tagged, "F OK") || !strings.Contains(strings.Join(lines, " "), "* 1 FETCH") {
			t.Fatalf("standalone %s: %v %q", item, lines, tagged)
		}
	}
}
