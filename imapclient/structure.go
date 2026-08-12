package imapclient

import (
	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapcodec"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// These narrow adapters preserve the package-private call sites and fuzz
// targets while the semantic implementation is shared with the server through
// internal/imapcodec.
func readEnvelope(dec *imapwire.Decoder) (*imap.Envelope, error) {
	return imapcodec.ReadEnvelope(dec)
}

func readBodyStructure(dec *imapwire.Decoder, depth int) (imap.BodyStructure, error) {
	_ = depth // retained for the pre-migration package-private test adapter
	return imapcodec.ReadBodyStructure(dec)
}
