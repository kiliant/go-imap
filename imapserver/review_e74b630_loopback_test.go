package imapserver_test

import (
	"bufio"
	"bytes"
	"compress/flate"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
	"github.com/kiliant/go-imap/imapserver/memory"
)

func TestReviewE74AuthenticationPipelines(t *testing.T) {
	for _, auth := range []string{"LOGIN alice secret", "AUTHENTICATE PLAIN " + base64.StdEncoding.EncodeToString([]byte("\x00alice\x00secret")), "AUTHENTICATE PLAIN"} {
		t.Run(auth, func(t *testing.T) {
			w := r4Open(t, r4Memory(), nil)
			w.cmd("U", "UNAUTHENTICATE")
			if auth == "AUTHENTICATE PLAIN" {
				w.write("A " + auth + "\r\n")
				if line, err := w.line(); err != nil || !strings.HasPrefix(line, "+") {
					t.Fatalf("challenge = %q, %v", line, err)
				}
				w.write(base64.StdEncoding.EncodeToString([]byte("\x00alice\x00secret")) + "\r\nB NOOP\r\nC CAPABILITY\r\n")
			} else {
				w.write("A " + auth + "\r\nB NOOP\r\nC CAPABILITY\r\n")
			}
			for _, tag := range []string{"A", "B", "C"} {
				if got := w.until(tag); !strings.HasPrefix(r4Tagged(got), tag+" OK") {
					t.Fatalf("pipeline = %q", got)
				}
			}
			w.write("D UNAUTHENTICATE\r\nE LOGIN alice secret\r\nF NOOP\r\n")
			for _, tag := range []string{"D", "E", "F"} {
				if got := w.until(tag); !strings.HasPrefix(r4Tagged(got), tag+" OK") {
					t.Fatalf("reauthentication pipeline = %q", got)
				}
			}
		})
	}
}

func TestReviewE74UnauthenticateCompression(t *testing.T) {
	for _, pipeline := range []bool{false, true} {
		t.Run(fmt.Sprint(pipeline), func(t *testing.T) {
			w := r4Open(t, r4Memory(), nil)
			w.cmd("Z", "COMPRESS DEFLATE")
			var compressed bytes.Buffer
			writer, _ := flate.NewWriter(&compressed, flate.DefaultCompression)
			_, _ = io.WriteString(writer, "U UNAUTHENTICATE\r\n")
			_ = writer.Flush()
			input := append([]byte(nil), compressed.Bytes()...)
			if pipeline {
				input = append(input, "A LOGIN alice secret\r\nB CAPABILITY\r\n"...)
			}
			w.write(string(input))
			inflater := flate.NewReader(w.r)
			reader := bufio.NewReader(inflater)
			line, err := reader.ReadString('\n')
			if err != nil || !strings.HasPrefix(line, "U OK ") {
				t.Fatalf("compressed completion = %q, %v", line, err)
			}
			_ = inflater.Close()
			if !pipeline {
				w.write("A LOGIN alice secret\r\nB CAPABILITY\r\n")
			}
			w.until("A")
			caps := w.until("B")
			if !r4Has(caps, `COMPRESS=DEFLATE`) {
				t.Fatalf("compression not reset: %q", caps)
			}
		})
	}
}

func TestReviewE74CompressionDictionaryBoundary(t *testing.T) {
	w := r4Open(t, r4Memory(), nil)
	w.cmd("Z", "COMPRESS DEFLATE")
	const unauthenticate = "U UNAUTHENTICATE\r\n"
	const noop = "P NOOP\r\n"
	n := (32768 - len(unauthenticate)) / len(noop)
	padding := (32768 - len(unauthenticate)) % len(noop)
	plain := strings.Repeat(noop, n-1) + strings.Repeat("P", padding+1) + " NOOP\r\n" + unauthenticate
	if len(plain) != 32768 {
		t.Fatal(len(plain))
	}
	var input bytes.Buffer
	deflater, _ := flate.NewWriter(&input, flate.DefaultCompression)
	_, _ = io.WriteString(deflater, plain)
	_ = deflater.Flush()
	input.WriteString("A LOGIN alice secret\r\nB NOOP\r\n")
	w.write(input.String())
	inflater := flate.NewReader(w.r)
	reader := bufio.NewReader(inflater)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(line, "U OK ") {
			break
		}
	}
	_ = inflater.Close()
	if got := w.until("A"); !strings.HasPrefix(r4Tagged(got), "A OK") {
		t.Fatal(got)
	}
	w.until("B")
}

type e74FilteredBackend struct {
	*memory.Backend
	deny string
}

func (b *e74FilteredBackend) AllowCapability(_ context.Context, stringName string, _ *imapserver.CapabilityFilterOptions) bool {
	return stringName != b.deny
}

func TestReviewE74FilteredMove(t *testing.T) {
	for _, command := range []string{"MOVE", "UID MOVE"} {
		t.Run(command, func(t *testing.T) {
			w := r4Open(t, &e74FilteredBackend{r4Memory(), "MOVE"}, nil)
			r4Append(w, "INBOX", "", "one")
			w.cmd("C", "CREATE destination")
			w.cmd("S", "SELECT INBOX")
			if r4Has(w.cmd("P", "CAPABILITY"), `\bMOVE\b`) {
				t.Fatal("filtered MOVE advertised")
			}
			if got := w.cmd("M", command+" 1 destination"); !strings.HasPrefix(r4Tagged(got), "M BAD") {
				t.Fatalf("filtered MOVE = %q", got)
			}
			if got := w.cmd("T", "STATUS destination (MESSAGES)"); !r4Has(got, `MESSAGES 0`) {
				t.Fatalf("filtered MOVE mutated destination: %q", got)
			}
		})
	}
}

func TestReviewE74FilteredExtensionCommands(t *testing.T) {
	for _, test := range []struct{ capability, command string }{
		{"UIDPLUS", "UID EXPUNGE 1"},
		{"NAMESPACE", "NAMESPACE"}, {"UNAUTHENTICATE", "UNAUTHENTICATE"},
		{"ACL", "GETACL INBOX"}, {"ACL", "MYRIGHTS INBOX"},
		{"ACL", "LISTRIGHTS INBOX alice"}, {"ACL", "SETACL INBOX alice lr"}, {"ACL", "DELETEACL INBOX alice"},
		{"QUOTA", "GETQUOTA root"}, {"QUOTA", "GETQUOTAROOT INBOX"}, {"QUOTA", "SETQUOTA root (STORAGE 10)"},
		{"METADATA", "GETMETADATA INBOX /private/x"}, {"METADATA", `SETMETADATA INBOX (/private/x "value")`},
		{"SORT", "SORT (SUBJECT) UTF-8 ALL"}, {"THREAD=REFERENCES", "THREAD REFERENCES UTF-8 ALL"},
		{"MULTISEARCH", "ESEARCH IN (mailboxes INBOX) ALL"},
		{"NOTIFY", "NOTIFY NONE"}, {"LANGUAGE", "LANGUAGE"},
		{"URLAUTH", `URLFETCH "imap://localhost/INBOX/;UID=1"`},
		{"URLAUTH", `GENURLAUTH "imap://localhost/INBOX/;UID=1;URLAUTH=user+alice" INTERNAL`},
		{"URLAUTH", "RESETKEY INBOX"},
		{"CONDSTORE", "FETCH 1 (MODSEQ)"}, {"CONDSTORE", `STORE 1 (UNCHANGEDSINCE 1) +FLAGS (\Seen)`},
		{"SPECIAL-USE", `LIST "" "*" RETURN (SPECIAL-USE)`},
		{"BINARY", "APPEND INBOX ~{3+}\r\nx\r\n"},
		{"MULTIAPPEND", "APPEND INBOX {3+}\r\nx\r\n {3+}\r\nx\r\n"},
		{"REPLACE", "REPLACE 1 INBOX {3+}\r\nx\r\n"},
	} {
		t.Run(test.command, func(t *testing.T) {
			w := r4Open(t, &e74FilteredBackend{r4Memory(), test.capability}, nil)
			r4Append(w, "INBOX", "", "one")
			w.cmd("S", "SELECT INBOX")
			for _, line := range w.cmd("P", "CAPABILITY") {
				if strings.Contains(" "+line+" ", " "+test.capability+" ") {
					t.Fatalf("withdrawn capability advertised: %q", line)
				}
			}
			got := w.cmd("X", test.command)
			gate := test.capability
			if test.command == "FETCH 1 (MODSEQ)" {
				gate = "MODSEQ"
			}
			if test.capability == "BINARY" {
				gate = "invalid command syntax"
			}
			if strings.HasPrefix(r4Tagged(got), "X OK") || !strings.Contains(r4Tagged(got), gate) {
				t.Fatalf("withdrawn %s was not refused by its gate: %q", test.capability, got)
			}
			w.cmd("N", "NOOP")
		})
	}
}

type e74URLSession struct {
	imapserver.Session
	imapserver.URLAuthSession
	err error
}

func (s *e74URLSession) FetchURLAuth(context.Context, string, *imapserver.URLFetchOptions) (*imapserver.URLFetchResult, error) {
	return nil, s.err
}
func TestReviewE74URLFetchErrors(t *testing.T) {
	for _, err := range []error{errors.New("private storage failure"), context.Canceled, context.DeadlineExceeded} {
		t.Run(err.Error(), func(t *testing.T) {
			backend := &r4WrapBackend{inner: r4Memory(), wrap: func(s imapserver.Session) imapserver.Session {
				return &e74URLSession{s, s.(imapserver.URLAuthSession), err}
			}}
			w := r4Open(t, backend, nil)
			got := w.cmd("F", `URLFETCH "imap://localhost/INBOX/;UID=1" "imap://localhost/INBOX/;UID=2"`)
			if !strings.HasPrefix(r4Tagged(got), "F NO [") || !r4Has(got, `^\* URLFETCH .* NIL .* NIL$`) || strings.Contains(strings.Join(got, " "), err.Error()) {
				t.Fatalf("URLFETCH error/framing = %q", got)
			}
			w.cmd("N", "NOOP")
		})
	}
}

func TestReviewE74LargeAtomicStore(t *testing.T) {
	for _, kind := range []string{"memory", "maildir"} {
		t.Run(kind, func(t *testing.T) {
			backend := r4Backend(t, kind)
			n, budget := 20000, int64(1<<20)
			if kind == "maildir" {
				n, budget = 12, 256
			}
			session, err := backend.Authenticate(t.Context(), &imapserver.ConnInfo{}, &imapserver.Credentials{Mechanism: "PLAIN", Username: "alice", Password: "secret"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < n; i++ {
				if _, err := session.Append(t.Context(), "INBOX", strings.NewReader("Subject: small\r\n\r\nbody\r\n"), nil); err != nil {
					t.Fatal(err)
				}
			}
			_ = session.Close(t.Context(), nil)
			opts := &imapserver.Options{AllowInsecureAuth: true, Limits: imapserver.Limits{MaxQueuedUpdateBytes: budget}}
			mutator, observer := r4Open(t, backend, opts), r4Open(t, backend, opts)
			mutator.cmd("S", "SELECT INBOX")
			observer.cmd("S", "EXAMINE INBOX")
			got := mutator.cmd("F", `STORE 1:* +FLAGS.SILENT (\Seen)`)
			if !strings.HasPrefix(r4Tagged(got), "F OK") {
				t.Fatalf("STORE = %q", got)
			}
			mutator.cmd("N", "NOOP")
			updates := observer.cmd("N", "NOOP")
			count := 0
			for _, line := range updates {
				if strings.Contains(line, "FETCH") && strings.Contains(line, `\Seen`) {
					count++
				}
			}
			if count != n {
				t.Fatalf("observer received %d FLAGS updates, want %d", count, n)
			}
			observer.cmd("P", "NOOP")
		})
	}
}

func TestReviewE74StoreResponseLimit(t *testing.T) {
	w := r4Open(t, r4Memory(), &imapserver.Options{AllowInsecureAuth: true, Limits: imapserver.Limits{MaxStoreResponseBytes: 1100}})
	r4Append(w, "INBOX", "", "one")
	r4Append(w, "INBOX", "", "two")
	w.cmd("S", "SELECT INBOX")
	if got := w.cmd("F", `STORE 1:* +FLAGS (\Seen)`); !strings.HasPrefix(r4Tagged(got), "F NO [LIMIT]") {
		t.Fatalf("STORE buffer limit = %q", got)
	}
	if got := w.cmd("N", "NOOP"); !strings.HasPrefix(r4Tagged(got), "N OK") {
		t.Fatalf("connection after STORE limit = %q", got)
	}
}

type e74SessionBackend struct {
	*memory.Backend
	emptyName     bool
	failFetch     error
	closeDeadline chan time.Duration
}
type e74Session struct {
	imapserver.Session
	imapserver.CapabilitySupport
	owner *e74SessionBackend
}
type e74Selected struct {
	imapserver.SelectedMailbox
	imapserver.CondStoreMailbox
	err error
}

func (b *e74SessionBackend) Authenticate(ctx context.Context, info *imapserver.ConnInfo, cred *imapserver.Credentials, options *imapserver.AuthenticateOptions) (imapserver.Session, error) {
	s, err := b.Backend.Authenticate(ctx, info, cred, options)
	if err != nil {
		return nil, err
	}
	return &e74Session{Session: s, CapabilitySupport: b.Backend, owner: b}, nil
}
func (s *e74Session) Select(ctx context.Context, name string, updater *imapserver.Updater, options *imapserver.SelectOptions) (*imapserver.SelectResult, error) {
	r, err := s.Session.Select(ctx, name, updater, options)
	if err == nil {
		if s.owner.emptyName {
			r.Snapshot.Status.Mailbox = ""
		}
		r.Mailbox = &e74Selected{r.Mailbox, r.Mailbox.(imapserver.CondStoreMailbox), s.owner.failFetch}
	}
	return r, err
}
func (m *e74Selected) Fetch(ctx context.Context, writer *imapserver.FetchWriter, uids imap.UIDSet, options *imapserver.FetchOptions) error {
	if m.err != nil {
		return m.err
	}
	return m.SelectedMailbox.Fetch(ctx, writer, uids, options)
}
func (s *e74Session) Unauthenticate(ctx context.Context, options *imapserver.UnauthenticateOptions) error {
	if s.owner.closeDeadline != nil {
		timer := time.NewTimer(150 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.Session.(imapserver.UnauthenticateSession).Unauthenticate(ctx, options)
}
func (s *e74Session) Close(ctx context.Context, options *imapserver.SessionCloseOptions) error {
	if s.owner.closeDeadline != nil {
		deadline, _ := ctx.Deadline()
		s.owner.closeDeadline <- time.Until(deadline)
	}
	return s.Session.Close(ctx, options)
}

func TestReviewE74EmptySnapshotName(t *testing.T) {
	w := r4Open(t, &e74SessionBackend{Backend: r4Memory(), emptyName: true}, nil)
	r4Append(w, "INBOX", "", "one")
	w.cmd("S", "SELECT INBOX")
	if got := w.cmd("F", "FETCH 1 (MODSEQ)"); !strings.HasPrefix(r4Tagged(got), "F OK") {
		t.Fatalf("empty snapshot name = %q", got)
	}
}

func TestReviewE74UnauthenticateCleanupContext(t *testing.T) {
	deadline := make(chan time.Duration, 2)
	w := r4Open(t, &e74SessionBackend{Backend: r4Memory(), closeDeadline: deadline}, &imapserver.Options{AllowInsecureAuth: true, Limits: imapserver.Limits{CommandTimeout: 250 * time.Millisecond}})
	w.cmd("U", "UNAUTHENTICATE")
	if remaining := <-deadline; remaining < 200*time.Millisecond {
		t.Fatalf("cleanup inherited expiring deadline: %v", remaining)
	}
}

func TestReviewE74CloseOmitsForeignRemovals(t *testing.T) {
	for _, kind := range []string{"memory", "maildir"} {
		for _, qresync := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%v", kind, qresync), func(t *testing.T) {
				b := r4Backend(t, kind)
				a, other := r4Open(t, b, nil), r4Open(t, b, nil)
				r4Append(a, "INBOX", "", "one")
				r4Append(a, "INBOX", "", "two")
				if qresync {
					a.cmd("E", "ENABLE QRESYNC")
				}
				a.cmd("S", "SELECT INBOX")
				other.cmd("S", "SELECT INBOX")
				other.cmd("F", `STORE 1:* +FLAGS.SILENT (\Deleted)`)
				other.cmd("X", "UID EXPUNGE 1")
				got := a.cmd("C", "CLOSE")
				if r4Has(got, `EXPUNGE|VANISHED`) {
					t.Fatalf("CLOSE announced removals: %q", got)
				}
			})
		}
	}
}

func TestReviewE74ChangedSinceZero(t *testing.T) {
	w := r4Open(t, r4Memory(), nil)
	w.cmd("S", "SELECT INBOX")
	if got := w.cmd("F", "FETCH 1:* (UID) (CHANGEDSINCE 0)"); !strings.HasPrefix(r4Tagged(got), "F BAD") {
		t.Fatalf("CHANGEDSINCE 0 = %q", got)
	}
	w.cmd("N", "NOOP")
}

func TestReviewE74UpdateFallbackFailureSendsBye(t *testing.T) {
	errFetch := errors.New("private update fetch failure")
	b := r4Memory()
	owner := &e74SessionBackend{Backend: b, failFetch: errFetch}
	// Strip update clocks using the existing public-contract adapter, then
	// fail the framework's drain-time FETCH through a second session wrapper.
	outer := &r4WrapBackend{inner: &independentNoUpdateModSeqBackend{b}, wrap: func(s imapserver.Session) imapserver.Session {
		return &e74Session{Session: s, CapabilitySupport: b, owner: owner}
	}}
	events := make(chan *imapserver.ServerEvent, 8)
	observer := r4Open(t, outer, &imapserver.Options{AllowInsecureAuth: true, OnError: func(_ context.Context, e *imapserver.ServerEvent) { events <- e }})
	other := r4Open(t, b, nil)
	r4Append(other, "INBOX", "", "one")
	observer.cmd("E", "ENABLE CONDSTORE")
	observer.cmd("S", "SELECT INBOX")
	other.cmd("S", "SELECT INBOX")
	other.cmd("F", `STORE 1 +FLAGS (\Seen)`)
	observer.write("N NOOP\r\n")
	if line, err := observer.line(); err != nil || !strings.HasPrefix(line, "* BYE ") || strings.Contains(line, errFetch.Error()) {
		t.Fatalf("failed fallback = %q, %v", line, err)
	}
	select {
	case e := <-events:
		if !errors.Is(e.Err, errFetch) {
			t.Fatalf("diagnostic = %#v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("missing fallback diagnostic")
	}
}
