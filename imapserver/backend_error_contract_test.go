package imapserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

func TestBackendErrorContract(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"protocol", fmt.Errorf("wrapped: %w", &imap.Error{Type: imap.ErrorTypeNo, Code: imap.CodeBadCharset, CodeArgs: "(UTF-8)", Text: "unsupported charset"}), "NO [BADCHARSET (UTF-8)] unsupported charset"},
		{"bad", &imap.Error{Type: imap.ErrorTypeBad, Text: "bad request"}, "BAD bad request"},
		{"other-type", &imap.Error{Type: imap.ErrorTypeBye, Text: "refused"}, "NO refused"},
		{"empty-text", &imap.Error{Code: imap.CodeCannot, Err: errors.New("private cause")}, "NO [CANNOT] SEARCH failed"},
		{"canceled", fmt.Errorf("wrapped: %w", context.Canceled), "NO [UNAVAILABLE] SEARCH failed"},
		{"deadline", context.DeadlineExceeded, "NO [UNAVAILABLE] SEARCH failed"},
		{"private-error", errors.New("private database failure"), "NO [SERVERBUG] SEARCH failed"},
		{"protocol-precedence", &imap.Error{Code: imap.CodeCannot, Text: "request refused", Err: context.Canceled}, "NO [CANNOT] request refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			c := &conn{encoder: imapwire.NewEncoder(&output, &imapwire.EncoderOptions{ServerResponse: true})}
			if err := writeBackendError(c, "T", "SEARCH", tc.err); err != nil {
				t.Fatal(err)
			}
			if want := "T " + tc.want + "\r\n"; output.String() != want {
				t.Fatalf("response = %q, want %q", output.String(), want)
			}
		})
	}
}
