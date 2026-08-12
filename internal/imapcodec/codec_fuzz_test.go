package imapcodec

import (
	"strings"
	"testing"

	"github.com/kiliant/go-imap/internal/imapwire"
)

func FuzzReadSearchCriteria(f *testing.F) {
	for _, seed := range []string{"ALL", "OR SEEN NOT DELETED", "1:*", "HEADER Subject \"x\"", "MODSEQ 42"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		dec := imapwire.NewDecoderString(input, fuzzOptions())
		_, _ = ReadSearchCriteria(dec)
	})
}

func FuzzSemanticStructures(f *testing.F) {
	for _, seed := range []string{
		`(NIL NIL NIL NIL NIL NIL NIL NIL NIL NIL)`,
		`("text" "plain" NIL NIL NIL "7bit" 0 0)`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		_, _ = ReadEnvelope(imapwire.NewDecoder(strings.NewReader(input), fuzzOptions()))
		_, _ = ReadBodyStructure(imapwire.NewDecoder(strings.NewReader(input), fuzzOptions()))
	})
}

func fuzzOptions() *imapwire.Options {
	return &imapwire.Options{
		MaxLiteralSize:         1 << 20,
		MaxBufferedLiteralSize: 1 << 20,
		MaxLineLength:          8 << 10,
		MaxListDepth:           64,
	}
}
