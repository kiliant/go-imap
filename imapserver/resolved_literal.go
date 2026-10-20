package imapserver

import (
	"context"
	"io"
	"math"

	"github.com/kiliant/go-imap"
)

func resolvedContentTooBig() error {
	return &imap.Error{Type: imap.ErrorTypeNo, Code: imap.CodeTooBig, Text: "content exceeds the server's size limit"}
}

type literalContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *literalContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.reader.Read(p)
	if canceled := r.ctx.Err(); canceled != nil {
		return n, canceled
	}
	return n, err
}

// probingLimit leaves one byte for overflow detection without wrapping a valid
// maximum int64 configuration into a negative LimitReader length.
func probingLimit(limit int64) int64 {
	if limit == math.MaxInt64 {
		return limit
	}
	return limit + 1
}
