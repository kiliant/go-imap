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

// extCServer is a scripted IMAP server for group C unit tests.
type extCServer struct {
	mu       sync.Mutex
	lines    []string
	literals [][]byte
}

func (s *extCServer) Lines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.lines...)
}

func (s *extCServer) Literals() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]byte(nil), s.literals...)
}

func (s *extCServer) LastLine() string {
	lines := s.Lines()
	if len(lines) == 0 {
		return ""
	}
	return lines[len(lines)-1]
}

// extCDial starts a client against a scripted PREAUTH server.
func extCDial(t *testing.T, respond func(tag, line string) string) (*Client, *extCServer) {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	server := &extCServer{}
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
			full := line
			for {
				size, ok := extCLiteralSize(line)
				if !ok {
					break
				}
				if _, err := serverConn.Write([]byte("+ ready\r\n")); err != nil {
					return
				}
				payload := make([]byte, size)
				if _, err := io.ReadFull(r, payload); err != nil {
					return
				}
				server.mu.Lock()
				server.literals = append(server.literals, payload)
				server.mu.Unlock()
				rest, err := r.ReadString('\n')
				if err != nil {
					return
				}
				rest = strings.TrimRight(rest, "\r\n")
				full += rest
				// The next synchronising-literal announcement, if any, is in
				// this fragment alone — do not keep matching the earlier "{n}".
				line = rest
			}
			server.mu.Lock()
			server.lines = append(server.lines, full)
			server.mu.Unlock()
			if _, err := serverConn.Write([]byte(respond(tag, full))); err != nil {
				return
			}
		}
	}()
	c := NewClient(clientConn, nil)
	t.Cleanup(func() { _ = c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	if err := c.WaitGreeting(ctx, nil); err != nil {
		t.Fatalf("greeting: %v", err)
	}
	return c, server
}

func extCLiteralSize(line string) (int, bool) {
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

func extCContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func extCReady(c *Client, capabilities []string, enabled []string, selected bool) {
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
