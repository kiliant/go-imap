package imapclient

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// extAServer is a scripted IMAP server for the group A unit tests. It reads one
// command line at a time and writes whatever the test dictates, so every
// assertion is against literal wire bytes rather than against an abstraction
// that could agree with a bug in the encoder.
type extAServer struct {
	t    *testing.T
	conn net.Conn
	r    *bufio.Reader
}

// command reads one command line and returns its tag and the arguments after
// the tag, with the trailing CRLF removed.
func (s *extAServer) command() (tag, rest string) {
	s.t.Helper()
	line, err := s.r.ReadString('\n')
	if err != nil {
		return "", ""
	}
	line = strings.TrimRight(line, "\r\n")
	tag, rest, _ = strings.Cut(line, " ")
	return tag, rest
}

// reply writes the given lines verbatim, each terminated with CRLF.
func (s *extAServer) reply(lines ...string) {
	s.t.Helper()
	var b strings.Builder
	for _, line := range lines {
		b.WriteString(line)
		b.WriteString("\r\n")
	}
	_, _ = s.conn.Write([]byte(b.String()))
}

// ok completes the command tagged tag with a bare OK.
func (s *extAServer) ok(tag string) { s.reply(tag + " OK done") }

// newExtATestClient starts a client against a scripted server. greeting is the
// untagged greeting line, without CRLF; a CAPABILITY response code in it is how
// a test declares which extensions the server advertises.
func newExtATestClient(t *testing.T, greeting string, serve func(s *extAServer)) (*Client, context.Context) {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer serverConn.Close()
		s := &extAServer{t: t, conn: serverConn, r: bufio.NewReader(serverConn)}
		s.reply(greeting)
		serve(s)
	}()
	c := NewClient(clientConn, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(func() {
		cancel()
		_ = c.Close()
		<-done
	})
	if err := c.WaitGreeting(ctx); err != nil {
		t.Fatalf("greeting: %v", err)
	}
	return c, ctx
}

// selectInbox drives a real SELECT so the session reaches the selected state
// with a known mailbox and UIDVALIDITY, which the SEARCHRES validity checks
// depend on.
func (s *extAServer) selectInbox(mailbox string, uidValidity string) {
	s.t.Helper()
	tag, _ := s.command()
	s.reply("* 3 EXISTS", "* OK [UIDVALIDITY "+uidValidity+"] valid", tag+" OK [READ-WRITE] selected")
	_ = mailbox
}
