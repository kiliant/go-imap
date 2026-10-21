package imapdeflate

import (
	"bytes"
	"compress/flate"
	"io"
	"testing"
)

func FuzzCompressionBoundary(f *testing.F) {
	f.Add([]byte("U UNAUTHENTICATE"), uint8(0))
	f.Add(bytes.Repeat([]byte("x"), 32766), uint8(1))
	f.Add(bytes.Repeat([]byte("y"), 65534), uint8(2))
	f.Fuzz(func(t *testing.T, payload []byte, mode uint8) {
		if len(payload) > 128<<10 {
			t.Skip()
		}
		plain := append(bytes.Clone(payload), '\r', '\n')
		var wire bytes.Buffer
		writer, err := flate.NewWriter(&wire, flate.DefaultCompression)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(plain); err != nil {
			t.Fatal(err)
		}
		if mode%3 != 2 {
			if err := writer.Flush(); err != nil {
				t.Fatal(err)
			}
		}
		if mode%3 != 0 {
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
		}
		wire.WriteString("A NOOP\r\n")
		reader := NewReader(&wire)
		defer reader.Close()
		decoded := make([]byte, len(plain))
		if _, err := io.ReadFull(reader, decoded); err != nil || !bytes.Equal(decoded, plain) {
			t.Fatalf("compressed content changed: %v", err)
		}
		if err := reader.Finish(); err != nil {
			t.Fatal(err)
		}
		next, err := io.ReadAll(reader.PlainReader())
		if err != nil || string(next) != "A NOOP\r\n" {
			t.Fatalf("next-layer content changed: %q, %v", next, err)
		}
	})
}
