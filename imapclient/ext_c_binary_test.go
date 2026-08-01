package imapclient

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/kiliant/go-imap"
)

func TestFetchBinaryNative(t *testing.T) {
	payload := "hello\x00world"
	c, _ := extCDial(t, func(tag, line string) string {
		if strings.Contains(line, "UID FETCH") {
			return fmt.Sprintf("* 1 FETCH (UID 7 BINARY[] ~{%d}\r\n%s)\r\n%s OK done\r\n", len(payload), payload, tag)
		}
		return tag + " BAD unexpected\r\n"
	})
	extCReady(c, []string{"IMAP4REV1", "BINARY"}, nil, true)
	data, err := c.FetchBinaryUID(extCContext(t), 7, &imap.FetchItemBinarySection{Peek: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if data.FellBack || string(data.Content) != payload {
		t.Fatalf("data = %#v content=%q", data, data.Content)
	}
}

func TestFetchBinaryEmptyFetchIsProtocolError(t *testing.T) {
	c, _ := extCDial(t, func(tag, line string) string {
		return tag + " OK nothing fetched\r\n"
	})
	extCReady(c, []string{"IMAP4REV1", "BINARY"}, nil, true)
	_, err := c.FetchBinary(extCContext(t), 1, nil, &BinaryFetchOptions{DisableUnknownCTEFallback: true})
	if err == nil {
		t.Fatal("expected error")
	}
	var ie *imap.Error
	if !errors.As(err, &ie) {
		t.Fatalf("err = %v (%T), want *imap.Error", err, err)
	}
	if errors.Is(err, io.EOF) {
		t.Fatal("bare io.EOF must not be returned")
	}
}

func TestFetchBinaryUnknownCTEFallback(t *testing.T) {
	encoded := "aGVsbG8="
	decoded := "hello"
	step := 0
	c, _ := extCDial(t, func(tag, line string) string {
		step++
		switch {
		case strings.Contains(line, "BINARY"):
			return tag + " NO [UNKNOWN-CTE] cannot decode\r\n"
		case strings.Contains(line, "BODY.PEEK"):
			msg := "Content-Transfer-Encoding: base64\r\n\r\n" + encoded
			return fmt.Sprintf("* 1 FETCH (BODY[] {%d}\r\n%s)\r\n%s OK done\r\n", len(msg), msg, tag)
		default:
			return tag + " BAD unexpected\r\n"
		}
	})
	extCReady(c, []string{"IMAP4REV1", "BINARY"}, nil, true)
	data, err := c.FetchBinary(extCContext(t), 1, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !data.FellBack || data.CTE == "" || string(data.Content) != decoded {
		t.Fatalf("data = %#v content=%q", data, data.Content)
	}
}

func TestFetchBinaryUnknownCTEAfterEmptyFetch(t *testing.T) {
	// Server sends a FETCH without BINARY, then tags NO [UNKNOWN-CTE].
	encoded := "aGVsbG8="
	c, _ := extCDial(t, func(tag, line string) string {
		switch {
		case strings.Contains(line, "BINARY"):
			return "* 1 FETCH (FLAGS (\\Seen))\r\n" + tag + " NO [UNKNOWN-CTE] cannot decode\r\n"
		case strings.Contains(line, "BODY.PEEK"):
			msg := "Content-Transfer-Encoding: base64\r\n\r\n" + encoded
			return fmt.Sprintf("* 1 FETCH (BODY[] {%d}\r\n%s)\r\n%s OK done\r\n", len(msg), msg, tag)
		default:
			return tag + " BAD unexpected\r\n"
		}
	})
	extCReady(c, []string{"IMAP4REV1", "BINARY"}, nil, true)
	data, err := c.FetchBinary(extCContext(t), 1, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !data.FellBack || string(data.Content) != "hello" {
		t.Fatalf("data = %#v content=%q", data, data.Content)
	}
}

func TestFetchBinaryFallbackWithoutCapability(t *testing.T) {
	encoded := "aGVsbG8="
	c, server := extCDial(t, func(tag, line string) string {
		msg := "Content-Transfer-Encoding: base64\r\n\r\n" + encoded
		return fmt.Sprintf("* 1 FETCH (BODY[] {%d}\r\n%s)\r\n%s OK done\r\n", len(msg), msg, tag)
	})
	extCReady(c, []string{"IMAP4REV1"}, nil, true)
	data, err := c.FetchBinary(extCContext(t), 1, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !data.FellBack || string(data.Content) != "hello" {
		t.Fatalf("data = %#v", data)
	}
	if strings.Contains(server.LastLine(), "BINARY") {
		t.Fatalf("BINARY was sent without capability: %q", server.LastLine())
	}
}

func TestFetchBinaryDisableFallback(t *testing.T) {
	c, server := extCDial(t, func(tag, line string) string {
		return tag + " NO [UNKNOWN-CTE] cannot decode\r\n"
	})
	extCReady(c, []string{"IMAP4REV1", "BINARY"}, nil, true)
	_, err := c.FetchBinary(extCContext(t), 1, nil, &BinaryFetchOptions{DisableUnknownCTEFallback: true})
	if err == nil {
		t.Fatal("expected UNKNOWN-CTE error")
	}
	var ie *imap.Error
	if !errors.As(err, &ie) || !strings.EqualFold(string(ie.Code), string(imap.CodeUnknownCTE)) {
		t.Fatalf("err = %v", err)
	}
	if len(server.Lines()) != 1 {
		t.Fatalf("lines = %q", server.Lines())
	}
}

func TestFetchBinarySize(t *testing.T) {
	c, _ := extCDial(t, func(tag, line string) string {
		return "* 1 FETCH (BINARY.SIZE[] 42)\r\n" + tag + " OK done\r\n"
	})
	extCReady(c, []string{"IMAP4REV1", "BINARY"}, nil, true)
	size, err := c.FetchBinarySize(extCContext(t), 1, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if size != 42 {
		t.Fatalf("size = %d", size)
	}
}

func TestFetchBinarySizeRequiresCapability(t *testing.T) {
	c, server := extCDial(t, func(tag, line string) string { return tag + " OK\r\n" })
	extCReady(c, []string{"IMAP4REV1"}, nil, true)
	_, err := c.FetchBinarySize(extCContext(t), 1, nil, nil)
	if !errors.Is(err, ErrCapabilityNotAdvertised) {
		t.Fatalf("err = %v", err)
	}
	if len(server.Lines()) != 0 {
		t.Fatalf("sent: %q", server.Lines())
	}
}

func TestFetchBinarySizeRejectsZeroID(t *testing.T) {
	c, server := extCDial(t, func(tag, line string) string { return tag + " OK\r\n" })
	extCReady(c, []string{"IMAP4REV1", "BINARY"}, nil, true)
	_, err := c.FetchBinarySize(extCContext(t), 0, nil, nil)
	if err == nil {
		t.Fatal("expected zero-id rejection")
	}
	if len(server.Lines()) != 0 {
		t.Fatalf("sent: %q", server.Lines())
	}
}
