package imapserver_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap/imapserver"
)

// TestCatenateUnrepresentableURLRefusal pins #204. A refused CATENATE URL is
// echoed in NO [BADURL <url>], but url-resp-text cannot carry "]", CR or LF
// (RFC 4469 section 5). Such a URL is refused with a plain NO instead. It
// used to become NO [SERVERBUG] and reach OnError as a backend failure carrying
// the client's text. Either way the refusal must commit nothing and keep the
// connection synchronized, for APPEND and REPLACE alike.
func TestCatenateUnrepresentableURLRefusal(t *testing.T) {
	events := make(chan *imapserver.ServerEvent, 16)
	opts := &imapserver.Options{
		AllowInsecureAuth: true,
		// The read timeout ends the connection with a terminal event. Events
		// are delivered in order, so once it arrives, any diagnostic a refusal
		// had queued has been delivered too.
		Limits:  imapserver.Limits{ReadTimeout: time.Second},
		OnError: func(_ context.Context, e *imapserver.ServerEvent) { events <- e },
	}
	b := r4Memory()
	review6fFill(t, b, 1)
	w := r4Open(t, b, opts)
	sweepWant(t, w.cmd("C", "CREATE Out"), "OK")
	sweepWant(t, w.cmd("S", "SELECT INBOX"), "OK")
	status := func() string {
		got := w.cmd("T", "STATUS Out (MESSAGES UIDNEXT)")
		sweepWant(t, got, "OK")
		return strings.Join(got[:len(got)-1], "\n")
	}
	before := status()

	for _, command := range []string{"APPEND Out", "REPLACE 1 Out"} {
		for _, tc := range []struct {
			name, url string
			badURL    bool
		}{
			{name: "ordinary", url: `"/INBOX/;UID=99999"`, badURL: true},
			{name: "bracket", url: `"/INBOX]/;UID=1"`},
			{name: "crlf", url: sweepLiteral("/INBOX/;UID=1\r\nX", false)},
			{name: "lf", url: sweepLiteral("/INBOX/;UID=1\nX", false)},
		} {
			// A later TEXT part must still be drained after the refusal.
			got := w.cmd("A", fmt.Sprintf("%s CATENATE (URL %s TEXT %s)", command, tc.url, sweepLiteral("tail\r\n", false)))
			tagged := r4Tagged(got)
			switch {
			case tc.badURL && !strings.HasPrefix(tagged, `A NO [BADURL /INBOX/;UID=99999] `):
				t.Errorf("%s %s = %q, want NO [BADURL <url>]", command, tc.name, tagged)
			case !tc.badURL && (!strings.HasPrefix(tagged, "A NO ") || strings.HasPrefix(tagged, "A NO [")):
				t.Errorf("%s %s = %q, want a NO without a response code", command, tc.name, tagged)
			}
			if after := status(); after != before {
				t.Errorf("%s %s committed a message: %q, before %q", command, tc.name, after, before)
			}
			for _, line := range w.cmd("N", "NOOP") {
				if strings.Contains(line, " BAD ") || !strings.HasPrefix(line, "* ") && !strings.HasPrefix(line, "N OK") {
					t.Errorf("%s %s left the connection unsynchronized: %q", command, tc.name, line)
				}
			}
		}
	}
	got := w.cmd("F", "FETCH 1 (UID)")
	sweepWant(t, got, "OK")
	if !strings.HasPrefix(got[0], "* 1 FETCH ") {
		t.Errorf("a refused REPLACE removed the replaced message: %q", got)
	}

	_ = w.c.SetReadDeadline(time.Now().Add(10 * time.Second))
	if line, err := w.line(); err != nil || !strings.HasPrefix(line, "* BYE ") {
		t.Fatalf("read timeout = %q, %v; want BYE", line, err)
	}
	for {
		select {
		case e := <-events:
			if e.Kind == imapserver.ServerEventTimeout {
				return
			}
			t.Errorf("refused CATENATE URL reported %s: %v", e.Kind, e.Err)
		case <-time.After(10 * time.Second):
			t.Fatal("missing terminal timeout event")
		}
	}
}
