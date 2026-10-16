package imapclient

import (
	"fmt"
	"io"
	"runtime"
	"strings"
	"testing"

	"github.com/kiliant/go-imap"
)

func TestFetchPublishedItemsRemainImmutable(t *testing.T) {
	for _, item := range []string{"BODY[]", "BINARY[]", "RFC822"} {
		t.Run(item, func(t *testing.T) {
			const count = 256
			c, ctx := newExtATestClient(t, "* OK ready", func(s *extAServer) {
				tag, _ := s.command()
				var wire strings.Builder
				fmt.Fprintf(&wire, "* 1 FETCH (%s {1}\r\nx", item)
				for range count {
					wire.WriteString(" RFC822.SIZE 1")
				}
				fmt.Fprintf(&wire, " %s {1}\r\ny UID 42)\r\n", item)
				s.reply(strings.TrimSuffix(wire.String(), "\r\n"), tag+" OK done")
				tag, _ = s.command()
				s.ok(tag)
			})
			c.mu.Lock()
			c.state = StateSelected
			c.mu.Unlock()
			cmd := c.Fetch(imap.SeqSetNum(1), nil, &imap.FetchItemBodySection{Peek: true})
			first, err := cmd.Next(ctx)
			if err != nil {
				t.Fatal(err)
			}
			stop, observed := make(chan struct{}), make(chan struct{})
			// Keep reading the published map while draining literals allows decoding to
			// resume. Retaining Items must be safe even after its literal is exhausted.
			go func() {
				defer close(observed)
				for {
					select {
					case <-stop:
						return
					default:
					}
					for _, values := range first.Items {
						_ = len(values)
						runtime.Gosched()
					}
				}
			}()
			sizes, bodies, uids := 0, 0, 0
			data := first
			for {
				for _, values := range data.Items {
					for _, value := range values {
						var literal io.Reader
						switch value := value.(type) {
						case *imap.FetchDataBodySection:
							literal = value.Literal
						case *imap.FetchDataBinarySection:
							literal = value.Literal
						case *imap.FetchDataLiteral:
							literal = value.Literal
						case imap.FetchDataRFC822Size:
							sizes++
						case imap.FetchDataUID:
							if value != 42 {
								t.Errorf("UID = %d", value)
							}
							uids++
						}
						if literal != nil {
							if _, err := io.Copy(io.Discard, literal); err != nil {
								t.Error(err)
							}
							bodies++
						}
					}
				}
				data, err = cmd.Next(ctx)
				if err != nil {
					break
				}
			}
			close(stop)
			<-observed
			if err != io.EOF {
				t.Fatal(err)
			}
			if sizes != count || bodies != 2 || uids != 1 {
				t.Fatalf("items: sizes=%d bodies=%d UIDs=%d", sizes, bodies, uids)
			}
			if len(first.Items) != 1 || len(first.Items[imap.FetchDataKey(item)]) != 1 {
				t.Fatalf("published map changed: %v", first.Items)
			}
			if err := c.Noop(nil).Wait(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}
