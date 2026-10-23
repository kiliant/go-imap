package imapserver_test

import (
	"bufio"
	"context"
	"encoding/base64"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap/imapserver"
)

// saslOpen connects to a memory server without logging in.
func saslOpen(t *testing.T) *r4Wire {
	t.Helper()
	c, peer := net.Pipe()
	s := imapserver.New(r4Memory(), &imapserver.Options{AllowInsecureAuth: true})
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(func() { cancel(); _ = c.Close(); _ = s.Close(context.Background(), nil) })
	go func() { _ = s.ServeConn(ctx, peer, nil) }()
	w := &r4Wire{t: t, c: c, r: bufio.NewReader(c)}
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := w.line(); err != nil {
		t.Fatal(err)
	}
	return w
}

// saslChallenge reads one line, which must be a continuation request.
func saslChallenge(t *testing.T, w *r4Wire) string {
	t.Helper()
	_ = w.c.SetReadDeadline(time.Now().Add(10 * time.Second))
	line, err := w.line()
	if err != nil {
		t.Fatalf("reading challenge: %v", err)
	}
	if !strings.HasPrefix(line, "+") {
		t.Fatalf("challenge = %q, want a continuation request", line)
	}
	return line
}

// An empty server challenge is empty base64 after the continuation's SP:
// continue-req = "+" SP (resp-text / base64) CRLF (RFC 3501 section 6.2.2).
// "=" is RFC 4959's spelling of an empty client initial response and must not
// appear in a challenge.
func TestSASLEmptyChallengeIsEmptyBase64(t *testing.T) {
	plain := base64.StdEncoding.EncodeToString([]byte("\x00alice\x00secret"))
	t.Run("PLAIN", func(t *testing.T) {
		w := saslOpen(t)
		w.write("A AUTHENTICATE PLAIN\r\n")
		if got := saslChallenge(t, w); got != "+ " {
			t.Fatalf("PLAIN challenge = %q, want %q", got, "+ ")
		}
		w.write(plain + "\r\n")
		if got := r4Tagged(w.until("A")); !strings.HasPrefix(got, "A OK") {
			t.Fatalf("PLAIN after an empty challenge = %q", got)
		}
	})
	t.Run("SCRAM-SHA-256", func(t *testing.T) {
		w := saslOpen(t)
		w.write("A AUTHENTICATE SCRAM-SHA-256\r\n")
		if got := saslChallenge(t, w); got != "+ " {
			t.Fatalf("SCRAM challenge = %q, want %q", got, "+ ")
		}
		w.write("*\r\n")
		if got := r4Tagged(w.until("A")); !strings.HasPrefix(got, "A BAD") {
			t.Fatalf("cancelled SCRAM = %q", got)
		}
	})
}

// A nonempty challenge is unchanged: the base64 text follows the SP.
func TestSASLNonemptyChallengesAreBase64(t *testing.T) {
	w := saslOpen(t)
	w.write("A AUTHENTICATE LOGIN\r\n")
	for _, prompt := range []string{"Username:", "Password:"} {
		want := "+ " + base64.StdEncoding.EncodeToString([]byte(prompt))
		if got := saslChallenge(t, w); got != want {
			t.Fatalf("LOGIN challenge = %q, want %q", got, want)
		}
		answer := "alice"
		if prompt == "Password:" {
			answer = "secret"
		}
		w.write(base64.StdEncoding.EncodeToString([]byte(answer)) + "\r\n")
	}
	if got := r4Tagged(w.until("A")); !strings.HasPrefix(got, "A OK") {
		t.Fatalf("LOGIN = %q", got)
	}
}

// SASL-IR keeps its own grammar: a base64 initial response needs no
// continuation, and "=" is accepted as an empty one rather than refused as
// malformed base64. An empty SCRAM client-first message fails authentication
// with NO, where malformed base64 would have been BAD.
func TestSASLInitialResponseUnchanged(t *testing.T) {
	plain := base64.StdEncoding.EncodeToString([]byte("\x00alice\x00secret"))
	w := saslOpen(t)
	if got := r4Tagged(w.cmd("A", "AUTHENTICATE PLAIN "+plain)); !strings.HasPrefix(got, "A OK") {
		t.Fatalf("PLAIN with an initial response = %q", got)
	}

	w = saslOpen(t)
	got := w.cmd("B", "AUTHENTICATE SCRAM-SHA-256 =")
	if len(got) != 1 || !strings.HasPrefix(got[0], "B NO [AUTHENTICATIONFAILED]") {
		t.Fatalf("SCRAM with an empty initial response = %q", got)
	}
}
