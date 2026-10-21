package imapdeflate

import (
	"bytes"
	"compress/flate"
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestCompressionBoundary(t *testing.T) {
	for _, level := range []int{flate.NoCompression, flate.BestSpeed, flate.DefaultCompression, flate.HuffmanOnly} {
		for _, size := range []int{32767, 32768, 32769, 65536} {
			for _, ending := range []string{"flush", "flush-close", "close"} {
				t.Run(fmt.Sprintf("%d/%d/%s", level, size, ending), func(t *testing.T) {
					plain := strings.Repeat("P NOOP\r\n", size/8) + strings.Repeat("x", size%8)
					var encoded bytes.Buffer
					w, err := flate.NewWriter(&encoded, level)
					if err != nil {
						t.Fatal(err)
					}
					_, _ = io.WriteString(w, plain)
					if ending != "close" {
						_ = w.Flush()
					}
					if ending != "flush" {
						_ = w.Close()
					}
					encoded.WriteString("A LOGIN user password\r\n")
					r := NewReader(&encoded)
					decoded := make([]byte, len(plain))
					if _, err := io.ReadFull(r, decoded); err != nil {
						t.Fatal(err)
					}
					if string(decoded) != plain {
						t.Fatal("changed compressed data")
					}
					if err := r.Finish(); err != nil {
						t.Fatal(err)
					}
					rest, err := io.ReadAll(r.PlainReader())
					if err != nil || string(rest) != "A LOGIN user password\r\n" {
						t.Fatalf("lost next layer: %q, %v", rest, err)
					}
				})
			}
		}
	}
}
