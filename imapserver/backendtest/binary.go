package backendtest

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
)

func runBinaryAppend(t *testing.T, harness *Harness) {
	t.Run("binary-append", func(t *testing.T) {
		instance, session := newSession(t, harness)
		defer closeSession(t, instance, session)
		requireWitness(t, instance, session, "BINARY")
		mailbox := "backendtest-binary"
		if err := session.Create(t.Context(), mailbox, nil); err != nil {
			t.Fatal(err)
		}
		raw := "Subject: binary\r\nContent-Type: application/octet-stream\r\nContent-Transfer-Encoding: binary\r\n\r\na\x00b"
		_, err := session.Append(t.Context(), mailbox, strings.NewReader(raw), &imapserver.AppendOptions{Size: int64(len(raw)), HasSize: true})
		if err != nil {
			var protocol *imap.Error
			if errors.As(err, &protocol) && protocol.Type == imap.ErrorTypeNo && protocol.Code == imap.CodeUnknownCTE {
				return
			}
			t.Fatalf("BINARY APPEND: %v", err)
		}
		result := selectMailbox(t, session, mailbox, discardUpdater())
		defer result.Mailbox.Unselect(context.Background(), nil)
		if len(result.Snapshot.UIDs) != 1 {
			t.Fatalf("APPEND published %v", result.Snapshot.UIDs)
		}
		var body string
		writer := &imapserver.FetchWriter{WriteFunc: func(_ context.Context, data *imap.FetchMessageData) error {
			for _, values := range data.Items {
				for _, value := range values {
					if section, ok := value.(*imap.FetchDataBinarySection); ok && section != nil && section.Literal != nil {
						content, err := io.ReadAll(section.Literal)
						if err != nil {
							return err
						}
						body = string(content)
					}
				}
			}
			return nil
		}}
		err = result.Mailbox.Fetch(t.Context(), writer, imap.UIDSetNum(result.Snapshot.UIDs[0]), &imapserver.FetchOptions{Items: []imap.FetchItem{&imap.FetchItemBinarySection{Part: []int{1}, Peek: true}}})
		if err != nil || body != "a\x00b" {
			t.Fatalf("binary content = %q, %v", body, err)
		}
	})
}
