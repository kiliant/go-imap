package harness

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxResponseLine = 1 << 20
	commandTimeout  = 10 * time.Second
)

// Trace is a concurrency-safe, credential-redacting IMAP wire trace.
type Trace struct {
	mu sync.Mutex
	b  strings.Builder
}

func (t *Trace) add(direction, line string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b.WriteString(direction)
	t.b.WriteString(" ")
	t.b.WriteString(redactClientLine(line))
	if !strings.HasSuffix(line, "\n") {
		t.b.WriteByte('\n')
	}
}

// String returns a snapshot of the trace.
func (t *Trace) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.b.String()
}

func redactClientLine(line string) string {
	trimmed := strings.TrimRight(line, "\r\n")
	fields := strings.Fields(trimmed)
	if len(fields) >= 2 {
		switch strings.ToUpper(fields[1]) {
		case "LOGIN":
			return fields[0] + " LOGIN <redacted> <redacted>\r\n"
		case "AUTHENTICATE":
			mechanism := ""
			if len(fields) > 2 {
				mechanism = " " + fields[2]
			}
			return fields[0] + " AUTHENTICATE" + mechanism + " <redacted>\r\n"
		}
	}
	// A client line without a tag while AUTHENTICATE is active is never sent
	// through this helper by Session; keep the conservative redaction marker
	// available for callers which record such payloads directly.
	if len(fields) == 1 && !strings.HasPrefix(fields[0], "+") {
		return "<redacted-auth-payload>\r\n"
	}
	return line
}

// Session is the intentionally small raw-IMAP client used to provision and
// smoke-test servers before the production client is involved.
type Session struct {
	conn  net.Conn
	read  *bufio.Reader
	trace *Trace
	tag   uint64
}

// DialSession connects, validates the greeting, and returns a raw session.
func DialSession(ctx context.Context, address string, trace *Trace) (*Session, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	return openSession(ctx, conn, trace)
}

func openSession(ctx context.Context, conn net.Conn, trace *Trace) (*Session, error) {
	s := &Session{conn: conn, read: bufio.NewReaderSize(conn, 32<<10), trace: trace}
	if err := s.setDeadline(ctx); err != nil {
		conn.Close()
		return nil, err
	}
	line, err := s.readLine()
	if err != nil {
		conn.Close()
		return nil, err
	}
	upper := strings.ToUpper(string(line))
	if !strings.HasPrefix(upper, "* OK") && !strings.HasPrefix(upper, "* PREAUTH") {
		conn.Close()
		return nil, fmt.Errorf("interop: unexpected greeting %q", line)
	}
	return s, nil
}

func (s *Session) setDeadline(ctx context.Context) error {
	deadline := time.Now().Add(commandTimeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	return s.conn.SetDeadline(deadline)
}

func (s *Session) readLine() ([]byte, error) {
	line, err := readBoundedLine(s.read, maxResponseLine)
	if len(line) > 0 && s.trace != nil {
		s.trace.add("S:", string(line))
	}
	return line, err
}

func readBoundedLine(src io.Reader, limit int) ([]byte, error) {
	if limit < 2 {
		return nil, fmt.Errorf("interop: invalid line limit %d", limit)
	}
	line := make([]byte, 0, min(limit, 4096))
	var one [1]byte
	for len(line) < limit {
		n, err := src.Read(one[:])
		if n == 1 {
			line = append(line, one[0])
			if one[0] == '\n' {
				if len(line) < 2 || line[len(line)-2] != '\r' {
					return line, fmt.Errorf("interop: response line ended with LF without CR")
				}
				return line, nil
			}
		}
		if err != nil {
			if len(line) != 0 {
				return line, fmt.Errorf("interop: truncated response line: %w", err)
			}
			return nil, err
		}
		if n == 0 {
			return line, io.ErrNoProgress
		}
	}
	return line, fmt.Errorf("interop: response line exceeds %d bytes", limit)
}

func (s *Session) writeLine(line string) error {
	if s.trace != nil {
		s.trace.add("C:", line)
	}
	_, err := io.WriteString(s.conn, line)
	return err
}

func (s *Session) nextTag() string {
	s.tag++
	return fmt.Sprintf("H%04d", s.tag)
}

type response struct {
	status   string
	text     string
	untagged [][]byte
}

func (s *Session) command(ctx context.Context, command string) (response, error) {
	if err := s.setDeadline(ctx); err != nil {
		return response{}, err
	}
	tag := s.nextTag()
	if err := s.writeLine(tag + " " + command + "\r\n"); err != nil {
		return response{}, err
	}
	return s.readCompletion(tag)
}

func (s *Session) readCompletion(tag string) (response, error) {
	var result response
	for {
		line, err := s.readLine()
		if err != nil {
			return response{}, err
		}
		trimmed := bytes.TrimSuffix(line, []byte("\r\n"))
		if bytes.HasPrefix(trimmed, []byte("* ")) {
			result.untagged = append(result.untagged, bytes.Clone(trimmed))
			continue
		}
		fields := bytes.Fields(trimmed)
		if len(fields) < 2 || !bytes.EqualFold(fields[0], []byte(tag)) {
			return response{}, fmt.Errorf("interop: response tag mismatch: got %q, want %s", trimmed, tag)
		}
		result.status = strings.ToUpper(string(fields[1]))
		if len(fields) > 2 {
			result.text = string(bytes.Join(fields[2:], []byte(" ")))
		}
		if result.status != "OK" {
			return result, fmt.Errorf("interop: %s: %s", result.status, result.text)
		}
		return result, nil
	}
}

// Login authenticates using the fixed interop account.
func (s *Session) Login(ctx context.Context) error {
	_, err := s.command(ctx, "LOGIN "+quote("interop@example.test")+" "+quote("interop-pw"))
	return err
}

// Capabilities asks the authenticated server for its current capabilities.
func (s *Session) Capabilities(ctx context.Context) (map[string]bool, error) {
	resp, err := s.command(ctx, "CAPABILITY")
	if err != nil {
		return nil, err
	}
	caps := make(map[string]bool)
	for _, line := range resp.untagged {
		fields := bytes.Fields(line)
		if len(fields) < 2 || !bytes.EqualFold(fields[1], []byte("CAPABILITY")) {
			continue
		}
		for _, cap := range fields[2:] {
			caps[strings.ToUpper(string(cap))] = true
		}
	}
	if len(caps) == 0 {
		return nil, errors.New("interop: CAPABILITY returned no capabilities")
	}
	return caps, nil
}

// Create creates a mailbox.
func (s *Session) Create(ctx context.Context, mailbox string) error {
	_, err := s.command(ctx, "CREATE "+quote(mailbox))
	return err
}

// Select selects a mailbox and validates the tagged completion.
func (s *Session) Select(ctx context.Context, mailbox string) error {
	_, err := s.command(ctx, "SELECT "+quote(mailbox))
	return err
}

// Noop runs a minimal authenticated command.
func (s *Session) Noop(ctx context.Context) error {
	_, err := s.command(ctx, "NOOP")
	return err
}

// Append streams exactly size bytes into a synchronising IMAP literal.
func (s *Session) Append(ctx context.Context, mailbox string, flags []string, size int64, body io.Reader) error {
	if size < 0 {
		return fmt.Errorf("interop: negative APPEND size %d", size)
	}
	if err := s.setDeadline(ctx); err != nil {
		return err
	}
	tag := s.nextTag()
	flagList := ""
	if len(flags) != 0 {
		flagList = " (" + strings.Join(flags, " ") + ")"
	}
	line := tag + " APPEND " + quote(mailbox) + flagList + " {" + strconv.FormatInt(size, 10) + "}\r\n"
	if err := s.writeLine(line); err != nil {
		return err
	}
	continuation, err := s.readLine()
	if err != nil {
		return err
	}
	if !bytes.HasPrefix(continuation, []byte("+")) {
		return fmt.Errorf("interop: APPEND expected continuation, got %q", continuation)
	}
	written, err := io.CopyN(s.conn, body, size)
	if err != nil {
		return fmt.Errorf("interop: APPEND body after %d of %d bytes: %w", written, size, err)
	}
	var extra [1]byte
	if n, _ := body.Read(extra[:]); n != 0 {
		return fmt.Errorf("interop: APPEND body exceeds declared size %d", size)
	}
	if _, err := io.WriteString(s.conn, "\r\n"); err != nil {
		return err
	}
	_, err = s.readCompletion(tag)
	return err
}

// Logout closes the protocol session. Close should still be deferred in case
// the server terminates the connection before its tagged completion.
func (s *Session) Logout(ctx context.Context) error {
	_, err := s.command(ctx, "LOGOUT")
	return err
}

// Close closes the underlying network connection.
func (s *Session) Close() error { return s.conn.Close() }

func quote(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "\"", "\\\"")
	return "\"" + value + "\""
}
