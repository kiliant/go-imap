package imapserver

import (
	"bufio"
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
)

type appendSizeBackend struct {
	calls  chan AppendOptions
	reject bool
}

func (b *appendSizeBackend) Authenticate(context.Context, *ConnInfo, *Credentials, *AuthenticateOptions) (Session, error) {
	return &appendSizeSession{backend: b}, nil
}

type appendSizeSession struct {
	stubSession
	backend *appendSizeBackend
}

func (s *appendSizeSession) Append(_ context.Context, _ string, literal io.Reader, options *AppendOptions) (*imap.AppendData, error) {
	s.backend.calls <- *options
	if s.backend.reject {
		// A quota check can refuse the message without consuming the stream.
		return nil, &imap.Error{Type: imap.ErrorTypeNo, Code: imap.CodeOverQuota, Text: "quota exceeded"}
	}
	_, err := io.Copy(io.Discard, literal)
	return nil, err
}

func (*appendSizeSession) ResolveCatenateURL(context.Context, string, *CatenateOptions) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("url")), nil
}

func TestAppendPassesDeclaredSize(t *testing.T) {
	for _, tc := range []struct {
		name, payload string
		sizes         []int64
		known         bool
		reject        bool
	}{
		{name: "literal", payload: "{3+}\r\nabc", sizes: []int64{3}, known: true},
		{name: "multiappend", payload: "{3+}\r\nabc {5+}\r\n12345", sizes: []int64{3, 5}, known: true},
		{name: "catenate", payload: "CATENATE (TEXT {3+}\r\nabc URL \"imap://example.test/INBOX/;UID=1\")", sizes: []int64{0}},
		{name: "early-refusal", payload: "{3+}\r\nabc", sizes: []int64{3}, known: true, reject: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := &appendSizeBackend{calls: make(chan AppendOptions, 2), reject: tc.reject}
			server := New(backend, &Options{AllowInsecureAuth: true})
			serverSide, clientSide := net.Pipe()
			defer clientSide.Close()
			if err := clientSide.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			go func() { _ = server.ServeConn(ctx, serverSide, nil) }()
			reader := bufio.NewReader(clientSide)
			if _, err := reader.ReadString('\n'); err != nil {
				t.Fatal(err)
			}
			request := func(tag, command string) string {
				t.Helper()
				writeUpdateOrderCommand(t, clientSide, tag+" "+command+"\r\n")
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						t.Fatal(err)
					}
					if strings.HasPrefix(line, tag+" ") {
						return line
					}
				}
			}
			if line := request("L", "LOGIN alice secret"); !strings.HasPrefix(line, "L OK ") {
				t.Fatal(line)
			}
			line := request("A", "APPEND INBOX "+tc.payload)
			want := "A OK "
			if tc.reject {
				want = "A NO [OVERQUOTA]"
			}
			if !strings.HasPrefix(line, want) {
				t.Fatalf("APPEND = %q, want %q", line, want)
			}
			for _, size := range tc.sizes {
				select {
				case options := <-backend.calls:
					if options.Size != size || options.HasSize != tc.known {
						t.Errorf("size = %d (present %t), want %d (present %t)", options.Size, options.HasSize, size, tc.known)
					}
				case <-ctx.Done():
					t.Fatal("APPEND did not reach backend")
				}
			}
			if line := request("N", "NOOP"); !strings.HasPrefix(line, "N OK ") {
				t.Fatalf("APPEND left unread command bytes: %q", line)
			}
		})
	}
}
