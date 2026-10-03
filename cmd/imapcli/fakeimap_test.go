package main

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// startFakeIMAP serves a minimal cleartext IMAP4rev1 server for CLI e2e tests.
// It supports AUTHENTICATE PLAIN (SASL-IR), CAPABILITY, LIST, EXAMINE, FETCH,
// UID FETCH, and LOGOUT.
func startFakeIMAP(t *testing.T) (addr string, closeFn func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	done := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				select {
				case <-done:
					return
				default:
					return
				}
			}
			wg.Add(1)
			go func(c net.Conn) {
				defer wg.Done()
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(10 * time.Second))
				serveFakeIMAP(c)
			}(conn)
		}
	}()
	return ln.Addr().String(), func() {
		close(done)
		_ = ln.Close()
		wg.Wait()
	}
}

func serveFakeIMAP(conn net.Conn) {
	w := bufio.NewWriter(conn)
	r := bufio.NewReader(conn)
	writeLine(w, "* OK [CAPABILITY IMAP4rev1 AUTH=PLAIN SASL-IR] fake ready")
	selected := false
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		tag, cmd := fields[0], strings.ToUpper(fields[1])
		switch cmd {
		case "CAPABILITY":
			writeLine(w, "* CAPABILITY IMAP4rev1 AUTH=PLAIN SASL-IR")
			writeLine(w, tag+" OK CAPABILITY completed")
		case "AUTHENTICATE":
			if len(fields) < 4 || !strings.EqualFold(fields[2], "PLAIN") {
				writeLine(w, tag+` BAD expected AUTHENTICATE PLAIN <ir>`)
				continue
			}
			raw, err := base64.StdEncoding.DecodeString(fields[3])
			if err != nil || !validPLAIN(raw, "user", "secret") {
				writeLine(w, tag+" NO authentication failed")
				continue
			}
			writeLine(w, tag+" OK [CAPABILITY IMAP4rev1 AUTH=PLAIN] AUTHENTICATE completed")
		case "LIST":
			writeLine(w, `* LIST (\HasNoChildren) "/" INBOX`)
			writeLine(w, `* LIST (\HasChildren) "/" Archive`)
			writeLine(w, `* LIST (\HasNoChildren \Archive) "/" Archive/2024`)
			writeLine(w, tag+" OK LIST completed")
		case "EXAMINE":
			writeLine(w, `* FLAGS (\Seen \Answered)`)
			writeLine(w, `* OK [PERMANENTFLAGS ()] Read-only`)
			writeLine(w, `* 2 EXISTS`)
			writeLine(w, `* 0 RECENT`)
			writeLine(w, `* OK [UIDVALIDITY 1] UIDs valid`)
			writeLine(w, `* OK [UIDNEXT 12] Predicted next UID`)
			writeLine(w, `* OK [UNSEEN 1] First unseen`)
			writeLine(w, tag+" OK [READ-ONLY] EXAMINE completed")
			selected = true
		case "FETCH":
			if !selected {
				writeLine(w, tag+" NO not selected")
				continue
			}
			// Summaries for seq 1:2 (newest window of two).
			writeLine(w, `* 1 FETCH (UID 10 FLAGS (\Seen) ENVELOPE ("01-Aug-2026 10:00:00 +0000" "Older" (("Alice" NIL "alice" "example.com")) (("Alice" NIL "alice" "example.com")) (("Alice" NIL "alice" "example.com")) (("Bob" NIL "bob" "example.com")) NIL NIL NIL "<a@example.com>"))`)
			writeLine(w, `* 2 FETCH (UID 11 FLAGS () ENVELOPE ("01-Aug-2026 12:00:00 +0000" "Newer" (("Carol" NIL "carol" "example.com")) (("Carol" NIL "carol" "example.com")) (("Carol" NIL "carol" "example.com")) (("Bob" NIL "bob" "example.com")) NIL NIL NIL "<b@example.com>"))`)
			writeLine(w, tag+" OK FETCH completed")
		case "UID":
			if !selected || len(fields) < 3 || !strings.EqualFold(fields[2], "FETCH") {
				writeLine(w, tag+" BAD expected UID FETCH")
				continue
			}
			body := "From: carol@example.com\r\nSubject: Newer\r\n\r\nhello from fake server\r\n"
			writeLine(w, fmt.Sprintf(`* 2 FETCH (UID 11 FLAGS () ENVELOPE ("01-Aug-2026 12:00:00 +0000" "Newer" (("Carol" NIL "carol" "example.com")) (("Carol" NIL "carol" "example.com")) (("Carol" NIL "carol" "example.com")) (("Bob" NIL "bob" "example.com")) NIL NIL NIL "<b@example.com>") BODY[] {%d}`, len(body)))
			if _, err := io.WriteString(w, body); err != nil {
				return
			}
			writeLine(w, ")")
			writeLine(w, tag+" OK UID FETCH completed")
		case "LOGOUT":
			writeLine(w, "* BYE fake closing")
			writeLine(w, tag+" OK LOGOUT completed")
			return
		default:
			writeLine(w, tag+" BAD unknown command "+cmd)
		}
	}
}

func writeLine(w *bufio.Writer, s string) {
	_, _ = io.WriteString(w, s+"\r\n")
	_ = w.Flush()
}

func validPLAIN(raw []byte, wantUser, wantPass string) bool {
	// PLAIN: [authzid] NUL authcid NUL passwd
	parts := strings.Split(string(raw), "\x00")
	if len(parts) != 3 {
		return false
	}
	return parts[1] == wantUser && parts[2] == wantPass
}
