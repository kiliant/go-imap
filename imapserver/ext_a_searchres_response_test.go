package imapserver_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestLoopbackSearchResSaveOnlySuppressesResponse(t *testing.T) {
	for _, rev2 := range []bool{false, true} {
		for _, uid := range []bool{false, true} {
			t.Run(fmt.Sprintf("rev2=%t/uid=%t", rev2, uid), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				_, connection, reader := newGroupARawSessionIn(t, ctx, true)
				if rev2 {
					writeRawCommand(t, connection, "E ENABLE IMAP4rev2\r\n")
					collectUntilTag(t, reader, "E ")
				}
				writeRawCommand(t, connection, "S SELECT INBOX\r\n")
				collectUntilTag(t, reader, "S ")
				prefix := ""
				if uid {
					prefix = "UID "
				}
				for i, criterion := range []string{"2:3", "SUBJECT \"nomatch\"", "MODSEQ 1"} {
					tag := fmt.Sprintf("Q%d", i)
					writeRawCommand(t, connection, tag+" "+prefix+"SEARCH RETURN (SAVE) "+criterion+"\r\n")
					lines, tagged := collectUntilTag(t, reader, tag+" ")
					if !strings.HasPrefix(tagged, tag+" OK") {
						t.Fatal(tagged)
					}
					for _, line := range lines {
						if strings.HasPrefix(line, "* ESEARCH") || strings.HasPrefix(line, "* SEARCH") {
							t.Errorf("SAVE-only returned search data: %q", line)
						}
					}
					writeRawCommand(t, connection, "F UID FETCH $ (UID)\r\n")
					lines, tagged = collectUntilTag(t, reader, "F ")
					if !strings.HasPrefix(tagged, "F OK") {
						t.Fatal(tagged)
					}
					var count int
					for _, line := range lines {
						if strings.Contains(line, " FETCH ") {
							count++
						}
					}
					if want := []int{2, 0, 3}[i]; count != want {
						t.Errorf("saved result returned %d messages, want %d", count, want)
					}
				}
			})
		}
	}
}
