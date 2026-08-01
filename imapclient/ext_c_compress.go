package imapclient

import (
	"compress/flate"
	"context"
	"io"
	"net"
	"sync"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// CompressOptions configures COMPRESS. A nil pointer selects DEFLATE, the only
// mechanism RFC 4978 defines.
//
// Construct with keyed fields only; fields may be added in a future release.
type CompressOptions struct {
	// Mechanism is the compression algorithm name. Empty selects "DEFLATE".
	// A future RFC that registers another mechanism adds a constant here; the
	// method signature does not change.
	Mechanism string

	_ struct{}
}

func (o *CompressOptions) mechanism() string {
	if o == nil || o.Mechanism == "" {
		return "DEFLATE"
	}
	return o.Mechanism
}

// Compress enables IMAP stream compression after the tagged OK.
// COMPRESS, RFC 4978.
//
// The client MUST NOT pipeline any command behind COMPRESS: the RFC forbids
// further commands until the result is seen. Compression starts immediately
// after the CRLF that ends a tagged OK, on both directions. This method wraps
// the connection with DEFLATE (RFC 1951) via [compress/flate] and rebuilds the
// encoder and decoder on that wrapper.
//
// # Flushing
//
// Every command Flush sync-flushes the compressor. Without that, the peer can
// stall waiting for bytes that are still sitting in a deflate block — the
// classic COMPRESS deadlock. Large FETCHes are safe under the same rule: the
// server sync-flushes; this client sync-flushes after every command line and
// every literal chunk that reaches the wrapper's Write.
//
// # Layering with TLS
//
// Compression sits inside TLS (application data is compressed, then encrypted),
// never the other way around. Call this after DialTLS / DialStartTLS.
//
// # Reader pause
//
// Like STARTTLS, the reader goroutine pauses at the tagged OK before the
// transport is swapped, so it cannot mis-parse the first compressed response
// as cleartext. Octets that the previous decoder's bufio already prefetch
// past that OK's CRLF are a residual edge case: draining them needs a
// Decoder.Buffered API from T01 (see .state/progress/T10.md). Typical servers
// send nothing after OK until the next command.
func (c *Client) Compress(ctx context.Context, options *CompressOptions) error {
	if ctx == nil {
		return &imap.Error{Type: imap.ErrorTypeProtocol, Text: "COMPRESS requires a non-nil context"}
	}
	mechanism := options.mechanism()
	capName := "COMPRESS=" + mechanism
	if !c.Supports(capName) && !(mechanism == "DEFLATE" && c.Supports("COMPRESS=DEFLATE")) {
		return capabilityError("COMPRESS", capName)
	}

	c.mu.Lock()
	if _, ok := c.conn.(*deflateConn); ok {
		c.mu.Unlock()
		return &imap.Error{Type: imap.ErrorTypeProtocol, Text: "COMPRESS is already active on this connection"}
	}
	if c.pauseTag != "" {
		c.mu.Unlock()
		return &imap.Error{Type: imap.ErrorTypeProtocol, Text: "COMPRESS cannot run while the reader is paused"}
	}
	if len(c.pendingQ) > 0 {
		c.mu.Unlock()
		return &imap.Error{Type: imap.ErrorTypeProtocol, Text: "COMPRESS cannot be pipelined with pending commands (RFC 4978)"}
	}
	// Fresh pause channels: DialStartTLS leaves the previous pair non-nil after
	// a successful handshake, so a leftover channel is not an active pause —
	// pauseTag is the live signal.
	c.paused = make(chan struct{})
	c.resume = make(chan struct{})
	c.resumeOnce = sync.Once{}
	// pauseTag is bound to the real command tag inside issue (same path as STARTTLS).
	c.mu.Unlock()

	cmd := c.beginCommand("COMPRESS", stateAuthenticated|stateSelected, func(enc *imapwire.Encoder) {
		enc.SP().Atom(mechanism)
	}, nil)
	// Hold writeMu from before Wait through the transport swap. completeTagged
	// removes COMPRESS from pendingQ before Wait returns; without this lock a
	// concurrent issue() would see an empty queue and write cleartext while the
	// server already expects compressed octets (RFC 4978). Matches the
	// STARTTLS upgrade window in DialStartTLS, pulled earlier to cover the
	// Wait→swap gap.
	c.writeMu.Lock()
	if err := cmd.Wait(ctx); err != nil {
		c.writeMu.Unlock()
		c.releaseReader()
		c.clearPause()
		return err
	}
	select {
	case <-c.paused:
	case <-ctx.Done():
		c.writeMu.Unlock()
		c.releaseReader()
		c.clearPause()
		c.poison(ctx.Err())
		return ctx.Err()
	}
	wrapErr := c.enableDeflateLocked()
	c.writeMu.Unlock()
	c.releaseReader()
	c.clearPause()
	if wrapErr != nil {
		// Server is already compressing; a cleartext decoder would desync.
		c.poison(wrapErr)
		return wrapErr
	}
	return nil
}

func (c *Client) clearPause() {
	c.mu.Lock()
	c.pauseTag = ""
	c.paused = nil
	c.resume = nil
	c.mu.Unlock()
}

// enableDeflateLocked replaces the transport with a DEFLATE wrapper.
// The caller must hold writeMu; this also takes mu.
func (c *Client) enableDeflateLocked() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		if c.closeErr != nil {
			return c.closeErr
		}
		return net.ErrClosed
	}
	if _, ok := c.conn.(*deflateConn); ok {
		return &imap.Error{Type: imap.ErrorTypeProtocol, Text: "COMPRESS is already active on this connection"}
	}
	dc, err := newDeflateConn(c.conn)
	if err != nil {
		return &imap.Error{Type: imap.ErrorTypeProtocol, Text: "COMPRESS DEFLATE setup failed", Err: err}
	}
	utf8Accept := c.enc.UTF8Accept()
	wopts := c.opts.wireOptions()
	eopts := c.opts.encoderOptions()
	c.conn = dc
	c.dec = imapwire.NewDecoder(dc, &wopts)
	c.enc = imapwire.NewEncoder(dc, &eopts)
	c.dec.SetUTF8Accept(utf8Accept)
	c.enc.SetUTF8Accept(utf8Accept)
	return nil
}

// deflateConn is a net.Conn that DEFLATE-compresses writes and decompresses
// reads. Write always ends with a sync flush so the peer is never left waiting
// for a block that has not been emitted.
type deflateConn struct {
	net.Conn
	r     io.ReadCloser
	w     *flate.Writer
	write sync.Mutex
	read  sync.Mutex
}

func newDeflateConn(conn net.Conn) (*deflateConn, error) {
	w, err := flate.NewWriter(conn, flate.DefaultCompression)
	if err != nil {
		return nil, err
	}
	return &deflateConn{
		Conn: conn,
		r:    flate.NewReader(conn),
		w:    w,
	}, nil
}

func (c *deflateConn) Read(p []byte) (int, error) {
	c.read.Lock()
	defer c.read.Unlock()
	return c.r.Read(p)
}

func (c *deflateConn) Write(p []byte) (int, error) {
	c.write.Lock()
	defer c.write.Unlock()
	n, err := c.w.Write(p)
	if err != nil {
		return n, err
	}
	// Sync flush: RFC 4978 section 4. Without this, IMAP deadlocks whenever
	// the peer is waiting for a command or response that is still buffered
	// inside the compressor.
	if err := c.w.Flush(); err != nil {
		return n, err
	}
	return n, nil
}

func (c *deflateConn) Close() error {
	c.write.Lock()
	_ = c.w.Close()
	c.write.Unlock()
	c.read.Lock()
	_ = c.r.Close()
	c.read.Unlock()
	return c.Conn.Close()
}

func (c *deflateConn) SetDeadline(t time.Time) error {
	return c.Conn.SetDeadline(t)
}

func (c *deflateConn) SetReadDeadline(t time.Time) error {
	return c.Conn.SetReadDeadline(t)
}

func (c *deflateConn) SetWriteDeadline(t time.Time) error {
	return c.Conn.SetWriteDeadline(t)
}

// Compressed reports whether COMPRESS is active on this connection.
func (c *Client) Compressed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.conn.(*deflateConn)
	return ok
}
