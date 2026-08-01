package imapclient

import (
	"errors"
	"strings"
	"testing"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

func TestIDSendsNILAndParsesServerList(t *testing.T) {
	var sent string
	c, ctx := newExtATestClient(t, "* OK [CAPABILITY IMAP4REV1 ID] ready", func(s *extAServer) {
		tag, rest := s.command()
		sent = rest
		s.reply(`* ID ("name" "Cyrus" "version" "1.5" "os" "sunos")`, tag+" OK done")
	})
	data, err := c.ID(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sent != "ID NIL" {
		t.Fatalf("wire form = %q", sent)
	}
	if !data.Received || len(data.Fields) != 3 {
		t.Fatalf("data = %#v", data)
	}
	if data.Fields[0].Name != "name" || data.Fields[0].Value == nil || *data.Fields[0].Value != "Cyrus" {
		t.Fatalf("first field = %#v", data.Fields[0])
	}
	if data.Fields[2].Name != "os" || data.Fields[2].Value == nil || *data.Fields[2].Value != "sunos" {
		t.Fatalf("os field = %#v", data.Fields[2])
	}
}

func TestIDSendsClientFieldsAndParsesNIL(t *testing.T) {
	var sent string
	c, ctx := newExtATestClient(t, "* OK [CAPABILITY IMAP4REV1 ID] ready", func(s *extAServer) {
		tag, rest := s.command()
		sent = rest
		s.reply("* ID NIL", tag+" OK done")
	})
	data, err := c.ID(ctx, &IDOptions{
		Fields: []IDField{
			{Name: "name", Value: IDString("go-imap")},
			{Name: "version", Value: IDString("0.1")},
			{Name: "vendor", Value: nil},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sent, "ID (") || !strings.Contains(sent, `"name"`) || !strings.Contains(sent, `"go-imap"`) {
		t.Fatalf("wire form = %q", sent)
	}
	if !strings.Contains(sent, "NIL") {
		t.Fatalf("expected NIL value on wire: %q", sent)
	}
	if !data.Received || data.Fields != nil {
		t.Fatalf("expected Received with NIL list, got %#v", data)
	}
}

func TestIDAllowsMissingUntagged(t *testing.T) {
	c, ctx := newExtATestClient(t, "* OK [CAPABILITY IMAP4REV1 ID] ready", func(s *extAServer) {
		tag, _ := s.command()
		s.ok(tag)
	})
	data, err := c.ID(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if data.Received || data.Fields != nil {
		t.Fatalf("expected empty IDData, got %#v", data)
	}
}

func TestIDRequiresCapability(t *testing.T) {
	sawCommand := false
	c, ctx := newExtATestClient(t, "* OK [CAPABILITY IMAP4REV1] ready", func(s *extAServer) {
		if tag, _ := s.command(); tag != "" {
			sawCommand = true
			s.ok(tag)
		}
	})
	_, err := c.ID(ctx, nil)
	if !errors.Is(err, ErrCapabilityNotAdvertised) {
		t.Fatalf("err = %v", err)
	}
	if sawCommand {
		t.Fatal("ID must not be sent without the capability")
	}
}

func TestIDRequiresContext(t *testing.T) {
	c, _ := newExtATestClient(t, "* OK [CAPABILITY IMAP4REV1 ID] ready", func(s *extAServer) {
		if tag, _ := s.command(); tag != "" {
			s.ok(tag)
		}
	})
	_, err := c.ID(nil, nil)
	var imapErr *imap.Error
	if !errors.As(err, &imapErr) || imapErr.Text == "" {
		t.Fatalf("err = %v", err)
	}
}

func TestIDRejectsDuplicateAndOversizedFields(t *testing.T) {
	sawCommand := false
	c, ctx := newExtATestClient(t, "* OK [CAPABILITY IMAP4REV1 ID] ready", func(s *extAServer) {
		if tag, _ := s.command(); tag != "" {
			sawCommand = true
			s.ok(tag)
		}
	})
	_, err := c.ID(ctx, &IDOptions{Fields: []IDField{
		{Name: "name", Value: IDString("a")},
		{Name: "Name", Value: IDString("b")},
	}})
	if err == nil {
		t.Fatal("expected duplicate-name error")
	}
	_, err = c.ID(ctx, &IDOptions{Fields: []IDField{
		{Name: strings.Repeat("x", 31), Value: IDString("a")},
	}})
	if err == nil {
		t.Fatal("expected long-name error")
	}
	_, err = c.ID(ctx, &IDOptions{Fields: []IDField{
		{Name: "name", Value: IDString(strings.Repeat("v", 1025))},
	}})
	if err == nil {
		t.Fatal("expected long-value error")
	}
	if sawCommand {
		t.Fatal("invalid ID must not hit the wire")
	}
}

func TestReadIDResponseEmptyList(t *testing.T) {
	data, err := readIDResponse(imapwire.NewDecoderString(" ()\r\n", nil))
	if err != nil {
		t.Fatal(err)
	}
	if !data.Received || data.Fields == nil || len(data.Fields) != 0 {
		t.Fatalf("data = %#v", data)
	}
}

func TestReadIDResponseNilValue(t *testing.T) {
	data, err := readIDResponse(imapwire.NewDecoderString(` ("name" NIL "version" "1")`+"\r\n", nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(data.Fields) != 2 || data.Fields[0].Value != nil {
		t.Fatalf("data = %#v", data)
	}
	if data.Fields[1].Value == nil || *data.Fields[1].Value != "1" {
		t.Fatalf("version = %#v", data.Fields[1])
	}
}

func TestReadIDResponseRejectsOversizedInbound(t *testing.T) {
	_, err := readIDResponse(imapwire.NewDecoderString(` ("`+strings.Repeat("n", 31)+`" "x")`+"\r\n", nil))
	if err == nil {
		t.Fatal("expected long inbound name error")
	}
	_, err = readIDResponse(imapwire.NewDecoderString(` ("name" "`+strings.Repeat("v", 1025)+`")`+"\r\n", nil))
	if err == nil {
		t.Fatal("expected long inbound value error")
	}
}
