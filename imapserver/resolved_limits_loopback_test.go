package imapserver_test

import (
	"context"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
	"github.com/kiliant/go-imap/imapserver/memory"
)

type resolvedLimitBackend struct {
	*memory.Backend
	size                 int
	known                bool
	bytes, calls, closes atomic.Int32
}

func (b *resolvedLimitBackend) Authenticate(ctx context.Context, info *imapserver.ConnInfo, credentials *imapserver.Credentials, options *imapserver.AuthenticateOptions) (imapserver.Session, error) {
	session, err := b.Backend.Authenticate(ctx, info, credentials, options)
	if err != nil {
		return nil, err
	}
	return resolvedLimitSession{Session: session, backend: b}, nil
}

type resolvedLimitSession struct {
	imapserver.Session
	backend *resolvedLimitBackend
}

func (s resolvedLimitSession) MultiAppend(ctx context.Context, mailbox string, messages []imapserver.AppendMessage, options *imapserver.MultiAppendOptions) (*imap.MultiAppendData, error) {
	return s.Session.(imapserver.MultiAppendSession).MultiAppend(ctx, mailbox, messages, options)
}

func (s resolvedLimitSession) ResolveCatenateURL(context.Context, string, *imapserver.CatenateOptions) (io.ReadCloser, error) {
	return s.backend.body(), nil
}
func (s resolvedLimitSession) GenerateURLAuth(context.Context, string, string, *imapserver.GenURLAuthOptions) (string, error) {
	return "url", nil
}
func (s resolvedLimitSession) ResetURLAuthKey(context.Context, string, *imapserver.ResetURLAuthKeyOptions) error {
	return nil
}
func (s resolvedLimitSession) FetchURLAuth(context.Context, string, *imapserver.URLFetchOptions) (*imapserver.URLFetchResult, error) {
	return &imapserver.URLFetchResult{Body: s.backend.body(), HasSize: s.backend.known, Size: int64(s.backend.size)}, nil
}

func (b *resolvedLimitBackend) body() io.ReadCloser {
	b.calls.Add(1)
	return &meteredResolvedBody{backend: b, remaining: b.size}
}

type meteredResolvedBody struct {
	backend   *resolvedLimitBackend
	remaining int
}

func (b *meteredResolvedBody) Read(p []byte) (int, error) {
	if b.remaining == 0 {
		return 0, io.EOF
	}
	if len(p) > b.remaining {
		p = p[:b.remaining]
	}
	for i := range p {
		p[i] = 'x'
	}
	b.remaining -= len(p)
	b.backend.bytes.Add(int32(len(p)))
	return len(p), nil
}
func (b *meteredResolvedBody) Close() error { b.backend.closes.Add(1); return nil }

func TestResolvedContentAggregateLimits(t *testing.T) {
	for _, test := range []struct {
		name, command string
		size          int
		known         bool
		bytes, calls  int32
	}{
		{name: "catenate-many-urls", command: "APPEND INBOX CATENATE (URL \"url\" URL \"url\" URL \"url\" URL \"url\" TEXT {1+}\r\nx)", size: 8, bytes: 17, calls: 3},
		{name: "catenate-text-sum", command: "APPEND INBOX CATENATE (TEXT {10+}\r\n0123456789 TEXT {10+}\r\n0123456789)", size: 8},
		{name: "append-sum", command: "APPEND INBOX {10+}\r\n0123456789 {10+}\r\n0123456789", size: 8},
		{name: "urlfetch-known", command: "URLFETCH \"url\"", size: 32, known: true, calls: 1},
		{name: "urlfetch-unknown", command: "URLFETCH \"url\" \"url\"", size: 32, bytes: 17, calls: 1},
		{name: "urlfetch-sum", command: "URLFETCH \"url\" \"url\" \"url\"", size: 8, known: true, bytes: 16, calls: 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			backend := &resolvedLimitBackend{Backend: memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}}), size: test.size, known: test.known}
			_, client, reader := newRawSessionOn(t, ctx, imapserver.New(backend, &imapserver.Options{AllowInsecureAuth: true, Limits: imapserver.Limits{MaxLiteralBytes: 16}}))
			writeRawCommand(t, client, "L "+test.command+"\r\n")
			if _, tagged := collectUntilTag(t, reader, "L "); !strings.HasPrefix(tagged, "L NO [TOOBIG]") {
				t.Fatalf("oversized resolved content = %q", tagged)
			}
			if backend.bytes.Load() != test.bytes || backend.calls.Load() != test.calls || backend.closes.Load() != test.calls {
				t.Fatalf("resolved bytes=%d calls=%d closes=%d, want bytes=%d calls=%d", backend.bytes.Load(), backend.calls.Load(), backend.closes.Load(), test.bytes, test.calls)
			}
			writeRawCommand(t, client, "S STATUS INBOX (MESSAGES)\r\n")
			lines, tagged := collectUntilTag(t, reader, "S ")
			if !strings.HasPrefix(tagged, "S OK") || len(lines) != 1 || !strings.Contains(lines[0], "MESSAGES 0") {
				t.Fatalf("oversize command changed mailbox or desynchronized input: %v %q", lines, tagged)
			}
		})
	}
}
