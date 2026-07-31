package imapclient

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"strings"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// Dial connects to an IMAP server without TLS. Prefer [DialTLS] or
// [DialStartTLS] whenever the server supports either secure transport.
func Dial(ctx context.Context, address string, opts *Options) (*Client, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	c := NewClient(conn, opts)
	if err := c.WaitGreeting(ctx); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

// DialTLS connects to an IMAP server using TLS from the first byte. TLS 1.2
// and certificate verification are required unless Options.InsecureSkipVerify
// is explicitly set for a controlled test server.
func DialTLS(ctx context.Context, address string, opts *Options) (*Client, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	tlsConn := tls.Client(conn, tlsConfig(address, opts))
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	c := NewClient(tlsConn, opts)
	if err := c.WaitGreeting(ctx); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

// DialStartTLS connects in cleartext, completes STARTTLS, discards all
// cleartext capabilities, and reissues CAPABILITY over TLS before returning.
// The re-query prevents a modified pre-TLS capability list from influencing
// security decisions.
func DialStartTLS(ctx context.Context, address string, opts *Options) (*Client, error) {
	c, err := Dial(ctx, address, opts)
	if err != nil {
		return nil, err
	}
	if err := c.startTLS(ctx, address); err != nil {
		_ = c.Close()
		return nil, err
	}
	if err := c.requestCapability(ctx); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

func tlsConfig(address string, opts *Options) *tls.Config {
	var config *tls.Config
	if opts != nil && opts.TLSConfig != nil {
		config = opts.TLSConfig.Clone()
	} else {
		config = &tls.Config{}
	}
	if config.MinVersion < tls.VersionTLS12 {
		config.MinVersion = tls.VersionTLS12
	}
	if config.ServerName == "" {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			host = address
		}
		config.ServerName = host
	}
	// Do not let a caller smuggle an insecure setting through TLSConfig: the
	// dedicated Options field is the only documented and auditable opt-in.
	config.InsecureSkipVerify = opts != nil && opts.InsecureSkipVerify
	return config
}

func (c *Client) startTLS(ctx context.Context, address string) error {
	c.mu.Lock()
	if c.closed {
		err := c.closeErr
		c.mu.Unlock()
		if err == nil {
			return net.ErrClosed
		}
		return err
	}
	c.paused = make(chan struct{})
	c.resume = make(chan struct{})
	c.mu.Unlock()
	cmd := c.beginCommand("STARTTLS", stateNotAuthenticated, nil, nil)
	if err := cmd.Wait(ctx); err != nil {
		return err
	}
	select {
	case <-c.paused:
	case <-ctx.Done():
		c.releaseReader()
		c.poison(ctx.Err())
		return ctx.Err()
	}

	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	tlsConn := tls.Client(conn, tlsConfig(address, &c.opts))
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		c.releaseReader()
		c.poison(err)
		return err
	}
	c.writeMu.Lock()
	c.mu.Lock()
	c.conn = tlsConn
	wopts := c.opts.wireOptions()
	eopts := c.opts.encoderOptions()
	c.dec = imapwire.NewDecoder(tlsConn, &wopts)
	c.enc = imapwire.NewEncoder(tlsConn, &eopts)
	c.caps = make(map[string]struct{}) // Cleartext capabilities are untrusted.
	c.enabled = make(map[string]struct{})
	c.mu.Unlock()
	c.writeMu.Unlock()
	c.releaseReader()
	return nil
}

func (c *Client) releaseReader() {
	c.mu.Lock()
	resume := c.resume
	c.mu.Unlock()
	if resume != nil {
		c.resumeOnce.Do(func() { close(resume) })
	}
}

func (c *Client) requestCapability(ctx context.Context) error {
	cmd := c.beginCommand("CAPABILITY", stateNotAuthenticated|stateAuthenticated|stateSelected, nil, nil)
	return cmd.Wait(ctx)
}

type commandCollector func(*untaggedResponse) (claimed bool, err error)

type untaggedResponse struct {
	name   string
	number uint32
	hasNum bool
	dec    *imapwire.Decoder
	cond   *imapwire.RespCond
}

func (c *Client) readResponses() {
	defer close(c.readerDone)
	greeting := true
	for {
		c.mu.Lock()
		dec := c.dec
		closed := c.closed
		c.mu.Unlock()
		if closed {
			c.setGreeting(c.closeErr)
			return
		}
		kind, tag, err := dec.BeginResponse()
		if err != nil {
			if greeting {
				c.setGreeting(protocolError(err))
			}
			c.poison(protocolError(err))
			return
		}
		switch kind {
		case imapwire.ResponseTagged:
			if !dec.ExpectSP() {
				c.readerFailure(greeting, dec.Err())
				return
			}
			var cond imapwire.RespCond
			if !dec.ExpectRespCond(&cond) || !dec.ExpectCRLF() {
				c.readerFailure(greeting, dec.Err())
				return
			}
			if greeting || (cond.Status != "OK" && cond.Status != "NO" && cond.Status != "BAD") {
				c.readerFailure(greeting, fmt.Errorf("invalid tagged response condition %q", cond.Status))
				return
			}
			c.completeTagged(tag, cond)
		case imapwire.ResponseContinuation:
			var text string
			if !dec.ExpectContinuationText(&text) {
				c.readerFailure(greeting, dec.Err())
				return
			}
			if err := c.deliverContinuation(text); err != nil {
				c.readerFailure(greeting, err)
				return
			}
		case imapwire.ResponseUntagged:
			cond, handled, err := c.readUntagged(dec, greeting)
			if err != nil {
				c.readerFailure(greeting, err)
				return
			}
			if greeting {
				if !handled || (cond.Status != "OK" && cond.Status != "PREAUTH" && cond.Status != "BYE") {
					c.readerFailure(true, fmt.Errorf("invalid IMAP greeting"))
					return
				}
				if cond.Status == "BYE" {
					err := responseError("", cond)
					c.setGreeting(err)
					c.poison(err)
					return
				}
				c.mu.Lock()
				if cond.Status == "PREAUTH" {
					c.state = StateAuthenticated
				}
				c.mu.Unlock()
				c.setGreeting(nil)
				greeting = false
			}
		}
	}
}

func (c *Client) readerFailure(greeting bool, err error) {
	if err == nil {
		err = fmt.Errorf("unexpected end of IMAP response")
	}
	perr := protocolError(err)
	if greeting {
		c.setGreeting(perr)
	}
	c.poison(perr)
}

// readUntagged returns a non-nil condition only for an untagged status
// condition. All other untagged data is offered to command collectors before
// the connection-level handler receives the response types understood here.
func (c *Client) readUntagged(dec *imapwire.Decoder, greeting bool) (imapwire.RespCond, bool, error) {
	if !dec.ExpectSP() {
		return imapwire.RespCond{}, false, dec.Err()
	}
	var first string
	if !dec.ExpectAtom(&first) {
		return imapwire.RespCond{}, false, dec.Err()
	}
	upper := strings.ToUpper(first)
	if upper == "OK" || upper == "NO" || upper == "BAD" || upper == "PREAUTH" || upper == "BYE" {
		cond := imapwire.RespCond{Status: upper}
		if dec.SP() && !dec.ExpectRespText(&cond.Text) {
			return imapwire.RespCond{}, false, dec.Err()
		}
		if !dec.ExpectCRLF() {
			return imapwire.RespCond{}, false, dec.Err()
		}
		resp := untaggedResponse{name: upper, dec: dec, cond: &cond}
		if c.offerCollector(&resp) {
			return cond, true, nil
		}
		if cond.Text.Code == "CAPABILITY" {
			c.addCapabilities(strings.Fields(cond.Text.Args))
		}
		c.trace(TraceServer, "* "+upper)
		if upper == "BYE" && !greeting {
			// RFC 3501 section 7.1.5: the server closes the connection after an
			// untagged BYE. Every command except the LOGOUT that may have asked
			// for it is over, and so is the session — a client that keeps
			// waiting on this connection waits forever.
			err := responseError("", cond)
			c.completeAllExceptLogout(err)
			if !c.logoutPending() {
				c.poison(err)
			}
		}
		return cond, true, nil
	}

	resp := untaggedResponse{name: upper, dec: dec}
	if n, err := parseUint32(first); err == nil {
		resp.number, resp.hasNum = n, true
		if !dec.ExpectSP() || !dec.ExpectAtom(&resp.name) {
			return imapwire.RespCond{}, false, dec.Err()
		}
		resp.name = strings.ToUpper(resp.name)
	}
	if resp.name == "CAPABILITY" {
		if err := c.readCapabilities(dec); err != nil {
			return imapwire.RespCond{}, false, err
		}
		return imapwire.RespCond{}, true, nil
	}
	if c.offerCollector(&resp) {
		return imapwire.RespCond{}, true, nil
	}
	if err := c.handleUnilateral(&resp); err != nil {
		return imapwire.RespCond{}, false, err
	}
	return imapwire.RespCond{}, true, nil
}

func (c *Client) readCapabilities(dec *imapwire.Decoder) error {
	var capabilities []string
	for dec.SP() {
		var cap string
		if !dec.ExpectAtom(&cap) {
			return dec.Err()
		}
		capabilities = append(capabilities, strings.ToUpper(cap))
	}
	if !dec.ExpectCRLF() {
		return dec.Err()
	}
	c.setCapabilities(capabilities)
	c.trace(TraceServer, "* CAPABILITY")
	return nil
}

func (c *Client) addCapabilities(capabilities []string) {
	c.mu.Lock()
	for _, cap := range capabilities {
		c.caps[strings.ToUpper(cap)] = struct{}{}
	}
	c.mu.Unlock()
}

// setCapabilities installs the authoritative response from a CAPABILITY
// command. Unlike [CAPABILITY] response codes, this is a complete set and
// must replace stale entries rather than accumulating them indefinitely.
func (c *Client) setCapabilities(capabilities []string) {
	c.mu.Lock()
	c.caps = make(map[string]struct{}, len(capabilities))
	for _, capability := range capabilities {
		c.caps[strings.ToUpper(capability)] = struct{}{}
	}
	c.mu.Unlock()
}

func (c *Client) offerCollector(resp *untaggedResponse) bool {
	c.mu.Lock()
	commands := append([]*Command(nil), c.pendingQ...)
	c.mu.Unlock()
	for _, cmd := range commands {
		if cmd.collector == nil {
			continue
		}
		claimed, err := cmd.collector(resp)
		if err != nil {
			c.poison(protocolError(err))
			return true
		}
		if claimed {
			return true
		}
	}
	return false
}

func (c *Client) handleUnilateral(resp *untaggedResponse) error {
	h := c.opts.UnilateralData
	if resp.hasNum {
		if resp.name == "FETCH" {
			data, hasFlags, err := parseFlagsFetch(resp)
			if err != nil {
				return err
			}
			if hasFlags && h != nil && h.Fetch != nil {
				h.Fetch(data)
			}
			c.trace(TraceServer, "* FETCH")
			return nil
		}
		switch resp.name {
		case "EXISTS":
			if !resp.dec.ExpectCRLF() {
				return resp.dec.Err()
			}
			if h != nil && h.Exists != nil {
				h.Exists(resp.number)
			}
			c.trace(TraceServer, "* EXISTS")
			return nil
		case "EXPUNGE":
			if !resp.dec.ExpectCRLF() {
				return resp.dec.Err()
			}
			if h != nil && h.Expunge != nil {
				h.Expunge(resp.number)
			}
			c.trace(TraceServer, "* EXPUNGE")
			return nil
		case "RECENT":
			if !resp.dec.ExpectCRLF() {
				return resp.dec.Err()
			}
			if h != nil && h.Recent != nil {
				h.Recent(resp.number)
			}
			c.trace(TraceServer, "* RECENT")
			return nil
		}
	}
	// Unknown unsolicited responses are discarded without losing stream
	// alignment. Command-specific parsers get the first opportunity above.
	return resp.dec.DiscardLine()
}

// parseFlagsFetch handles the connection-scoped form of FETCH used for flag
// updates. T06's command collector claims full FETCH responses before they
// reach this function. Keeping this narrow parser here gives unsolicited flag
// changes a useful home without pre-empting the streaming FETCH implementation.
func parseFlagsFetch(resp *untaggedResponse) (*imap.FetchMessageData, bool, error) {
	if !resp.dec.ExpectSP() {
		return nil, false, resp.dec.Err()
	}
	data := &imap.FetchMessageData{SeqNum: imap.SeqNum(resp.number), Items: make(map[imap.FetchDataKey][]imap.FetchData)}
	hasFlags := false
	err := resp.dec.ExpectList(func() error {
		var key string
		if !resp.dec.ExpectAtom(&key) {
			return resp.dec.Err()
		}
		if !resp.dec.ExpectSP() {
			return resp.dec.Err()
		}
		if strings.EqualFold(key, "FLAGS") {
			var raw []string
			if err := resp.dec.ExpectFlagList(&raw); err != nil {
				return err
			}
			flags := make(imap.FetchDataFlags, len(raw))
			for i, flag := range raw {
				flags[i] = imap.Flag(flag)
			}
			data.Items[imap.FetchDataKey(key)] = append(data.Items[imap.FetchDataKey(key)], flags)
			hasFlags = true
			return nil
		}
		return resp.dec.DiscardValue()
	})
	if err != nil {
		return nil, false, err
	}
	if !resp.dec.ExpectCRLF() {
		return nil, false, resp.dec.Err()
	}
	return data, hasFlags, nil
}

func (c *Client) completeAllExceptLogout(err error) {
	c.mu.Lock()
	var complete []*Command
	kept := c.pendingQ[:0]
	for _, cmd := range c.pendingQ {
		if cmd.name == "LOGOUT" {
			kept = append(kept, cmd)
			continue
		}
		delete(c.pending, cmd.tag)
		complete = append(complete, cmd)
	}
	c.pendingQ = kept
	c.mu.Unlock()
	for _, cmd := range complete {
		cmd.complete(err)
	}
}

// logoutPending reports whether a LOGOUT is still awaiting its tagged
// completion, which is the one case where an untagged BYE is expected and the
// connection must stay readable until that completion arrives.
func (c *Client) logoutPending() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, cmd := range c.pendingQ {
		if cmd.name == "LOGOUT" {
			return true
		}
	}
	return false
}

func parseUint32(s string) (uint32, error) {
	var n uint64
	if s == "" {
		return 0, fmt.Errorf("empty number")
	}
	for _, b := range []byte(s) {
		if b < '0' || b > '9' {
			return 0, fmt.Errorf("not a number")
		}
		n = n*10 + uint64(b-'0')
		if n > 0xffffffff {
			return 0, fmt.Errorf("number overflows uint32")
		}
	}
	return uint32(n), nil
}
