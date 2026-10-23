package imapserver_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
	"github.com/kiliant/go-imap/imapserver/maildir"
	"github.com/kiliant/go-imap/imapserver/memory"
)

// nulWire is a raw client that keeps literal payloads byte for byte, which
// the r4 helpers summarise as "<n octets>".
type nulWire struct {
	t *testing.T
	c net.Conn
	r *bufio.Reader
}

var nulLiteral = regexp.MustCompile(`~?\{(\d+)\}\r\n$`)

func openNULWire(t *testing.T, b imapserver.Backend) *nulWire {
	t.Helper()
	c, peer := net.Pipe()
	s := imapserver.New(b, &imapserver.Options{AllowInsecureAuth: true})
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(func() { cancel(); _ = c.Close(); _ = s.Close(context.Background(), nil) })
	go func() { _ = s.ServeConn(ctx, peer, nil) }()
	w := &nulWire{t: t, c: c, r: bufio.NewReader(c)}
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := w.r.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	w.cmd("L", "LOGIN alice secret")
	return w
}

// cmd sends one command and returns everything up to and including its
// tagged response, with literal payloads verbatim.
func (w *nulWire) cmd(tag, command string) string {
	w.t.Helper()
	go func() { _, _ = io.WriteString(w.c, tag+" "+command+"\r\n") }()
	var all strings.Builder
	for {
		var line strings.Builder
		for {
			part, err := w.r.ReadString('\n')
			line.WriteString(part)
			if err != nil {
				w.t.Fatalf("reading for %s: %v (so far %q)", tag, err, all.String()+line.String())
			}
			m := nulLiteral.FindStringSubmatch(part)
			if m == nil {
				break
			}
			n, _ := strconv.Atoi(m[1])
			payload := make([]byte, n)
			if _, err := io.ReadFull(w.r, payload); err != nil {
				w.t.Fatalf("reading literal for %s: %v", tag, err)
			}
			line.Write(payload)
		}
		all.WriteString(line.String())
		if strings.HasPrefix(line.String(), tag+" ") {
			if !strings.HasPrefix(line.String(), tag+" OK") {
				w.t.Fatalf("%s %s = %q", tag, command, all.String())
			}
			return all.String()
		}
	}
}

func requireContains(t *testing.T, got, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Fatalf("response %q\ndoes not contain %q", got, want)
	}
}

// A message delivered into Maildir from outside the server can hold a NUL in
// an 8bit part. BODY sends it as 0x80 with the stored length, and BINARY
// sends the original octet in a literal8 (#210).
func TestStoredNULIsSentAs0x80(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	options := &maildir.Options{Accounts: []maildir.AccountOptions{{Username: "alice", Password: "secret"}}}
	b, err := maildir.Open(ctx, root, options)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Close(ctx, nil); err != nil {
		t.Fatal(err)
	}
	message := "From: a@example.test\r\nSubject: nul\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: text/plain\r\nContent-Transfer-Encoding: 8bit\r\n\r\na\x00b\r\n"
	account := fmt.Sprintf("%x", sha256.Sum256([]byte("alice")))
	if err := os.WriteFile(filepath.Join(root, account, "mailboxes", "INBOX", "new", "nul-delivery"), []byte(message), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err = maildir.Open(ctx, root, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close(context.Background(), nil) })

	w := openNULWire(t, b)
	// IMAP4rev2 makes BINARY fetch available on Maildir.
	w.cmd("E", "ENABLE IMAP4REV2")
	w.cmd("S", "SELECT INBOX")
	got := w.cmd("F", "FETCH 1 (RFC822.SIZE BODYSTRUCTURE BODY.PEEK[TEXT])")
	requireContains(t, got, fmt.Sprintf("RFC822.SIZE %d", len(message)))
	requireContains(t, got, `"8bit" 5`)
	requireContains(t, got, "BODY[TEXT] {5}\r\na\x80b\r\n")
	got = w.cmd("G", "FETCH 1 BODY.PEEK[]")
	requireContains(t, got, fmt.Sprintf("BODY[] {%d}\r\n%s", len(message), strings.ReplaceAll(message, "\x00", "\x80")))
	got = w.cmd("B", "FETCH 1 BINARY.PEEK[1]")
	requireContains(t, got, "BINARY[1] ~{5}\r\na\x00b\r\n")
	w.cmd("N", "NOOP")
}

// nulBackend answers FETCH with NUL in each kind of literal reader the
// framework handles: one exposing Len, and an opaque one that is staged to a
// temporary file and measured by seeking. It answers URLFETCH with NUL too.
type nulBackend struct{ *memory.Backend }

type nulSession struct {
	imapserver.Session
	imapserver.URLAuthSession
}

type nulMailbox struct{ imapserver.SelectedMailbox }

func (b *nulBackend) Authenticate(ctx context.Context, info *imapserver.ConnInfo, credentials *imapserver.Credentials, options *imapserver.AuthenticateOptions) (imapserver.Session, error) {
	session, err := b.Backend.Authenticate(ctx, info, credentials, options)
	if err != nil {
		return nil, err
	}
	return &nulSession{Session: session, URLAuthSession: session.(imapserver.URLAuthSession)}, nil
}

func (s *nulSession) Select(ctx context.Context, name string, updater *imapserver.Updater, options *imapserver.SelectOptions) (*imapserver.SelectResult, error) {
	result, err := s.Session.Select(ctx, name, updater, options)
	if err == nil {
		result.Mailbox = &nulMailbox{result.Mailbox}
	}
	return result, err
}

func (s *nulSession) FetchURLAuth(context.Context, string, *imapserver.URLFetchOptions) (*imapserver.URLFetchResult, error) {
	return &imapserver.URLFetchResult{Body: io.NopCloser(strings.NewReader("a\x00b")), HasSize: true, Size: 3}, nil
}

func (m *nulMailbox) Fetch(ctx context.Context, writer *imapserver.FetchWriter, uids imap.UIDSet, options *imapserver.FetchOptions) error {
	return writer.WriteMessage(ctx, &imap.FetchMessageData{Items: map[imap.FetchDataKey][]imap.FetchData{
		"UID":         {imap.FetchDataUID(1)},
		"BODY[1]":     {&imap.FetchDataBodySection{Part: []int{1}, Literal: bytes.NewReader([]byte("a\x00b"))}},
		"RFC822.TEXT": {&imap.FetchDataLiteral{Literal: io.MultiReader(strings.NewReader("\x00x\x00"))}},
		"BINARY[1]":   {&imap.FetchDataBinarySection{Part: []int{1}, Literal: bytes.NewReader([]byte("a\x00b"))}},
	}})
}

func TestBackendNULIsSentAs0x80(t *testing.T) {
	w := openNULWire(t, &nulBackend{r4Memory()})
	w.cmd("A", "APPEND INBOX {13}\r\nSubject: x\r\n\r\n")
	w.cmd("S", "SELECT INBOX")
	got := w.cmd("F", "UID FETCH 1 (UID)")
	requireContains(t, got, "* 1 FETCH (BINARY[1] ~{3}\r\na\x00b BODY[1] {3}\r\na\x80b RFC822.TEXT {3}\r\n\x80x\x80 UID 1)\r\n")
	got = w.cmd("U", `URLFETCH "imap://localhost/INBOX/;UID=1"`)
	requireContains(t, got, "{3}\r\na\x80b\r\n")
	w.cmd("N", "NOOP")
}
