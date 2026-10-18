package imapserver_test

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestLoopbackCondStoreModifiedUsesCommandNumberSpace(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, connection, reader := newGroupARawSession(t, ctx)
	for _, command := range []string{"D STORE 1 +FLAGS (\\Deleted)", "X EXPUNGE"} {
		writeRawCommand(t, connection, command+"\r\n")
		tag, _, _ := strings.Cut(command, " ")
		if _, tagged := collectUntilTag(t, reader, tag+" "); !strings.HasPrefix(tagged, tag+" OK") {
			t.Fatal(tagged)
		}
	}
	for _, tc := range []struct{ command, want string }{
		{"S STORE 1:2 (UNCHANGEDSINCE 0) +FLAGS (\\Flagged)", "[MODIFIED 1:2]"},
		{"U UID STORE 2:3 (UNCHANGEDSINCE 0) +FLAGS (\\Flagged)", "[MODIFIED 2:3]"},
		{"T STORE 2 (UNCHANGEDSINCE 0) +FLAGS.SILENT (\\Flagged)", "[MODIFIED 2]"},
	} {
		writeRawCommand(t, connection, tc.command+"\r\n")
		tag, _, _ := strings.Cut(tc.command, " ")
		_, tagged := collectUntilTag(t, reader, tag+" ")
		if !strings.HasPrefix(tagged, tag+" OK") || !strings.Contains(tagged, tc.want) {
			t.Errorf("%s: %q, want OK %s", tc.command, tagged, tc.want)
		}
	}
}
