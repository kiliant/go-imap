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

func TestFetchAdvertisesNewKeywordsBeforeFlagsData(t *testing.T) {
	var wire bytes.Buffer
	c := &conn{encoder: imapwire.NewEncoder(&wire, &imapwire.EncoderOptions{ServerResponse: true})}
	c.state.selected = &selectedState{flags: []imap.Flag{imap.FlagSeen}}
	data := &imap.FetchMessageData{SeqNum: 4, Items: map[imap.FetchDataKey][]imap.FetchData{
		imap.FetchDataKey(imap.FetchItemFlags): {
			imap.FetchDataFlags{imap.FlagSeen, "$Label4", "$Label5"},
		},
	}}

	if err := writeFetchLikeResponse(c, data); err != nil {
		t.Fatal(err)
	}
	if err := c.encoder.Flush(); err != nil {
		t.Fatal(err)
	}
	got := wire.String()
	flagsAt := strings.Index(got, "* FLAGS ")
	fetchAt := strings.Index(got, "* 4 FETCH ")
	if flagsAt < 0 || fetchAt < 0 || flagsAt > fetchAt {
		t.Fatalf("mailbox FLAGS did not precede FETCH FLAGS:\n%s", got)
	}
	if !strings.Contains(got[:fetchAt], "$Label4") || !strings.Contains(got[:fetchAt], "$Label5") {
		t.Fatalf("mailbox FLAGS did not announce every fetched keyword:\n%s", got)
	}
	if !imap.ContainsFlag(c.state.selected.flags, "$Label4") || !imap.ContainsFlag(c.state.selected.flags, "$Label5") {
		t.Fatalf("selected applicable flags were not advanced: %v", c.state.selected.flags)
	}
}
