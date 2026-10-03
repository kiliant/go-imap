package imapclient

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/kiliant/go-imap"
)

// FuzzUnilateralStream feeds arbitrary server output, after a valid greeting,
// to a client with every unilateral handler installed and no command running.
// Everything it reaches is reachable by a hostile server at any moment, which
// since T27 includes the STATUS and LIST parsers and status-response
// delivery. The client may reject the stream; it must not panic or hang.
func FuzzUnilateralStream(f *testing.F) {
	for _, seed := range []string{
		"* OK [ALERT] System shutdown in 10 minutes\r\n",
		"* OK [INPROGRESS (\"A1\" 5 10)] Searching\r\n",
		"* NO [X-FUTURE a b] c\r\n",
		"* STATUS Archive (MESSAGES 12 UIDNEXT 40)\r\n",
		"* STATUS \"x\" (MAILBOXID (F1) SIZE 9)\r\n",
		"* LIST (\\NonExistent) \"/\" Gone\r\n",
		"* LIST () NIL INBOX (\"OLDNAME\" (\"Old\"))\r\n",
		"* 3 EXISTS\r\n* 1 EXPUNGE\r\n* 2 FETCH (FLAGS (\\Seen))\r\n",
		"* VANISHED (EARLIER) 1:3\r\n",
		"* STATUS\r\n* LIST\r\n* OK [\r\n",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, stream string) {
		clientConn, serverConn := net.Pipe()
		defer serverConn.Close()
		go func() {
			_, _ = serverConn.Write([]byte("* OK ready\r\n" + stream))
			// Close so a stream that ends mid-response is an EOF, not a hang.
			_ = serverConn.Close()
		}()
		c := NewClient(clientConn, &Options{UnilateralData: &UnilateralDataHandler{
			Exists:         func(uint32) {},
			Expunge:        func(uint32) {},
			Recent:         func(uint32) {},
			Fetch:          func(*imap.FetchMessageData) {},
			Vanished:       func(VanishedData) {},
			StatusResponse: func(*StatusResponse) {},
			MailboxStatus:  func(*StatusData) {},
			List:           func(*ListData) {},
		}})
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = c.WaitGreeting(ctx, nil)
		// The server side closes after writing, so the reader must finish:
		// either it rejected the stream or it reached EOF. Waiting for it is
		// what turns a parser that loops into a fuzz failure.
		select {
		case <-c.readerDone:
		case <-ctx.Done():
			t.Fatal("reader did not finish on a closed connection")
		}
		_ = c.Close()
	})
}
