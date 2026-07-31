package imapclient

import (
	"testing"

	"github.com/kiliant/go-imap/internal/imapwire"
)

// A hostile server must not be able to panic the client through the ENVELOPE or
// BODYSTRUCTURE productions, which are the deepest recursion in the response
// grammar. An error is a fine outcome; a panic is not.

func FuzzReadEnvelope(f *testing.F) {
	f.Add(`("Wed, 17 Jul 1996 02:23:25 -0700" "s" ((NIL NIL "a" "b")) NIL NIL NIL NIL NIL NIL "<i@b>")`)
	f.Add(`(NIL NIL NIL NIL NIL NIL NIL NIL NIL NIL)`)
	f.Add(`()`)
	f.Add(`(`)
	f.Add(`("d" "s" ((("g" NIL "grp" NIL))) NIL NIL NIL NIL NIL NIL NIL)`)
	f.Fuzz(func(t *testing.T, s string) {
		dec := imapwire.NewDecoderString(s, nil)
		if env, err := readEnvelope(dec); err == nil && env == nil {
			t.Fatal("readEnvelope returned no envelope and no error")
		}
	})
}

func FuzzReadBodyStructure(f *testing.F) {
	f.Add(`("TEXT" "PLAIN" ("CHARSET" "UTF-8") NIL NIL "7BIT" 1152 23)`)
	f.Add(`(("TEXT" "PLAIN" NIL NIL NIL "7BIT" 1 1)("TEXT" "HTML" NIL NIL NIL "7BIT" 1 1) "ALTERNATIVE")`)
	f.Add(`("MESSAGE" "RFC822" NIL NIL NIL "7BIT" 4 (NIL NIL NIL NIL NIL NIL NIL NIL NIL NIL) ("TEXT" "PLAIN" NIL NIL NIL "7BIT" 1 1) 60)`)
	f.Add(`("TEXT" "PLAIN" NIL NIL NIL "7BIT" 1 1 "md5" ("inline" ("FILENAME" "a")) ("en") "http://x")`)
	f.Add(`(((((((("a"`)
	f.Fuzz(func(t *testing.T, s string) {
		dec := imapwire.NewDecoderString(s, nil)
		if bs, err := readBodyStructure(dec, 0); err == nil && bs == nil {
			t.Fatal("readBodyStructure returned no structure and no error")
		}
	})
}
