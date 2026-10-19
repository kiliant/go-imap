package imapserver_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap/imapserver"
)

func TestAppendLiteralLimitRefusals(t *testing.T) {
	for _, suffix := range []string{"", "+"} {
		for name, prefix := range map[string]string{
			"first":          "APPEND INBOX ",
			"later-message":  "APPEND INBOX {1+}\r\nx ",
			"first-catenate": "APPEND INBOX CATENATE (TEXT ",
			"later-catenate": "APPEND INBOX CATENATE (TEXT {1+}\r\nx TEXT ",
		} {
			t.Run(name+"/non-sync="+fmt.Sprint(suffix != ""), func(t *testing.T) {
				h := newSecurityHarness(t, imapserver.Limits{MaxLiteralBytes: 16})
				h.login()
				// Send only the announcement: refusing the oversized payload
				// must not require reading or draining any of its bytes.
				h.write("b " + prefix + "{17" + suffix + "}\r\n")
				if got := readTagged(t, h.reader, "b"); !strings.HasPrefix(got, "b NO [TOOBIG]") {
					t.Fatalf("oversized APPEND = %q", got)
				}
				if suffix != "" {
					if line, err := h.reader.ReadString('\n'); err != io.EOF || line != "" {
						t.Fatalf("oversized non-sync connection = %q, %v; want EOF", line, err)
					}
					h.requireServeReturns()
					return
				}
				h.write("c STATUS INBOX (MESSAGES)\r\n")
				lines, tagged := collectUntilTag(t, h.reader, "c ")
				if !strings.HasPrefix(tagged, "c OK") || len(lines) != 1 || !strings.Contains(lines[0], "MESSAGES 0") {
					t.Fatalf("refusal changed mailbox or framing: %v %q", lines, tagged)
				}
			})
		}
	}
}

func TestDefaultAppendAcceptsMailSizedLiteral(t *testing.T) {
	h := newSecurityHarness(t, imapserver.Limits{})
	h.login()
	body := strings.Repeat("x", 100<<10)
	h.write(fmt.Sprintf("b APPEND INBOX {%d}\r\n", len(body)))
	if line, err := h.reader.ReadString('\n'); err != nil || !strings.HasPrefix(line, "+ ") {
		t.Fatalf("literal continuation = %q, %v", line, err)
	}
	h.write(body + "\r\n")
	if got := readTagged(t, h.reader, "b"); !strings.HasPrefix(got, "b OK") {
		t.Fatalf("mail-sized default APPEND = %q", got)
	}
}

func TestDefaultCommandLifetimeExceedsFormerLimit(t *testing.T) {
	h := newSecurityHarness(t, imapserver.Limits{})
	h.login()
	for range 10_001 {
		h.write("b NOOP\r\n")
		if got := readTagged(t, h.reader, "b"); !strings.HasPrefix(got, "b OK") {
			t.Fatalf("default NOOP = %q", got)
		}
	}
}

func TestPositiveCommandLimitSendsBye(t *testing.T) {
	h := newSecurityHarness(t, imapserver.Limits{MaxCommands: 2})
	h.login()
	h.write("b NOOP\r\n")
	if got := readTagged(t, h.reader, "b"); !strings.HasPrefix(got, "b OK") {
		t.Fatal(got)
	}
	h.write("c NOOP\r\n")
	if line, err := h.reader.ReadString('\n'); err != nil || !strings.HasPrefix(line, "* BYE ") {
		t.Fatalf("command-limit response = %q, %v", line, err)
	}
	if line, err := h.reader.ReadString('\n'); err != io.EOF || line != "" {
		t.Fatalf("after BYE = %q, %v", line, err)
	}
	h.requireServeReturns()
}

func TestStatusAndListStatusRespectLiteralLimit(t *testing.T) {
	h := newSecurityHarness(t, imapserver.Limits{MaxLiteralBytes: 16})
	h.login()
	for _, command := range []string{"STATUS INBOX (APPENDLIMIT)", `LIST "" INBOX RETURN (STATUS (APPENDLIMIT))`} {
		h.write("b " + command + "\r\n")
		lines, tagged := collectUntilTag(t, h.reader, "b ")
		if !strings.HasPrefix(tagged, "b OK") || !strings.Contains(strings.Join(lines, ""), "APPENDLIMIT 16") {
			t.Fatalf("%s = %v %q", command, lines, tagged)
		}
	}
}

func TestNegativeConnectionLimitsAllowSharedSources(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	server := imapserver.New(nil, &imapserver.Options{Limits: imapserver.Limits{MaxConnections: -1, MaxConnectionsPerIP: -1}})
	defer server.Close(ctx, nil)
	// net.Pipe peers share one source. The former per-source default was 32.
	for range 34 {
		client, transport := net.Pipe()
		defer client.Close()
		_ = client.SetDeadline(time.Now().Add(5 * time.Second))
		go func() { _ = server.ServeConn(ctx, transport, nil) }()
		if line, err := bufio.NewReader(client).ReadString('\n'); err != nil || !strings.HasPrefix(line, "* OK ") {
			t.Fatalf("shared-source greeting = %q, %v", line, err)
		}
	}
}
