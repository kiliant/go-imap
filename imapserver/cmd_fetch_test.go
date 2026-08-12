package imapserver

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapcodec"
	"github.com/kiliant/go-imap/internal/imapwire"
)

type opaqueFetchReader struct{ io.Reader }

func TestOpaqueFetchLiteralIsBoundedStagedAndRemoved(t *testing.T) {
	data := &imap.FetchMessageData{SeqNum: 1, Items: map[imap.FetchDataKey][]imap.FetchData{
		"BODY[]": {&imap.FetchDataBodySection{Literal: &opaqueFetchReader{Reader: strings.NewReader("message")}}},
	}}
	if _, cleanup, err := prepareFetchResponseLiterals(data, 64); err != nil {
		t.Fatal(err)
	} else {
		reader := fetchLiteralReader(data.Items["BODY[]"][0])
		file, ok := reader.(*os.File)
		if !ok {
			t.Fatalf("staged literal reader = %T", reader)
		}
		name := file.Name()
		wireSize, err := fetchResponseWireSize(data)
		if err != nil {
			cleanup()
			t.Fatal(err)
		}
		var wire bytes.Buffer
		encoder := imapwire.NewEncoder(&wire, &imapwire.EncoderOptions{ServerResponse: true})
		if err := imapcodec.WriteFetchResponse(encoder, data, fetchLiteralSize); err != nil {
			cleanup()
			t.Fatal(err)
		}
		if err := encoder.Flush(); err != nil {
			cleanup()
			t.Fatal(err)
		}
		if wireSize != int64(wire.Len()) {
			cleanup()
			t.Fatalf("preflight wire size = %d, actual = %d", wireSize, wire.Len())
		}
		cleanup()
		if _, err := os.Stat(name); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("temporary FETCH spool remains: %v", err)
		}
	}
}

func TestOpaqueFetchLiteralLimitIsProtocolLimit(t *testing.T) {
	data := &imap.FetchMessageData{SeqNum: 1, Items: map[imap.FetchDataKey][]imap.FetchData{
		"BODY[]": {&imap.FetchDataBodySection{Literal: &opaqueFetchReader{Reader: strings.NewReader("too large")}}},
	}}
	_, cleanup, err := prepareFetchResponseLiterals(data, 3)
	cleanup()
	var protocolErr *imap.Error
	if !errors.As(err, &protocolErr) || protocolErr.Code != imap.CodeLimit {
		t.Fatalf("limit error = %v", err)
	}
}
