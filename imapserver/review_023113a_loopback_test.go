package imapserver_test

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kiliant/go-imap/imapclient"
	"github.com/kiliant/go-imap/imapserver"
	"github.com/kiliant/go-imap/imapserver/memory"
)

type review023TLSBackend struct {
	*memory.Backend
	mu     sync.Mutex
	states []*tls.ConnectionState
}

func (b *review023TLSBackend) Authenticate(ctx context.Context, info *imapserver.ConnInfo, credentials *imapserver.Credentials, opts *imapserver.AuthenticateOptions) (imapserver.Session, error) {
	b.mu.Lock()
	b.states = append(b.states, info.TLS)
	b.mu.Unlock()
	return b.Backend.Authenticate(ctx, info, credentials, opts)
}

func TestReview023TLSAfterCompressionReset(t *testing.T) {
	for _, implicit := range []bool{false, true} {
		for _, version := range []uint16{tls.VersionTLS12, tls.VersionTLS13} {
			t.Run(fmt.Sprintf("implicit=%v/version=%x", implicit, version), func(t *testing.T) {
				b := &review023TLSBackend{Backend: r4Memory()}
				config := serverTLSConfig(t, testCertificate(t))
				config.MinVersion, config.MaxVersion = version, version
				s := imapserver.New(b, &imapserver.Options{TLSConfig: config})
				ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
				defer cancel()
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				address := listener.Addr().String()
				if implicit {
					listener = tls.NewListener(listener, config)
				}
				go func() { _ = s.Serve(ctx, listener, nil) }()
				opts := &imapclient.Options{InsecureSkipVerify: true, TLSConfig: &tls.Config{MinVersion: version, MaxVersion: version}}
				var c *imapclient.Client
				if implicit {
					c, err = imapclient.DialTLS(ctx, address, opts)
				} else {
					c, err = imapclient.DialStartTLS(ctx, address, opts)
				}
				t.Cleanup(func() {
					if c != nil {
						_ = c.Close()
					}
					_ = listener.Close()
					_ = s.Close(context.Background(), nil)
				})
				if err != nil {
					t.Fatal(err)
				}
				if err := c.Login(ctx, "alice", "secret", nil); err != nil {
					t.Fatal(err)
				}
				for _, mechanism := range []string{"", "SCRAM-SHA-1-PLUS", "LOGIN", "SCRAM-SHA-256-PLUS"} {
					if err := c.Compress(ctx, nil); err != nil {
						t.Fatal(err)
					}
					if err := c.Unauthenticate(ctx, nil); err != nil {
						t.Fatal(err)
					}
					var err error
					if mechanism == "LOGIN" {
						err = c.Login(ctx, "alice", "secret", nil)
					} else if mechanism == "" {
						err = c.Authenticate(ctx, "alice", "secret", nil)
					} else {
						err = c.Authenticate(ctx, "alice", "secret", &imapclient.AuthenticateOptions{Mechanism: mechanism})
					}
					if err != nil {
						t.Fatalf("re-authenticate %q: %v", mechanism, err)
					}
				}
				b.mu.Lock()
				defer b.mu.Unlock()
				if len(b.states) != 5 {
					t.Fatalf("backend authentications = %d", len(b.states))
				}
				for _, state := range b.states {
					if state == nil || !state.HandshakeComplete || state.Version != version {
						t.Fatalf("lost backend TLS state: %#v", state)
					}
				}
			})
		}
	}
}

type review023URLBackend struct {
	*memory.Backend
	resolved atomic.Int32
	fetched  atomic.Int32
}

func (b *review023URLBackend) AllowCapability(_ context.Context, name string, _ *imapserver.CapabilityFilterOptions) bool {
	return name != "URL-PARTIAL"
}

func (b *review023URLBackend) Authenticate(ctx context.Context, info *imapserver.ConnInfo, credentials *imapserver.Credentials, opts *imapserver.AuthenticateOptions) (imapserver.Session, error) {
	s, err := b.Backend.Authenticate(ctx, info, credentials, opts)
	if err != nil {
		return nil, err
	}
	return &review023URLSession{Session: s, MultiAppendSession: s.(imapserver.MultiAppendSession), URLAuthSession: s.(imapserver.URLAuthSession), backend: b}, nil
}

type review023URLSession struct {
	imapserver.Session
	imapserver.MultiAppendSession
	imapserver.URLAuthSession
	backend *review023URLBackend
}

func (s *review023URLSession) ResolveCatenateURL(context.Context, string, *imapserver.CatenateOptions) (io.ReadCloser, error) {
	s.backend.resolved.Add(1)
	return io.NopCloser(strings.NewReader("Subject: resolved\r\n\r\ncontent\r\n")), nil
}

func (s *review023URLSession) FetchURLAuth(context.Context, string, *imapserver.URLFetchOptions) (*imapserver.URLFetchResult, error) {
	s.backend.fetched.Add(1)
	return &imapserver.URLFetchResult{Body: io.NopCloser(strings.NewReader("valid")), HasSize: true, Size: 5}, nil
}

func TestReview023CatenatePartialWithdrawal(t *testing.T) {
	b := &review023URLBackend{Backend: r4Memory()}
	w := r4Open(t, b, nil)
	for i, url := range []string{"/INBOX/;UID=1/;PARTIAL=0.4", "imap://alice@localhost/INBOX/;UID=1/;partial=0.4"} {
		tag := fmt.Sprintf("A%d", i)
		got := w.cmd(tag, fmt.Sprintf("APPEND INBOX CATENATE (URL %q TEXT {3+}\r\nabc URL %q) {3+}\r\nxyz", url, "/INBOX/;UID=2"))
		if !strings.HasPrefix(r4Tagged(got), tag+" NO [BADURL ") || !strings.Contains(r4Tagged(got), url) {
			t.Fatal(got)
		}
		if b.resolved.Load() != 0 {
			t.Fatal("withdrawn URL or later part reached backend")
		}
		if got := w.cmd("S", "STATUS INBOX (MESSAGES)"); !r4Has(got, "MESSAGES 0") {
			t.Fatalf("refused CATENATE committed: %q", got)
		}
	}
	if got := w.cmd("V", "APPEND INBOX CATENATE (URL \"/INBOX/;UID=1\")"); !strings.HasPrefix(r4Tagged(got), "V OK") || b.resolved.Load() != 1 {
		t.Fatalf("ordinary URL = %q, resolutions = %d", got, b.resolved.Load())
	}
}

func TestReview023URLFetchPartialWithdrawal(t *testing.T) {
	b := &review023URLBackend{Backend: r4Memory()}
	w := r4Open(t, b, nil)
	got := w.cmd("F", "URLFETCH \"imap://localhost/INBOX/;UID=1/;PARTIAL=0.4\" \"imap://localhost/INBOX/;UID=2\" \"imap://localhost/INBOX/;UID=3/;partial=0.4\"")
	if !strings.HasPrefix(r4Tagged(got), "F OK") || !r4Has(got, "^\\* URLFETCH .* NIL .*<5 octets>.* NIL$") || b.fetched.Load() != 1 {
		t.Fatalf("per-URL refusal = %q, backend calls = %d", got, b.fetched.Load())
	}
	w.cmd("N", "NOOP")
}

func TestReview023HandlerReadTimeouts(t *testing.T) {
	for _, command := range []string{"A IDLE\r\n", "A APPEND INBOX {20}\r\n", "A APPEND INBOX CATENATE (TEXT {20}\r\n"} {
		t.Run(command, func(t *testing.T) {
			w := r4Open(t, r4Memory(), &imapserver.Options{AllowInsecureAuth: true, Limits: imapserver.Limits{ReadTimeout: 150 * time.Millisecond, PreAuthTimeout: 2 * time.Second}})
			w.write(command)
			if line, err := w.line(); err != nil || !strings.HasPrefix(line, "+") {
				t.Fatalf("continuation = %q, %v", line, err)
			}
			if !strings.Contains(command, "IDLE") {
				w.write("short")
			}
			if line, err := w.line(); err != nil || !strings.HasPrefix(line, "* BYE ") {
				t.Fatalf("handler read timeout = %q, %v", line, err)
			}
		})
	}
}
