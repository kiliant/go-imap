package imapserver_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap/imapserver"
	"github.com/kiliant/go-imap/imapserver/memory"
)

// Hiding the optional interface must withdraw MULTIAPPEND and still refuse its
// syntax without committing the first message through the mandatory Append.
type singleAppendBackend struct{ *memory.Backend }

func (b singleAppendBackend) Authenticate(ctx context.Context, info *imapserver.ConnInfo, credentials *imapserver.Credentials, options *imapserver.AuthenticateOptions) (imapserver.Session, error) {
	session, err := b.Backend.Authenticate(ctx, info, credentials, options)
	if err != nil {
		return nil, err
	}
	return struct{ imapserver.Session }{session}, nil
}

func TestMultiAppendRefusalLeavesMailboxUnchanged(t *testing.T) {
	for _, atomic := range []bool{false, true} {
		name := "single-only"
		if atomic {
			name = "atomic"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			var backend imapserver.Backend = memory.New(&memory.Options{Users: map[string]string{"alice": "secret"}})
			if !atomic {
				backend = singleAppendBackend{backend.(*memory.Backend)}
			}
			_, client, reader := newRawSessionOn(t, ctx, imapserver.New(backend, &imapserver.Options{AllowInsecureAuth: true}))
			writeRawCommand(t, client, "C CAPABILITY\r\n")
			lines, _ := collectUntilTag(t, reader, "C ")
			if strings.Contains(strings.Join(lines, " "), "MULTIAPPEND") != atomic {
				t.Fatalf("capability witness: %v", lines)
			}
			for _, payload := range []string{
				"{7+}\r\nhello\r\n {0+}\r\n", // #121's original probe.
				"{0+}\r\n {7+}\r\nhello\r\n",
				"{0+}\r\n",
				"{7+}\r\nhello\r\n invalid",
				"{7+}\r\nhello\r\n CATENATE (URL \"imap://localhost/INBOX/;UID=999\")",
			} {
				writeRawCommand(t, client, "A APPEND INBOX "+payload+"\r\n")
				if _, tagged := collectUntilTag(t, reader, "A "); strings.HasPrefix(tagged, "A OK") {
					t.Fatalf("invalid batch succeeded: %q", tagged)
				}
				writeRawCommand(t, client, "S STATUS INBOX (MESSAGES UIDNEXT)\r\n")
				lines, tagged := collectUntilTag(t, reader, "S ")
				if !strings.HasPrefix(tagged, "S OK") || len(lines) != 1 || !strings.Contains(lines[0], "MESSAGES 0") || !strings.Contains(lines[0], "UIDNEXT 1") {
					t.Fatalf("failed append changed mailbox or desynchronized command: %v %q", lines, tagged)
				}
			}
			if !atomic {
				writeRawCommand(t, client, "A APPEND INBOX {1+}\r\nx {1+}\r\ny\r\n")
				if _, tagged := collectUntilTag(t, reader, "A "); !strings.HasPrefix(tagged, "A NO") {
					t.Fatalf("unwitnessed MULTIAPPEND = %q", tagged)
				}
				writeRawCommand(t, client, "S STATUS INBOX (MESSAGES)\r\n")
				lines, _ := collectUntilTag(t, reader, "S ")
				if !strings.Contains(findResponse(t, lines, "* STATUS"), "MESSAGES 0") {
					t.Fatal("unwitnessed MULTIAPPEND committed a prefix")
				}
			}
		})
	}
}
