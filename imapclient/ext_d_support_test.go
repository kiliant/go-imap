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

// extDServer is a scripted IMAP server for the group D unit tests.
type extDServer struct {
	mu       sync.Mutex
	lines    []string
	literals [][]byte
}

func (s *extDServer) Lines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.lines...)
}

func (s *extDServer) LastLine() string {
	lines := s.Lines()
	if len(lines) == 0 {
		return ""
	}
	return lines[len(lines)-1]
}

// extDDial starts a client against a scripted server. respond is called once
// per command line with the command's tag and the full line (without CRLF).
func extDDial(t *testing.T, respond func(tag, line string) string) (*Client, *extDServer) {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	server := &extDServer{}
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
			if size, ok := extDLiteralSize(line); ok {
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

func extDLiteralSize(line string) (int, bool) {
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

func extDContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func extDReady(c *Client, capabilities []string, enabled []string, selected bool) {
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

// extEDial is the group E alias of extDDial; the two groups share the harness.
func extEDial(t *testing.T, respond func(tag, line string) string) (*Client, *extDServer) {
	return extDDial(t, respond)
}

func extEContext(t *testing.T) context.Context { return extDContext(t) }

func extEReady(c *Client, capabilities []string, enabled []string, selected bool) {
	extDReady(c, capabilities, enabled, selected)
}
