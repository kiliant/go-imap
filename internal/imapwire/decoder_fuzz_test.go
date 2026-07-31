package imapwire

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

var fuzzOptions = Options{
	MaxLiteralSize:         64 << 10,
	MaxBufferedLiteralSize: 64 << 10,
	MaxLineLength:          64 << 10,
	MaxListDepth:           32,
}

func assertWireError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	var wireErr *Error
	if !errors.As(err, &wireErr) {
		t.Fatalf("decoder returned %T, want *imapwire.Error: %v", err, err)
	}
}

func addWireCorpus(f *testing.F) {
	f.Helper()
	err := filepath.WalkDir("testdata", func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		f.Add(string(data))
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		f.Fatal(err)
	}
}

func FuzzDecoderAtom(f *testing.F) {
	addWireCorpus(f)
	for _, seed := range []string{"OK\r\n", "IMAP4rev1\r\n", "bad\x00atom\r\n", "\r\n"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		decoder := NewDecoderString(input, &fuzzOptions)
		var value string
		decoder.ExpectAtom(&value)
		decoder.ExpectCRLF()
		assertWireError(t, decoder.Err())
	})
}

func FuzzDecoderString(f *testing.F) {
	addWireCorpus(f)
	for _, seed := range []string{
		"\"quoted\\\" value\"\r\n",
		"{5}\r\nhello\r\n",
		"{4294967295}\r\n",
		"~{3}\r\nabc\r\n",
		"\"unterminated",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		decoder := NewDecoderString(input, &fuzzOptions)
		var value string
		decoder.ExpectString(&value)
		assertWireError(t, decoder.Err())
	})
}

func FuzzDecoderNumber(f *testing.F) {
	addWireCorpus(f)
	for _, seed := range []string{"0", "1", "4294967295", "4294967296", "9223372036854775808", "-1", ""} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		decoder32 := NewDecoderString(input, &fuzzOptions)
		var number32 uint32
		decoder32.ExpectNumber(&number32)
		assertWireError(t, decoder32.Err())

		decoder64 := NewDecoderString(input, &fuzzOptions)
		var number64 int64
		decoder64.ExpectNumber64(&number64)
		assertWireError(t, decoder64.Err())
	})
}

func FuzzDecoderList(f *testing.F) {
	addWireCorpus(f)
	for _, seed := range []string{
		"()",
		"(one \"two\" (three NIL))",
		"((((((((((((((((((((((((((((((((((NIL))))))))))))))))))))))))))))))))))",
		"(unterminated",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		decoder := NewDecoderString(input, &fuzzOptions)
		err := decoder.DiscardValue()
		if err == nil {
			err = decoder.Err()
		}
		assertWireError(t, err)
	})
}

func FuzzDecoderDiscardLine(f *testing.F) {
	addWireCorpus(f)
	for _, seed := range []string{
		"* OK ready\r\nnext",
		"* 1 FETCH (BODY[] {5}\r\nhello)\r\n",
		"* OK bad\x00atom\r\n",
		"* OK broken\r",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		decoder := NewDecoderString(input, &fuzzOptions)
		assertWireError(t, decoder.DiscardLine())
	})
}

func FuzzDecoderMailbox(f *testing.F) {
	addWireCorpus(f)
	for _, seed := range []string{"INBOX", "T&AOs-st", "\"quoted mailbox\"", "*", "%", "{4}\r\nTest"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		decoder := NewDecoderString(input, &fuzzOptions)
		var mailbox string
		decoder.ListMailbox(&mailbox)
		assertWireError(t, decoder.Err())
	})
}

// FuzzDecoder drives the response framing and discard path that every server
// response crosses. The production-specific fuzzers below exercise individual
// token decoders more deeply.
func FuzzDecoder(f *testing.F) {
	addWireCorpus(f)
	for _, seed := range []string{
		"* OK ready\r\n",
		"A001 OK completed\r\n",
		"+ continue\r\n",
		"* BYE truncated",
		"* * *\r\n",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		decoder := NewDecoderString(input, &fuzzOptions)
		_, _, err := decoder.BeginResponse()
		if errors.Is(err, io.EOF) {
			return
		}
		if err == nil {
			err = decoder.DiscardLine()
		}
		assertWireError(t, err)
	})
}

func FuzzDecoderBodySection(f *testing.F) {
	for _, seed := range []string{
		"[]",
		"[HEADER]",
		"[1.2.MIME]",
		"[HEADER.FIELDS (From Subject)]<0.1024>",
		"[1.0]",
		"[" + strings.Repeat("1.", 1000) + "TEXT]",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		decoder := NewDecoderString(input, &fuzzOptions)
		var section BodySection
		decoder.ExpectBodySection(&section)
		assertWireError(t, decoder.Err())
	})
}

func FuzzDecoderResponseContent(f *testing.F) {
	for _, seed := range []struct {
		mode byte
		text string
	}{
		{0, " [ALERT] continue\r\n"},
		{1, "[CAPABILITY IMAP4rev1 IDLE] ready"},
		{2, "OK [UIDVALIDITY 123] selected"},
		{3, "T&AOs-st"},
		{4, "(\\Seen \\Flagged custom)"},
	} {
		f.Add(seed.mode, seed.text)
	}
	f.Fuzz(func(t *testing.T, mode byte, input string) {
		decoder := NewDecoderString(input, &fuzzOptions)
		switch mode % 5 {
		case 0:
			var text string
			decoder.ExpectContinuationText(&text)
		case 1:
			var text RespText
			decoder.ExpectRespText(&text)
		case 2:
			var condition RespCond
			decoder.ExpectRespCond(&condition)
		case 3:
			var mailbox string
			decoder.ExpectMailbox(&mailbox)
		case 4:
			var flags []string
			_ = decoder.ExpectFlagList(&flags)
		}
		assertWireError(t, decoder.Err())
	})
}

func FuzzDecoderDate(f *testing.F) {
	for _, seed := range []struct {
		dateTime bool
		text     string
	}{
		{true, "\"17-Jul-1996 02:44:25 -0700\""},
		{true, "\" 7-Jul-1996 02:44:25 +0000\""},
		{false, "1-Feb-2024"},
		{false, "\"29-Feb-2024\""},
	} {
		f.Add(seed.dateTime, seed.text)
	}
	f.Fuzz(func(t *testing.T, dateTime bool, input string) {
		decoder := NewDecoderString(input, &fuzzOptions)
		var value time.Time
		if dateTime {
			decoder.ExpectDateTime(&value)
		} else {
			decoder.ExpectDate(&value)
		}
		assertWireError(t, decoder.Err())
	})
}

func FuzzUTF7(f *testing.F) {
	for _, seed := range []string{"INBOX", "Tëst", "台北/日本語", "R&D", "&", "\xff"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, mailbox string) {
		encoded, err := EncodeMailboxName(mailbox)
		if err != nil {
			return
		}
		decoded, err := DecodeMailboxName(encoded)
		if err != nil {
			t.Fatalf("decode encoded mailbox %q: %v", encoded, err)
		}
		if decoded != mailbox {
			t.Fatalf("round trip = %q, want %q", decoded, mailbox)
		}
	})
}

func TestHostileLiteralSizeRejectedBeforeAllocation(t *testing.T) {
	decoder := NewDecoderString("{4294967295}\r\n", &Options{MaxLiteralSize: 1 << 20})
	if literal, ok := decoder.Literal(); ok || literal != nil {
		t.Fatal("oversized literal accepted")
	}
	if !errors.Is(decoder.Err(), ErrLimitExceeded) || !decoder.Fatal() {
		t.Fatalf("error = %#v, want fatal ErrLimitExceeded", decoder.Err())
	}
}

func TestHostileLineAndNestingLimits(t *testing.T) {
	t.Run("line", func(t *testing.T) {
		decoder := NewDecoderString(strings.Repeat("x", 10<<20)+"\r\n", &Options{MaxLineLength: 8 << 10})
		var text string
		decoder.ExpectText(&text)
		if !errors.Is(decoder.Err(), ErrLimitExceeded) || !decoder.Fatal() {
			t.Fatalf("error = %#v, want fatal ErrLimitExceeded", decoder.Err())
		}
	})
	t.Run("nesting", func(t *testing.T) {
		input := strings.Repeat("(", 1000) + "NIL" + strings.Repeat(")", 1000)
		decoder := NewDecoderString(input, &Options{MaxListDepth: 32})
		if err := decoder.DiscardValue(); !errors.Is(err, ErrLimitExceeded) || !decoder.Fatal() {
			t.Fatalf("error = %#v, want fatal ErrLimitExceeded", err)
		}
	})
}

func TestPartiallyConsumedLiteralPoisonsDecoder(t *testing.T) {
	decoder := NewDecoderString("{5}\r\nhello next\r\n", nil)
	literal, ok := decoder.Literal()
	if !ok {
		t.Fatal(decoder.Err())
	}
	var one [1]byte
	if _, err := literal.Read(one[:]); err != nil {
		t.Fatal(err)
	}
	var atom string
	if decoder.Atom(&atom) {
		t.Fatal("decoder advanced past an undrained literal")
	}
	if !errors.Is(decoder.Err(), ErrLiteralPending) || !decoder.Fatal() {
		t.Fatalf("error = %#v, want fatal ErrLiteralPending", decoder.Err())
	}
}

func TestStalledLiteralObservesReadDeadline(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	writeDone := make(chan error, 1)
	go func() {
		_, err := io.WriteString(server, "{10}\r\nx")
		writeDone <- err
	}()

	decoder := NewDecoder(client, &Options{ReadTimeout: 50 * time.Millisecond})
	literal, ok := decoder.Literal()
	if !ok {
		t.Fatal(decoder.Err())
	}
	if _, err := io.Copy(io.Discard, literal); err == nil {
		t.Fatal("stalled literal did not time out")
	}
	if !decoder.Fatal() {
		t.Fatalf("timeout did not poison decoder: %v", decoder.Err())
	}
	select {
	case <-writeDone:
	case <-time.After(time.Second):
		t.Fatal("writer goroutine did not finish")
	}
}

func TestTwoHundredMegabyteLiteralStreamsWithFlatAllocation(t *testing.T) {
	if testing.Short() {
		t.Skip("200 MB streaming regression")
	}
	const size = int64(200 << 20)
	stream := io.MultiReader(
		strings.NewReader(fmt.Sprintf("{%d}\r\n", size)),
		io.LimitReader(repeatedByte('x'), size),
	)
	decoder := NewDecoder(stream, &Options{MaxLiteralSize: size})
	literal, ok := decoder.Literal()
	if !ok {
		t.Fatal(decoder.Err())
	}

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	buffer := make([]byte, 32<<10)
	written, err := io.CopyBuffer(io.Discard, literal, buffer)
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if written != size {
		t.Fatalf("streamed %d bytes, want %d", written, size)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 2<<20 {
		t.Fatalf("stream allocated %d bytes while copying %d", allocated, size)
	}
}

type repeatedByte byte

func (r repeatedByte) Read(dst []byte) (int, error) {
	for i := range dst {
		dst[i] = byte(r)
	}
	return len(dst), nil
}

func TestCorpusContainsHostileInputs(t *testing.T) {
	for _, path := range []string{
		"testdata/hostile/oversized-literal.imap",
		"testdata/hostile/truncated-literal.imap",
		"testdata/hostile/nul-atom.imap",
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(bytes.TrimSpace(data)) == 0 {
			t.Fatalf("empty corpus file %s", path)
		}
	}
}
