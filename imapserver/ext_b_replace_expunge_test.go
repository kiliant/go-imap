package imapserver_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestReplaceReportsRemoval(t *testing.T) {
	for _, qresync := range []bool{false, true} {
		for _, destination := range []string{"INBOX", "Archive"} {
			t.Run(fmt.Sprintf("qresync=%t/to=%s", qresync, destination), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_, conn, reader := newGroupARawSessionIn(t, ctx, true)
				if qresync {
					writeRawCommand(t, conn, "e ENABLE QRESYNC\r\n")
					collectUntilTag(t, reader, "e ")
				}
				writeRawCommand(t, conn, "s SELECT INBOX\r\n")
				collectUntilTag(t, reader, "s ")
				// Diverge sequence numbers from UIDs: sequence 1 will be UID 2.
				writeRawCommand(t, conn, "d STORE 1 +FLAGS.SILENT (\\Deleted)\r\n")
				collectUntilTag(t, reader, "d ")
				writeRawCommand(t, conn, "x UID EXPUNGE 1\r\n")
				collectUntilTag(t, reader, "x ")
				if destination == "Archive" {
					writeRawCommand(t, conn, "c CREATE Archive\r\n")
					collectUntilTag(t, reader, "c ")
				}
				command := "REPLACE 1"
				body := "Subject: replacement\r\n\r\nnew\r\n"
				writeRawCommand(t, conn, fmt.Sprintf("r %s %s {%d+}\r\n%s\r\n", command, destination, len(body), body))
				lines, tagged := collectUntilTag(t, reader, "r ")
				if !strings.HasPrefix(tagged, "r OK") {
					t.Fatalf("REPLACE = %q", tagged)
				}
				want := "* 1 EXPUNGE"
				if qresync {
					want = "* VANISHED 2"
				}
				count := 0
				for _, line := range lines {
					if line == want {
						count++
					}
				}
				if count != 1 {
					t.Errorf("removal count = %d, want one %q before completion; responses: %v", count, want, lines)
				}
				writeRawCommand(t, conn, "n NOOP\r\n")
				lines, _ = collectUntilTag(t, reader, "n ")
				for _, line := range lines {
					if strings.Contains(line, "EXPUNGE") || strings.Contains(line, "VANISHED") {
						t.Errorf("removal repeated after completion: %q", line)
					}
				}
			})
		}
	}
}
