package imapwire

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestBeginCommandAndLiteralDecision(t *testing.T) {
	dec := NewDecoderString("A1 APPEND inbox {3}\r\nabc\r\nA2 NOOP\r\n", nil)
	tag, name, err := dec.BeginCommand()
	if err != nil || tag != "A1" || name != "APPEND" {
		t.Fatalf("BeginCommand = %q, %q, %v", tag, name, err)
	}
	var mailbox string
	if !dec.ExpectSP() || !dec.ExpectMailbox(&mailbox) || mailbox != "INBOX" || !dec.ExpectSP() {
		t.Fatalf("APPEND prefix: %v", dec.Err())
	}
	info, ok := dec.LiteralAnnouncement()
	if !ok || info != (LiteralInfo{Size: 3}) {
		t.Fatalf("literal = %#v, %v (%v)", info, ok, dec.Err())
	}
	lr := dec.OpenLiteral(info)
	b, err := io.ReadAll(lr)
	if err != nil || string(b) != "abc" {
		t.Fatalf("payload = %q, %v", b, err)
	}
	if !dec.ExpectCRLF() {
		t.Fatalf("APPEND CRLF: %v", dec.Err())
	}
	tag, name, err = dec.BeginCommand()
	if err != nil || tag != "A2" || name != "NOOP" || !dec.ExpectCRLF() {
		t.Fatalf("second command = %q, %q, %v (%v)", tag, name, err, dec.Err())
	}
}

func TestRejectSynchronisingLiteral(t *testing.T) {
	dec := NewDecoderString("A1 APPEND inbox {9}\r\nA2 NOOP\r\n", nil)
	_, _, _ = dec.BeginCommand()
	var mailbox string
	if !dec.ExpectSP() || !dec.ExpectMailbox(&mailbox) || !dec.ExpectSP() {
		t.Fatal(dec.Err())
	}
	info, ok := dec.LiteralAnnouncement()
	if !ok {
		t.Fatal(dec.Err())
	}
	if err := dec.RejectLiteral(info); err != nil {
		t.Fatal(err)
	}
	tag, name, err := dec.BeginCommand()
	if err != nil || tag != "A2" || name != "NOOP" {
		t.Fatalf("after rejection = %q, %q, %v", tag, name, err)
	}
}

func TestNonSynchronisingLiteralMustDrain(t *testing.T) {
	dec := NewDecoderString("A1 APPEND inbox {3+}\r\nabc\r\n", nil)
	_, _, _ = dec.BeginCommand()
	var mailbox string
	if !dec.ExpectSP() || !dec.ExpectMailbox(&mailbox) || !dec.ExpectSP() {
		t.Fatal(dec.Err())
	}
	info, ok := dec.LiteralAnnouncement()
	if !ok || !info.NonSynchronising {
		t.Fatalf("literal = %#v, %v", info, ok)
	}
	if err := dec.RejectLiteral(info); err == nil {
		t.Fatal("RejectLiteral accepted a non-synchronising literal")
	}
	if got := string(mustReadAll(t, dec.OpenLiteral(info))); got != "abc" {
		t.Fatalf("payload = %q", got)
	}
}

func TestCommandStringLiteralDecision(t *testing.T) {
	dec := NewDecoderString("{3}\r\nabc", nil)
	called := false
	dec.SetLiteralDecision(func(info LiteralInfo) error {
		called = true
		if info != (LiteralInfo{Size: 3}) {
			t.Fatalf("announcement = %#v", info)
		}
		return nil // a real server writes and flushes "+ ...\\r\\n" here
	})
	var value string
	if !dec.ExpectString(&value) || value != "abc" || !called {
		t.Fatalf("string = %q, called = %v, err = %v", value, called, dec.Err())
	}
}

func TestRejectedSynchronisingStringPreservesPipelinedCommand(t *testing.T) {
	wantErr := errors.New("quota exceeded")
	dec := NewDecoderString("A1 X {3}\r\nA2 NOOP\r\n", nil)
	dec.SetLiteralDecision(func(LiteralInfo) error { return wantErr })
	_, _, _ = dec.BeginCommand()
	var value string
	if !dec.ExpectSP() || dec.ExpectString(&value) {
		t.Fatalf("literal unexpectedly accepted: %q", value)
	}
	if !errors.Is(dec.Err(), wantErr) || dec.Fatal() {
		t.Fatalf("error = %#v", dec.Err())
	}
	if err := dec.DiscardLine(); err != nil {
		t.Fatal(err)
	}
	tag, name, err := dec.BeginCommand()
	if err != nil || tag != "A2" || name != "NOOP" || !dec.ExpectCRLF() {
		t.Fatalf("next command = %q %q, %v (%v)", tag, name, err, dec.Err())
	}
}

func TestRejectedNonSynchronisingStringIsDrained(t *testing.T) {
	wantErr := errors.New("not permitted")
	dec := NewDecoderString("A1 X {3+}\r\nabc trailing\r\nA2 NOOP\r\n", nil)
	dec.SetLiteralDecision(func(LiteralInfo) error { return wantErr })
	_, _, _ = dec.BeginCommand()
	var value string
	if !dec.ExpectSP() || dec.ExpectString(&value) {
		t.Fatalf("literal unexpectedly accepted: %q", value)
	}
	if !errors.Is(dec.Err(), wantErr) || dec.Fatal() {
		t.Fatalf("error = %#v", dec.Err())
	}
	if err := dec.DiscardLine(); err != nil {
		t.Fatal(err)
	}
	tag, name, err := dec.BeginCommand()
	if err != nil || tag != "A2" || name != "NOOP" {
		t.Fatalf("next command = %q %q, %v (%v)", tag, name, err, dec.Err())
	}
}

func TestOversizedSynchronisingCommandLiteralIsRecoverable(t *testing.T) {
	dec := NewDecoderString("A1 X {17}\r\nA2 NOOP\r\n", &Options{MaxLiteralSize: 16})
	_, _, _ = dec.BeginCommand()
	if !dec.ExpectSP() {
		t.Fatal(dec.Err())
	}
	info, ok := dec.LiteralAnnouncement()
	if !ok || info.Size != 17 || dec.Err() != nil {
		t.Fatalf("announcement = %#v, %v (%v)", info, ok, dec.Err())
	}
	if err := dec.RejectLiteral(info); err != nil {
		t.Fatal(err)
	}
	tag, name, err := dec.BeginCommand()
	if err != nil || tag != "A2" || name != "NOOP" {
		t.Fatalf("next command = %q %q, %v (%v)", tag, name, err, dec.Err())
	}
}

func TestResponseEncodeDecode(t *testing.T) {
	var buf bytes.Buffer
	enc := NewEncoder(&buf, &EncoderOptions{ServerResponse: true})
	enc.BeginResponse(ResponseTagged, "A7").RespCond(RespCond{
		Status: "OK",
		Text:   RespText{Code: "uidvalidity", Args: "42", Text: "selected"},
	}).CRLF()
	if err := enc.Flush(); err != nil {
		t.Fatal(err)
	}
	dec := NewDecoder(bytes.NewReader(buf.Bytes()), nil)
	kind, tag, err := dec.BeginResponse()
	if err != nil || kind != ResponseTagged || tag != "A7" || !dec.ExpectSP() {
		t.Fatalf("framing = %v, %q, %v (%v)", kind, tag, err, dec.Err())
	}
	var cond RespCond
	if !dec.ExpectRespCond(&cond) || !dec.ExpectCRLF() {
		t.Fatal(dec.Err())
	}
	if cond.Status != "OK" || cond.Text.Code != "UIDVALIDITY" || cond.Text.Args != "42" || cond.Text.Text != "selected" {
		t.Fatalf("condition = %#v", cond)
	}
}

func TestEmptyResponseConditionUsesRequiredSpace(t *testing.T) {
	var buf bytes.Buffer
	enc := NewEncoder(&buf, &EncoderOptions{ServerResponse: true})
	enc.BeginResponse(ResponseTagged, "A1").RespCond(RespCond{Status: "OK"}).CRLF()
	if err := enc.Flush(); err != nil {
		t.Fatal(err)
	}
	if got, want := buf.String(), "A1 OK \r\n"; got != want {
		t.Fatalf("response = %q, want %q", got, want)
	}
}

func TestResponseEncoderRejectsInjectionAndLiteralNUL(t *testing.T) {
	t.Run("response-code", func(t *testing.T) {
		enc := NewEncoder(io.Discard, &EncoderOptions{ServerResponse: true})
		enc.RespText(RespText{Code: "X", Args: "arg] forged"})
		if enc.Err() == nil {
			t.Fatal("closing bracket accepted in raw response-code arguments")
		}
	})
	t.Run("literal", func(t *testing.T) {
		enc := NewEncoder(io.Discard, &EncoderOptions{ServerResponse: true})
		lw, err := enc.ResponseLiteral(1, false)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := lw.Write([]byte{0}); err == nil {
			t.Fatal("NUL accepted in a non-binary literal")
		}
	})
}

func mustReadAll(t *testing.T, r io.Reader) []byte {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
