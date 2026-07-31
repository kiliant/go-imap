package imapwire

import (
	"errors"
	"testing"
)

func TestMailboxNameCodec(t *testing.T) {
	tests := []struct {
		utf8, encoded string
	}{
		{utf8: "", encoded: ""},
		{utf8: "INBOX", encoded: "INBOX"},
		{utf8: "R&D", encoded: "R&-D"},
		{utf8: "Envoyé", encoded: "Envoy&AOk-"},
		{utf8: "台北", encoded: "&U,BTFw-"},
		{utf8: "日本語", encoded: "&ZeVnLIqe-"},
		{utf8: "~peter/mail/台北/日本語", encoded: "~peter/mail/&U,BTFw-/&ZeVnLIqe-"},
		{utf8: "😀", encoded: "&2D3eAA-"},
		{utf8: "\n", encoded: "&AAo-"},
	}
	for _, tc := range tests {
		t.Run(tc.encoded, func(t *testing.T) {
			encoded, err := EncodeMailboxName(tc.utf8)
			if err != nil {
				t.Fatal(err)
			}
			if encoded != tc.encoded {
				t.Fatalf("EncodeMailboxName(%q) = %q, want %q", tc.utf8, encoded, tc.encoded)
			}
			decoded, err := DecodeMailboxName(tc.encoded)
			if err != nil {
				t.Fatal(err)
			}
			if decoded != tc.utf8 {
				t.Fatalf("DecodeMailboxName(%q) = %q, want %q", tc.encoded, decoded, tc.utf8)
			}
		})
	}
}

func TestDecodeMailboxNameRejectsMalformedInput(t *testing.T) {
	for _, input := range []string{
		"&",
		"&A-",
		"&2AA-",
		"&3AA-",
		"&Jjo",
		"&Jjo!-",
		"café",
		"a\x00b",
		"a\x7fb",
	} {
		t.Run(input, func(t *testing.T) {
			if _, err := DecodeMailboxName(input); !errors.Is(err, ErrInvalidMailboxName) {
				t.Fatalf("DecodeMailboxName(%q) error = %v", input, err)
			}
		})
	}
}

func TestEncodeMailboxNameRejectsInvalidUTF8(t *testing.T) {
	if _, err := EncodeMailboxName("\xff"); !errors.Is(err, ErrInvalidMailboxName) {
		t.Fatalf("error = %v", err)
	}
}
