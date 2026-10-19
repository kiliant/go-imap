package imapserver_test

import (
	"context"
	"fmt"
	"github.com/kiliant/go-imap/imapserver"
	"github.com/kiliant/go-imap/imapserver/maildir"
	"github.com/kiliant/go-imap/imapserver/memory"
	"slices"
	"strings"
	"testing"
	"time"
)

func conformanceBackends(t *testing.T, test func(*testing.T, imapserver.Backend)) {
	t.Helper()
	t.Run("memory", func(t *testing.T) { test(t, memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}})) })
	t.Run("maildir", func(t *testing.T) {
		backend, err := maildir.Open(context.Background(), t.TempDir(), &maildir.Options{Accounts: []maildir.AccountOptions{{Username: "alice", Password: "secret"}}})
		if err != nil {
			t.Fatal(err)
		}
		defer backend.Close(context.Background(), nil)
		test(t, backend)
	})
}

func TestConformanceRecentOwnershipAndReadOnlySelection(t *testing.T) {
	conformanceBackends(t, func(t *testing.T, backend imapserver.Backend) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		server := imapserver.New(backend, &imapserver.Options{AllowInsecureAuth: true})
		_, connection, reader := newRawSessionOn(t, ctx, server)
		tagNumber := 0
		command := func(raw string) []string {
			t.Helper()
			tagNumber++
			tag := fmt.Sprintf("R%d", tagNumber)
			writeRawCommand(t, connection, tag+" "+raw+"\r\n")
			lines, completion := collectUntilTag(t, reader, tag+" ")
			if !strings.HasPrefix(completion, tag+" OK") {
				t.Fatalf("%s: %s; %v", raw, completion, lines)
			}
			return lines
		}
		raw := "Subject: recent\r\n\r\nbody\r\n"
		for range 3 {
			command(fmt.Sprintf("APPEND INBOX {%d+}\r\n%s", len(raw), raw))
		}
		if lines := command("EXAMINE INBOX"); !slices.Contains(lines, "* 3 RECENT") {
			t.Fatalf("EXAMINE recent: %v", lines)
		}
		if lines := command("STATUS INBOX (RECENT)"); !strings.Contains(strings.Join(lines, "\n"), "RECENT 3") {
			t.Fatalf("EXAMINE consumed recent: %v", lines)
		}
		if lines := command("SELECT INBOX"); !slices.Contains(lines, "* 3 RECENT") {
			t.Fatalf("SELECT recent: %v", lines)
		}
		_, observer, observerReader := newRawSessionOn(t, ctx, server)
		writeRawCommand(t, observer, "O1 SELECT INBOX\r\n")
		lines, _ := collectUntilTag(t, observerReader, "O1 ")
		if !slices.Contains(lines, "* 0 RECENT") {
			t.Fatalf("second writer reclaimed recent: %v", lines)
		}
		writeRawCommand(t, observer, "O2 FETCH 1 FLAGS\r\n")
		lines, _ = collectUntilTag(t, observerReader, "O2 ")
		if strings.Contains(strings.ToUpper(strings.Join(lines, "\n")), "\\RECENT") {
			t.Fatalf("recent leaked to observer: %v", lines)
		}
		writeRawCommand(t, observer, "O3 UNSELECT\r\n")
		collectUntilTag(t, observerReader, "O3 ")
		if lines := command("FETCH 1 FLAGS"); !strings.Contains(strings.ToUpper(strings.Join(lines, "\n")), "\\RECENT") {
			t.Fatalf("owner has no recent flag: %v", lines)
		}
		if lines := command("SEARCH RECENT"); !slices.Contains(lines, "* SEARCH 1 2 3") {
			t.Fatalf("SEARCH RECENT: %v", lines)
		}
		command("STORE 1 FLAGS (\\Seen)")
		if lines := command("SEARCH NEW"); !slices.Contains(lines, "* SEARCH 2 3") {
			t.Fatalf("SEARCH NEW: %v", lines)
		}
		lines = command("COPY 1 INBOX")
		if !slices.Contains(lines, "* 4 EXISTS") || !slices.Contains(lines, "* 4 RECENT") {
			t.Fatalf("COPY recent updates: %v", lines)
		}
		lines = command("MOVE 1 INBOX")
		expunge, exists := slices.Index(lines, "* 1 EXPUNGE"), slices.Index(lines, "* 4 EXISTS")
		if expunge < 0 || exists <= expunge {
			t.Fatalf("same-mailbox MOVE lost copy/count ordering: %v", lines)
		}
		command("UNSELECT")
		if lines := command("SELECT INBOX"); !slices.Contains(lines, "* 0 RECENT") {
			t.Fatalf("recent survived reselection: %v", lines)
		}
		command("UNSELECT")
		command("ENABLE IMAP4rev2")
		lines = command("SELECT INBOX")
		lines = append(lines, command("FETCH 1 FLAGS")...)
		if strings.Contains(strings.ToUpper(strings.Join(lines, "\n")), "RECENT") {
			t.Fatalf("rev2 exposed RECENT: %v", lines)
		}
	})
}

func TestConformanceSearchContextChangesAndExpungeNumbers(t *testing.T) {
	conformanceBackends(t, func(t *testing.T, backend imapserver.Backend) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		server := imapserver.New(backend, &imapserver.Options{AllowInsecureAuth: true})
		_, connection, reader := newRawSessionOn(t, ctx, server)
		i := 0
		command := func(raw string) []string {
			t.Helper()
			i++
			tag := fmt.Sprintf("C%d", i)
			writeRawCommand(t, connection, tag+" "+raw+"\r\n")
			lines, done := collectUntilTag(t, reader, tag+" ")
			if !strings.HasPrefix(done, tag+" OK") {
				t.Fatalf("%s: %s %v", raw, done, lines)
			}
			return lines
		}
		raw := "Subject: context\r\n\r\nbody\r\n"
		for range 5 {
			command(fmt.Sprintf("APPEND INBOX {%d+}\r\n%s", len(raw), raw))
		}
		command("SELECT INBOX")
		command("SEARCH RETURN (UPDATE) BODY body SEEN")
		lines := command("STORE 1,2,4 +FLAGS (\\Seen)")
		if !strings.Contains(strings.Join(lines, "\n"), "ADDTO (0 1:2,4)") {
			t.Fatalf("missing ADDTO: %v", lines)
		}
		lines = command("STORE 1,2 -FLAGS (\\Seen)")
		if !strings.Contains(strings.Join(lines, "\n"), "REMOVEFROM (0 1:2)") {
			t.Fatalf("missing flag REMOVEFROM: %v", lines)
		}
		command("STORE 3:5 +FLAGS (\\Deleted)")
		lines = command("EXPUNGE")
		removal, expunge := -1, -1
		for j, line := range lines {
			if strings.Contains(line, "REMOVEFROM (0 4)") {
				removal = j
			}
			if strings.Contains(line, " EXPUNGE") && expunge < 0 {
				expunge = j
			}
		}
		if removal < 0 || expunge <= removal {
			t.Fatalf("context removal must use pre-expunge sequence 4: %v", lines)
		}
		lines = command(fmt.Sprintf("APPEND INBOX (\\Seen) {%d+}\r\n%s", len(raw), raw))
		if !strings.Contains(strings.Join(lines, "\n"), "ADDTO (0 3)") {
			t.Fatalf("append ADDTO: %v", lines)
		}
		command("CANCELUPDATE C7")
	})
}
