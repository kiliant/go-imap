package main

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAccount is a stateful, cleartext IMAP4rev1 account for backup tests.
// Tests change it between runs; each connection reads it under mu.
//
// It serves AUTHENTICATE PLAIN (SASL-IR), CAPABILITY, LIST, EXAMINE, UID FETCH
// and LOGOUT. Every other command is answered BAD and recorded in
// forbidden, which is how the tests assert that a backup is read-only.
type fakeAccount struct {
	mu        sync.Mutex
	mailboxes []*fakeMailbox
	// uidAfterBody makes body responses carry UID, INTERNALDATE and
	// RFC822.SIZE after the BODY[] literal rather than before it.
	uidAfterBody bool
	// failExamine names mailboxes whose EXAMINE is refused.
	failExamine map[string]bool
	// extraCaps are advertised beside AUTH=PLAIN, e.g. "AUTH=OAUTHBEARER".
	extraCaps string
	forbidden []string
}

type fakeMailbox struct {
	name        string
	noSelect    bool
	uidValidity uint32
	uidNext     uint32
	messages    []*fakeMessage // in sequence-number order
}

type fakeMessage struct {
	uid   uint32
	flags []string
	date  time.Time
	body  string
}

func (a *fakeAccount) mailbox(name string) *fakeMailbox {
	for _, m := range a.mailboxes {
		if m.name == name {
			return m
		}
	}
	return nil
}

// addMailbox creates a mailbox, or returns the existing one.
func (a *fakeAccount) addMailbox(name string, uidValidity uint32) *fakeMailbox {
	a.mu.Lock()
	defer a.mu.Unlock()
	if m := a.mailbox(name); m != nil {
		return m
	}
	m := &fakeMailbox{name: name, uidValidity: uidValidity, uidNext: 1}
	a.mailboxes = append(a.mailboxes, m)
	return m
}

func (a *fakeAccount) removeMailbox(name string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, m := range a.mailboxes {
		if m.name == name {
			a.mailboxes = append(a.mailboxes[:i], a.mailboxes[i+1:]...)
			return
		}
	}
}

// deliver appends a message and returns its UID.
func (a *fakeAccount) deliver(mailbox string, date time.Time, body string, flags ...string) uint32 {
	a.mu.Lock()
	defer a.mu.Unlock()
	m := a.mailbox(mailbox)
	uid := m.uidNext
	m.uidNext++
	m.messages = append(m.messages, &fakeMessage{uid: uid, flags: flags, date: date, body: body})
	return uid
}

func (a *fakeAccount) setFlags(mailbox string, uid uint32, flags ...string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, msg := range a.mailbox(mailbox).messages {
		if msg.uid == uid {
			msg.flags = flags
		}
	}
}

func (a *fakeAccount) expunge(mailbox string, uid uint32) {
	a.mu.Lock()
	defer a.mu.Unlock()
	m := a.mailbox(mailbox)
	for i, msg := range m.messages {
		if msg.uid == uid {
			m.messages = append(m.messages[:i], m.messages[i+1:]...)
			return
		}
	}
}

// renumber gives a mailbox a new UIDVALIDITY and fresh UIDs, as a server does
// after its index is rebuilt.
func (a *fakeAccount) renumber(mailbox string, uidValidity uint32) {
	a.mu.Lock()
	defer a.mu.Unlock()
	m := a.mailbox(mailbox)
	m.uidValidity = uidValidity
	m.uidNext = 1
	for _, msg := range m.messages {
		msg.uid = m.uidNext
		m.uidNext++
	}
}

func (a *fakeAccount) forbiddenCommands() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.forbidden...)
}

// serve starts a listener for the account and returns its address.
func (a *fakeAccount) serve(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func(c net.Conn) {
				defer wg.Done()
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(20 * time.Second))
				a.session(c)
			}(conn)
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		wg.Wait()
	})
	return ln.Addr().String()
}

func (a *fakeAccount) session(conn net.Conn) {
	w := bufio.NewWriter(conn)
	r := bufio.NewReader(conn)
	caps := strings.TrimSpace("IMAP4rev1 AUTH=PLAIN SASL-IR " + a.extraCaps)
	writeLine(w, "* OK [CAPABILITY "+caps+"] fake account ready")
	var selected *fakeMailbox
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		tag, cmd, args := splitCommand(strings.TrimRight(line, "\r\n"))
		switch cmd {
		case "CAPABILITY":
			writeLine(w, "* CAPABILITY "+caps)
			writeLine(w, tag+" OK CAPABILITY completed")
		case "AUTHENTICATE":
			fields := strings.Fields(args)
			if len(fields) != 2 || !strings.EqualFold(fields[0], "PLAIN") {
				// Only PLAIN with an initial response is a password login here;
				// anything else is the wrong mechanism for a password.
				writeLine(w, tag+" NO [AUTHENTICATIONFAILED] wrong mechanism "+fields[0])
				continue
			}
			raw, err := base64.StdEncoding.DecodeString(fields[1])
			if err != nil || !validPLAIN(raw, "user", "secret") {
				writeLine(w, tag+" NO authentication failed")
				continue
			}
			writeLine(w, tag+" OK [CAPABILITY IMAP4rev1 AUTH=PLAIN] AUTHENTICATE completed")
		case "LIST":
			a.mu.Lock()
			for _, m := range a.mailboxes {
				attrs := `\HasNoChildren`
				if m.noSelect {
					attrs = `\Noselect \HasChildren`
				}
				writeLine(w, fmt.Sprintf(`* LIST (%s) "/" %q`, attrs, m.name))
			}
			a.mu.Unlock()
			writeLine(w, tag+" OK LIST completed")
		case "EXAMINE":
			name := unquote(args)
			a.mu.Lock()
			m := a.mailbox(name)
			if m == nil || m.noSelect || a.failExamine[name] {
				a.mu.Unlock()
				selected = nil
				writeLine(w, tag+" NO cannot examine "+name)
				continue
			}
			selected = m
			writeLine(w, `* FLAGS (\Seen \Answered \Flagged \Deleted \Draft)`)
			writeLine(w, `* OK [PERMANENTFLAGS ()] Read-only`)
			writeLine(w, fmt.Sprintf("* %d EXISTS", len(m.messages)))
			writeLine(w, "* 0 RECENT")
			writeLine(w, fmt.Sprintf("* OK [UIDVALIDITY %d] UIDs valid", m.uidValidity))
			writeLine(w, fmt.Sprintf("* OK [UIDNEXT %d] Predicted next UID", m.uidNext))
			a.mu.Unlock()
			writeLine(w, tag+" OK [READ-ONLY] EXAMINE completed")
		case "UID":
			sub, rest, _ := strings.Cut(args, " ")
			if !strings.EqualFold(sub, "FETCH") || selected == nil {
				a.forbid(line)
				writeLine(w, tag+" BAD only UID FETCH in a selected mailbox")
				continue
			}
			setText, items, _ := strings.Cut(rest, " ")
			items = strings.ToUpper(items)
			if strings.Contains(items, "BODY[") || strings.Contains(items, "RFC822 ") || strings.Contains(items, "RFC822)") {
				a.forbid(line) // a non-PEEK body fetch would set \Seen
			}
			if err := a.uidFetch(w, selected, setText, strings.Contains(items, "BODY.PEEK[]")); err != nil {
				return
			}
			writeLine(w, tag+" OK UID FETCH completed")
		case "LOGOUT":
			writeLine(w, "* BYE fake closing")
			writeLine(w, tag+" OK LOGOUT completed")
			return
		default:
			a.forbid(line)
			writeLine(w, tag+" BAD unsupported command "+cmd)
		}
	}
}

func (a *fakeAccount) forbid(line string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.forbidden = append(a.forbidden, strings.TrimRight(line, "\r\n"))
}

func (a *fakeAccount) uidFetch(w *bufio.Writer, m *fakeMailbox, setText string, withBody bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	var maxUID uint32
	if n := len(m.messages); n > 0 {
		maxUID = m.messages[n-1].uid
	}
	inSet := parseFakeUIDSet(setText, maxUID)
	for i, msg := range m.messages {
		if !inSet(msg.uid) {
			continue
		}
		seq := i + 1
		if !withBody {
			writeLine(w, fmt.Sprintf("* %d FETCH (UID %d FLAGS (%s))", seq, msg.uid, strings.Join(msg.flags, " ")))
			continue
		}
		meta := fmt.Sprintf(`UID %d INTERNALDATE "%s" RFC822.SIZE %d`, msg.uid, msg.date.Format("02-Jan-2006 15:04:05 -0700"), len(msg.body))
		var err error
		if a.uidAfterBody {
			_, err = fmt.Fprintf(w, "* %d FETCH (BODY[] {%d}\r\n%s %s)\r\n", seq, len(msg.body), msg.body, meta)
		} else {
			_, err = fmt.Fprintf(w, "* %d FETCH (%s BODY[] {%d}\r\n%s)\r\n", seq, meta, len(msg.body), msg.body)
		}
		if err != nil {
			return err
		}
		if err := w.Flush(); err != nil {
			return err
		}
	}
	return nil
}

// parseFakeUIDSet returns a membership test for a UID set such as "1:*" or
// "2:4,7".
func parseFakeUIDSet(s string, maxUID uint32) func(uint32) bool {
	type span struct{ lo, hi uint32 }
	num := func(t string) uint32 {
		if t == "*" {
			return maxUID
		}
		n, _ := strconv.ParseUint(t, 10, 32)
		return uint32(n)
	}
	var spans []span
	for _, part := range strings.Split(s, ",") {
		lo, hi, isRange := strings.Cut(part, ":")
		if !isRange {
			hi = lo
		}
		a, b := num(lo), num(hi)
		if a > b {
			a, b = b, a
		}
		spans = append(spans, span{a, b})
	}
	return func(uid uint32) bool {
		for _, sp := range spans {
			if uid >= sp.lo && uid <= sp.hi {
				return true
			}
		}
		return false
	}
}

func splitCommand(line string) (tag, cmd, args string) {
	tag, rest, _ := strings.Cut(line, " ")
	cmd, args, _ = strings.Cut(rest, " ")
	return tag, strings.ToUpper(cmd), args
}

func unquote(s string) string {
	if u, err := strconv.Unquote(s); err == nil {
		return u
	}
	return s
}
