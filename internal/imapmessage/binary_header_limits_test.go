package imapmessage

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/kiliant/go-imap"
)

func TestEncodeNULPartsPreservesUnretainedHeaders(t *testing.T) {
	contentType := "Content-Type: application/octet-stream\r\n"
	fillFields := strings.Repeat("X-Fill: v\r\n", maxHeaderFields-2)
	fillBytes := strings.Repeat("X-Fill: "+strings.Repeat("v", maxHeaderLineKeep-64)+"\r\n", 9)
	keptTail := "X-Unretained: MUST KEEP\r\n \t\r\n"
	cte := "Content-Transfer-Encoding: binary\r\n"
	for _, tc := range []struct{ name, before, after string }{
		{"field-limit", contentType + fillFields + cte, keptTail},
		{"late-cte", contentType + fillFields + "X-Fill: last\r\n", cte + keptTail},
		{"byte-limit", contentType + cte + fillBytes, keptTail},
		{"late-cte-byte-limit", contentType + fillBytes, cte + keptTail},
		{"duplicate-folded-cte", contentType + cte + " \t\r\n" + fillFields, keptTail + cte + "\t\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := "a\x00b\r\n\xff"
			raw := tc.before + tc.after + "\r\n" + payload
			original, err := Analyze(strings.NewReader(raw), int64(len(raw)))
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			changed, err := original.EncodeNULParts(&out)
			if err != nil || !changed {
				t.Fatalf("rewrite = %v, %v", changed, err)
			}
			wantHeaders := strings.ReplaceAll(tc.before+tc.after, cte+" \t\r\n", "")
			wantHeaders = strings.ReplaceAll(wantHeaders, cte+"\t\r\n", "")
			wantHeaders = strings.ReplaceAll(wantHeaders, cte, "")
			wantHeaders += "Content-Transfer-Encoding: base64\r\n\r\n"
			if !bytes.HasPrefix(out.Bytes(), []byte(wantHeaders)) {
				t.Fatal("rewrite changed a non-CTE header or retained a stale CTE")
			}
			stored, err := Analyze(bytes.NewReader(out.Bytes()), int64(out.Len()))
			if err != nil {
				t.Fatal(err)
			}
			reader, size, err := stored.OpenBinarySection(&imap.FetchItemBinarySection{Part: []int{1}})
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := io.ReadAll(reader)
			if err != nil || string(decoded) != payload || size != int64(len(payload)) {
				t.Fatalf("BINARY round trip = %q (%d), %v", decoded, size, err)
			}
		})
	}
}
