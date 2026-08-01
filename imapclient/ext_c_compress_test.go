package imapclient

import (
	"compress/flate"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
)

func TestCompressDeflateLargeFetch(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	var mu sync.Mutex
	var sawCompress, sawFetch bool
	errCh := make(chan error, 1)
	go func() {
		defer serverConn.Close()
		if _, err := serverConn.Write([]byte("* PREAUTH ready\r\n")); err != nil {
			errCh <- err
			return
		}
		line, err := readClearLine(serverConn)
		if err != nil {
			errCh <- err
			return
		}
		mu.Lock()
		sawCompress = strings.Contains(line, "COMPRESS DEFLATE")
		mu.Unlock()
		tag := strings.Fields(line)[0]
		if _, err := serverConn.Write([]byte(tag + " OK deflate active\r\n")); err != nil {
			errCh <- err
			return
		}
		r := flate.NewReader(serverConn)
		w, err := flate.NewWriter(serverConn, flate.DefaultCompression)
		if err != nil {
			errCh <- err
			return
		}
		br := newLineReader(r)
		line, err = br.readLine()
		if err != nil {
			errCh <- err
			return
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			errCh <- fmt.Errorf("empty compressed command")
			return
		}
		tag = fields[0]
		mu.Lock()
		sawFetch = strings.Contains(line, "FETCH")
		mu.Unlock()
		body := strings.Repeat("Z", 64*1024)
		resp := fmt.Sprintf("* 1 FETCH (RFC822.SIZE %d BODY[] {%d}\r\n%s)\r\n%s OK done\r\n", len(body), len(body), body, tag)
		if _, err := io.WriteString(w, resp); err != nil {
			errCh <- err
			return
		}
		if err := w.Flush(); err != nil {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	c := NewClient(clientConn, &Options{ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second})
	t.Cleanup(func() { _ = c.Close() })
	ctx := extCContext(t)
	if err := c.WaitGreeting(ctx); err != nil {
		t.Fatal(err)
	}
	extCReady(c, []string{"IMAP4REV1", "COMPRESS=DEFLATE"}, nil, true)
	if err := c.Compress(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if !c.Compressed() {
		t.Fatal("Compressed() = false after successful COMPRESS")
	}
	cmd := c.Fetch(imap.SeqSetNum(1), &imap.FetchItemBodySection{Peek: true})
	msg, err := cmd.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	section := firstBodySection(t, msg)
	n, err := io.Copy(io.Discard, section.Literal)
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if n != 64*1024 {
		t.Fatalf("fetched %d bytes", n)
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("server goroutine timed out — likely a COMPRESS flush deadlock")
	}
	mu.Lock()
	ok := sawCompress && sawFetch
	mu.Unlock()
	if !ok {
		t.Fatalf("sawCompress=%v sawFetch=%v", sawCompress, sawFetch)
	}
}

func TestCompressBlocksConcurrentWritersDuringUpgrade(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	errCh := make(chan error, 1)
	go func() {
		defer serverConn.Close()
		if _, err := serverConn.Write([]byte("* PREAUTH ready\r\n")); err != nil {
			errCh <- err
			return
		}
		line, err := readClearLine(serverConn)
		if err != nil {
			errCh <- err
			return
		}
		tag := strings.Fields(line)[0]
		// Delay the OK so a concurrent writer can race the Wait→swap window.
		time.Sleep(20 * time.Millisecond)
		if _, err := serverConn.Write([]byte(tag + " OK deflate active\r\n")); err != nil {
			errCh <- err
			return
		}
		r := flate.NewReader(serverConn)
		w, err := flate.NewWriter(serverConn, flate.DefaultCompression)
		if err != nil {
			errCh <- err
			return
		}
		br := newLineReader(r)
		for {
			line, err = br.readLine()
			if err != nil {
				errCh <- err
				return
			}
			fields := strings.Fields(line)
			if len(fields) < 2 {
				errCh <- fmt.Errorf("unexpected compressed line %q", line)
				return
			}
			tag = fields[0]
			if strings.EqualFold(fields[1], "NOOP") {
				if _, err := io.WriteString(w, tag+" OK\r\n"); err != nil {
					errCh <- err
					return
				}
				if err := w.Flush(); err != nil {
					errCh <- err
					return
				}
				errCh <- nil
				return
			}
			if _, err := io.WriteString(w, tag+" BAD unexpected\r\n"); err != nil {
				errCh <- err
				return
			}
			_ = w.Flush()
		}
	}()

	c := NewClient(clientConn, &Options{ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second})
	t.Cleanup(func() { _ = c.Close() })
	ctx := extCContext(t)
	if err := c.WaitGreeting(ctx); err != nil {
		t.Fatal(err)
	}
	extCReady(c, []string{"IMAP4REV1", "COMPRESS=DEFLATE"}, nil, true)

	var noopErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Hammer NOOP while COMPRESS upgrades; it must not go out in cleartext.
		time.Sleep(5 * time.Millisecond)
		noopErr = c.Noop().Wait(ctx)
	}()

	if err := c.Compress(ctx, nil); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if noopErr != nil {
		t.Fatalf("NOOP after/during COMPRESS: %v", noopErr)
	}
	if !c.Compressed() {
		t.Fatal("Compressed() = false")
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("server timed out — concurrent cleartext write likely desynced the stream")
	}
}

func TestCompressRequiresCapability(t *testing.T) {
	c, server := extCDial(t, func(tag, line string) string { return tag + " OK\r\n" })
	extCReady(c, []string{"IMAP4REV1"}, nil, true)
	err := c.Compress(extCContext(t), nil)
	if !errors.Is(err, ErrCapabilityNotAdvertised) {
		t.Fatalf("err = %v", err)
	}
	if len(server.Lines()) != 0 {
		t.Fatalf("sent: %q", server.Lines())
	}
}

func TestCompressRejectsSecondEnable(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	go func() {
		defer serverConn.Close()
		_, _ = serverConn.Write([]byte("* PREAUTH ready\r\n"))
		line, _ := readClearLine(serverConn)
		tag := strings.Fields(line)[0]
		_, _ = serverConn.Write([]byte(tag + " OK compressed\r\n"))
		buf := make([]byte, 1)
		_, _ = serverConn.Read(buf)
	}()
	c := NewClient(clientConn, nil)
	t.Cleanup(func() { _ = c.Close() })
	ctx := extCContext(t)
	if err := c.WaitGreeting(ctx); err != nil {
		t.Fatal(err)
	}
	extCReady(c, []string{"IMAP4REV1", "COMPRESS=DEFLATE"}, nil, true)
	if err := c.Compress(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Compress(ctx, nil); err == nil {
		t.Fatal("second COMPRESS succeeded")
	}
}

func readClearLine(conn net.Conn) (string, error) {
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var buf []byte
	tmp := make([]byte, 1)
	for {
		n, err := conn.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[0])
			if tmp[0] == '\n' {
				return strings.TrimRight(string(buf), "\r\n"), nil
			}
		}
		if err != nil {
			return string(buf), err
		}
	}
}

type lineReader struct {
	r       io.Reader
	partial []byte
}

func newLineReader(r io.Reader) *lineReader { return &lineReader{r: r} }

func (l *lineReader) readLine() (string, error) {
	_ = l
	for {
		if i := indexByte(l.partial, '\n'); i >= 0 {
			line := string(l.partial[:i])
			l.partial = append([]byte(nil), l.partial[i+1:]...)
			return strings.TrimRight(line, "\r"), nil
		}
		buf := make([]byte, 4096)
		n, err := l.r.Read(buf)
		if n > 0 {
			l.partial = append(l.partial, buf[:n]...)
		}
		if err != nil {
			if len(l.partial) > 0 && errors.Is(err, io.EOF) {
				line := string(l.partial)
				l.partial = nil
				return strings.TrimRight(line, "\r\n"), nil
			}
			return "", err
		}
	}
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

func firstBodySection(t *testing.T, msg *imap.FetchMessageData) *imap.FetchDataBodySection {
	t.Helper()
	for _, values := range msg.Items {
		for _, v := range values {
			if s, ok := v.(*imap.FetchDataBodySection); ok {
				return s
			}
		}
	}
	t.Fatal("no BODY section")
	return nil
}
