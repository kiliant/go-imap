package imapclient

import (
	"context"
	"net"
	"reflect"
	"testing"
	"time"

	"github.com/kiliant/go-imap/internal/imapwire"
)

func FuzzDecodeContextUpdate(f *testing.F) {
	for _, wire := range append(append([]string(nil), hostileSeeds...),
		` (TAG "old") UID ADDTO (1 9,4:2 1 7) REMOVEFROM (0 1:4294967295) ADDTO (0 5)`+"\r\n",
		` (TAG "old") X-FUTURE ("a" NIL)`+"\r\n") {
		f.Add(wire)
	}
	f.Fuzz(func(t *testing.T, wire string) {
		parsed, err := readESearchResponse(imapwire.NewDecoderString(wire, nil))
		if err != nil {
			return
		}
		first, err := decodeContextUpdate(parsed, parsed.result.Data.UID)
		if err != nil {
			return
		}
		second, err := decodeContextUpdate(parsed, parsed.result.Data.UID)
		if err != nil || !reflect.DeepEqual(first, second) || len(first.Items) != len(parsed.items) {
			t.Fatal("decoding lost items or mutated cached data")
		}
	})
}

// FuzzContextUpdateStream feeds arbitrary server output to a client that holds
// live registrations: A1 for sequence numbers, A2 for UIDs and A3 for a UID
// SORT. A hostile server can send any of it at any moment. The client may
// reject the stream; it must not panic or hang, and what it delivers must
// belong to a registration and match that registration's number mode.
func FuzzContextUpdateStream(f *testing.F) {
	for _, seed := range []string{
		"* ESEARCH (TAG \"A1\") ADDTO (0 9,4:2 1 7) REMOVEFROM (2 8)\r\n",
		"* ESEARCH (TAG \"A2\") UID ADDTO (1 5) ADDTO (1 6)\r\n",
		"* ESEARCH (TAG \"A2\") ADDTO (0 5)\r\n",
		"* ESEARCH (TAG \"A1\") UID ADDTO (0 5)\r\n",
		"* ESEARCH (TAG \"A3\") UID ADDTO (0 5)\r\n",
		"* ESEARCH (TAG \"A3\") UID REMOVEFROM (2 7:5)\r\n",
		"* ESEARCH (TAG \"A2\" MAILBOX \"INBOX\" UIDVALIDITY 1) UID REMOVEFROM (0 5)\r\n",
		"* ESEARCH (TAG \"A2\" MAILBOX \"INBOX\") UID REMOVEFROM (0 5)\r\n",
		"* NO [NOUPDATE \"A2\"] full\r\n* ESEARCH (TAG \"A2\") UID ADDTO (1 5)\r\n",
		"* NO [NOUPDATE] x\r\n* OK [NOUPDATE (\"A1\")] y\r\n",
		"* ESEARCH (TAG \"zz\") REMOVEFROM (0 1)\r\n",
		"* ESEARCH (TAG \"A1\") X-FUTURE (1 2)\r\n",
		"* ESEARCH ADDTO (0 1)\r\n",
		"* ESEARCH (TAG \"A1\" TAG \"A1\") ADDTO (0 1)\r\n",
	} {
		f.Add(seed)
	}
	registered := map[string]searchContext{
		"A1": {tag: "A1"},
		"A2": {uid: true, tag: "A2"},
		"A3": {uid: true, sorted: true, tag: "A3"},
	}
	f.Fuzz(func(t *testing.T, stream string) {
		clientConn, serverConn := net.Pipe()
		defer serverConn.Close()
		c := NewClient(clientConn, &Options{UnilateralData: &UnilateralDataHandler{
			StatusResponse: func(*StatusResponse) {},
			ContextUpdate: func(update *ContextUpdate) {
				want, ok := registered[update.Tag]
				if !ok || update.UID != want.uid {
					t.Errorf("delivered %q UID=%t to no matching registration", update.Tag, update.UID)
					return
				}
				for _, item := range update.Items {
					for _, change := range item.Changes {
						if want.uid != (len(change.UIDs) > 0) || want.uid == (len(change.SeqNums) > 0) {
							t.Errorf("change numbers do not match the registration's mode: %#v", change)
						}
						if want.sorted && change.Position == 0 {
							t.Error("sorted context delivered position 0")
						}
					}
				}
			},
		}})
		// The pipe is synchronous, so nothing is read before the writer runs.
		c.mu.Lock()
		c.searchContexts = make(map[string]*searchContext)
		for tag, registration := range registered {
			registration := registration
			c.searchContexts[tag] = &registration
		}
		c.mu.Unlock()
		go func() {
			_, _ = serverConn.Write([]byte("* OK ready\r\n" + stream))
			// Close so a stream that ends mid-response is an EOF, not a hang.
			_ = serverConn.Close()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = c.WaitGreeting(ctx, nil)
		select {
		case <-c.readerDone:
		case <-ctx.Done():
			t.Fatal("reader did not finish on a closed connection")
		}
		_ = c.Close()
	})
}
