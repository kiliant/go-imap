package harness

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kiliant/go-imap/interop/harness/adversarial"
)

func TestRawSessionRejectsHostileResponses(t *testing.T) {
	wanted := map[string]bool{
		"unknown-tag":       true,
		"bye-mid-command":   true,
		"ten-megabyte-line": true,
		"cr-without-lf":     true,
		"stalled-literal":   true,
	}
	for _, scenario := range adversarial.Cases() {
		if !wanted[scenario.Name] {
			continue
		}
		scenario := scenario
		t.Run(scenario.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
			defer cancel()
			client, server := adversarial.Pipe(ctx, scenario)
			defer server.Close()
			session, err := openSession(ctx, client, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			if err := session.Noop(ctx); err == nil {
				t.Fatal("hostile response was accepted")
			}
		})
	}
}

func TestWireTraceRedactsCredentials(t *testing.T) {
	trace := new(Trace)
	trace.add("C:", "A001 LOGIN \"alice@example.test\" \"hunter2\"\r\n")
	trace.add("C:", "A002 AUTHENTICATE PLAIN AGFsaWNlAGh1bnRlcjI=\r\n")
	got := trace.String()
	for _, secret := range []string{"alice@example.test", "hunter2", "AGFsaWNlAGh1bnRlcjI="} {
		if strings.Contains(got, secret) {
			t.Fatalf("trace leaked %q: %s", secret, got)
		}
	}
	if count := strings.Count(got, "<redacted>"); count < 3 {
		t.Fatalf("trace does not identify redaction: %q", got)
	}
}

func TestBoundedLineRejectsBeforeGrowingPastLimit(t *testing.T) {
	const limit = 4096
	line, err := readBoundedLine(bytes.NewReader(bytes.Repeat([]byte{'x'}, 10<<20)), limit)
	if err == nil {
		t.Fatal("oversized line accepted")
	}
	if len(line) > limit {
		t.Fatalf("returned %d bytes with limit %d", len(line), limit)
	}
}
