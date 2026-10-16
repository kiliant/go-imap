package imapserver_test

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestLoopbackCondStoreSearchResponses(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, connection, reader := newGroupARawSession(t, ctx)
	writeRawCommand(t, connection, "C0 SELECT INBOX (CONDSTORE)\r\n")
	collectUntilTag(t, reader, "C0 ")
	writeRawCommand(t, connection, "C1 FETCH 1:3 (UID MODSEQ)\r\n")
	lines, _ := collectUntilTag(t, reader, "C1 ")
	first := parseModSeq(t, findResponse(t, lines, "* 1 FETCH"))
	last := parseModSeq(t, findResponse(t, lines, "* 3 FETCH"))
	// The newest message clock is deliberately neither endpoint.
	writeRawCommand(t, connection, "C2 STORE 2 +FLAGS (\\Flagged)\r\n")
	lines, _ = collectUntilTag(t, reader, "C2 ")
	highest := parseModSeq(t, findResponse(t, lines, "* 2 FETCH"))
	for i, tt := range []struct {
		command, response string
	}{
		{"SEARCH MODSEQ 1", fmt.Sprintf("* SEARCH 1 2 3 (MODSEQ %d)", highest)},
		{"UID SEARCH MODSEQ 1", fmt.Sprintf("* SEARCH 1 2 3 (MODSEQ %d)", highest)},
		{"SEARCH NOT NOT MODSEQ 1", fmt.Sprintf("* SEARCH 1 2 3 (MODSEQ %d)", highest)},
		{"SEARCH MODSEQ " + strconv.FormatUint(highest, 10), fmt.Sprintf("* SEARCH 2 (MODSEQ %d)", highest)},
		{"SEARCH MODSEQ " + strconv.FormatUint(highest+1, 10), "* SEARCH"},
		{"SEARCH ALL", "* SEARCH 1 2 3"},
		{"SEARCH RETURN () MODSEQ 1", fmt.Sprintf(" ALL 1:3 MODSEQ %d", highest)},
		{"UID SEARCH RETURN (ALL) MODSEQ 1", fmt.Sprintf(" UID ALL 1:3 MODSEQ %d", highest)},
		{"SEARCH RETURN (MIN) MODSEQ 1", fmt.Sprintf(" MIN 1 MODSEQ %d", first)},
		{"SEARCH RETURN (MAX) MODSEQ 1", fmt.Sprintf(" MAX 3 MODSEQ %d", last)},
		{"SEARCH RETURN (MIN MAX) MODSEQ 1", fmt.Sprintf(" MIN 1 MAX 3 MODSEQ %d", max(first, last))},
		{"SEARCH RETURN (MIN COUNT) MODSEQ 1", fmt.Sprintf(" MIN 1 COUNT 3 MODSEQ %d", highest)},
		{"SEARCH RETURN (COUNT) MODSEQ " + strconv.FormatUint(highest+1, 10), " COUNT 0"},
	} {
		t.Run(tt.command, func(t *testing.T) {
			tag := fmt.Sprintf("S%d", i)
			writeRawCommand(t, connection, tag+" "+tt.command+"\r\n")
			lines, tagged := collectUntilTag(t, reader, tag+" ")
			if !strings.HasPrefix(tagged, tag+" OK") {
				t.Fatalf("command failed: %s", tagged)
			}
			var want string
			if strings.HasPrefix(tt.response, "* SEARCH") {
				want = tt.response
			} else {
				want = "* ESEARCH (TAG \"" + tag + "\")" + tt.response
			}
			if len(lines) != 1 || lines[0] != want {
				t.Fatalf("response = %q, want %q", lines, want)
			}
		})
	}
}

func TestLoopbackSearchModSeqActivatesCondStore(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, connection, reader := newGroupARawSession(t, ctx)
	writeRawCommand(t, connection, "C1 FETCH 1 (FLAGS)\r\n")
	lines, _ := collectUntilTag(t, reader, "C1 ")
	if strings.Contains(findResponse(t, lines, "* 1 FETCH"), "MODSEQ") {
		t.Fatal("fixture already enabled CONDSTORE")
	}
	writeRawCommand(t, connection, "C2 SEARCH OR MODSEQ 1 ALL\r\n")
	_, tagged := collectUntilTag(t, reader, "C2 ")
	if !strings.HasPrefix(tagged, "C2 OK") {
		t.Fatal(tagged)
	}
	writeRawCommand(t, connection, "C3 FETCH 1 (FLAGS)\r\n")
	lines, _ = collectUntilTag(t, reader, "C3 ")
	if !strings.Contains(findResponse(t, lines, "* 1 FETCH"), "MODSEQ") {
		t.Fatal("MODSEQ SEARCH failed to enable CONDSTORE")
	}
}

func TestLoopbackRev2SearchModSeq(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, connection, reader := newGroupARawSessionIn(t, ctx, true)
	for _, command := range []string{"C1 ENABLE IMAP4rev2", "C2 SELECT INBOX", "C3 SEARCH MODSEQ 1"} {
		writeRawCommand(t, connection, command+"\r\n")
		tag, _, _ := strings.Cut(command, " ")
		lines, tagged := collectUntilTag(t, reader, tag+" ")
		if !strings.HasPrefix(tagged, tag+" OK") {
			t.Fatal(tagged)
		}
		if tag == "C3" {
			line := findResponse(t, lines, "* ESEARCH")
			if !strings.Contains(line, " ALL 1:3 MODSEQ ") {
				t.Fatalf("rev2 omitted ESEARCH MODSEQ: %q", line)
			}
		}
	}
}
