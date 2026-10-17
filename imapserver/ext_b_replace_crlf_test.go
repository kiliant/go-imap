package imapserver_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestReplaceConsumesCommandTerminator(t *testing.T) {
	for _, uidMode := range []bool{false, true} {
		for _, syncLiteral := range []bool{false, true} {
			for _, refused := range []bool{false, true} {
				t.Run(fmt.Sprintf("uid=%t/sync=%t/refused=%t", uidMode, syncLiteral, refused), func(t *testing.T) {
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					_, conn, reader := newGroupARawSession(t, ctx)
					_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
					command := "REPLACE"
					if uidMode {
						command = "UID REPLACE"
					}
					set, want := "1", "r OK"
					if refused {
						set, want = "1:2", "r BAD"
					}
					body, plus := "Subject: replaced\r\n\r\nbody\r\n", "+"
					if syncLiteral {
						plus = ""
					}
					writeRawCommand(t, conn, fmt.Sprintf("r %s %s INBOX {%d%s}\r\n", command, set, len(body), plus))
					if syncLiteral {
						if line, err := reader.ReadString('\n'); err != nil || !strings.HasPrefix(line, "+") {
							t.Fatalf("continuation: %q, %v", line, err)
						}
					}
					writeRawCommand(t, conn, body+"\r\n")
					lines, tagged := collectUntilTag(t, reader, "r ")
					if !strings.HasPrefix(tagged, want) {
						t.Fatalf("REPLACE: %q; responses: %v", tagged, lines)
					}
					writeRawCommand(t, conn, "n NOOP\r\n")
					lines, tagged = collectUntilTag(t, reader, "n ")
					if !strings.HasPrefix(tagged, "n OK") {
						t.Fatalf("NOOP: %q", tagged)
					}
					for _, line := range lines {
						if strings.Contains(line, " BAD") {
							t.Errorf("stray BAD after REPLACE: %q", line)
						}
					}
				})
			}
		}
	}
}
