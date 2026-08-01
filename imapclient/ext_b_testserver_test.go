package imapclient

import (
	"bufio"
	"context"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// extBServer is a scripted IMAP server for the group B unit tests. It records
// every command line the client writes and answers each one from a handler, so
// a test can assert both the exact bytes sent and the client's handling of a
// recorded server response.
type extBServer struct {
	mu       sync.Mutex
	lines    []string
	literals [][]byte
}

// Lines returns the command lines the client has written so far.
func (s *extBServer) Lines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.lines...)
}

// Literals returns the literal payloads the client has streamed so far.
func (s *extBServer) Literals() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]byte(nil), s.literals...)
}

// LastLine returns the most recent command line, or "" if there is none.
func (s *extBServer) LastLine() string {
	lines := s.Lines()
	if len(lines) == 0 {
		return ""
	}
	return lines[len(lines)-1]
}

// extBDial starts a client against a scripted server. respond is called once
// per command line with the command's tag and the full line (without CRLF) and
// returns the raw bytes to write back.
//
// A command line ending in a synchronising literal announcement is answered
// with a continuation request, and the payload is consumed and recorded before
// respond is called with the completed line.
func extBDial(t *testing.T, respond func(tag, line string) string) (*Client, *extBServer) {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	server := &extBServer{}
	go func() {
		defer serverConn.Close()
		_, _ = serverConn.Write([]byte("* PREAUTH ready\r\n"))
		r := bufio.NewReader(serverConn)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			fields := strings.Fields(line)
			if len(fields) == 0 {
				return
			}
			tag := fields[0]
			if size, ok := literalSize(line); ok {
				if _, err := serverConn.Write([]byte("+ ready\r\n")); err != nil {
					return
				}
				payload := make([]byte, size)
				if _, err := io.ReadFull(r, payload); err != nil {
					return
				}
				rest, err := r.ReadString('\n')
				if err != nil {
					return
				}
				server.mu.Lock()
				server.literals = append(server.literals, payload)
				server.mu.Unlock()
				line += strings.TrimRight(rest, "\r\n")
			}
			server.mu.Lock()
			server.lines = append(server.lines, line)
			server.mu.Unlock()
			if _, err := serverConn.Write([]byte(respond(tag, line))); err != nil {
				return
			}
		}
	}()
	c := NewClient(clientConn, nil)
	t.Cleanup(func() { _ = c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.WaitGreeting(ctx, nil); err != nil {
		t.Fatalf("greeting: %v", err)
	}
	return c, server
}

// literalSize reports the announced size of a synchronising literal at the end
// of a command line.
func literalSize(line string) (int, bool) {
	if !strings.HasSuffix(line, "}") {
		return 0, false
	}
	open := strings.LastIndex(line, "{")
	if open < 0 {
		return 0, false
	}
	inner := line[open+1 : len(line)-1]
	inner = strings.TrimSuffix(inner, "+")
	size, err := strconv.Atoi(inner)
	if err != nil || size < 0 {
		return 0, false
	}
	return size, true
}

func extBContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// extBReady puts the client in the state the group B commands need: a known
// capability set, optionally some enabled extensions, and a selected mailbox.
func extBReady(c *Client, capabilities []string, enabled []string, selected bool) {
	c.setCapabilities(capabilities)
	c.mu.Lock()
	for _, name := range enabled {
		c.enabled[strings.ToUpper(name)] = struct{}{}
	}
	if selected {
		c.state = StateSelected
		c.selectedMailbox = "INBOX"
	} else {
		c.state = StateAuthenticated
	}
	c.mu.Unlock()
}
