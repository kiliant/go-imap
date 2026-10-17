package imapserver_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestMetadataEntryNames(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, conn, reader := newGroupARawSession(t, ctx)
	names := []struct {
		name  string
		valid bool
	}{
		{"/private/comment", true}, {"/SHARED/comment", true},
		{"/private/a\x1ab", true}, {"/private/a\x7fb", true},
		{"/private/a*b", false}, {"/private/a%b", false},
		{"/private/café", false}, {"/private/a\x80b", false},
		{"/private//comment", false}, {"/private/comment/", false},
		{"", false}, {"private/comment", false},
	}
	for b := byte(1); b <= 0x19; b++ {
		names = append(names, struct {
			name  string
			valid bool
		}{"/private/a" + string(b) + "b", false})
	}
	for i, tc := range names {
		t.Run(fmt.Sprintf("%02d", i), func(t *testing.T) {
			// Literal names exercise every forbidden byte, including line endings,
			// without accidentally changing the surrounding command syntax.
			entry := fmt.Sprintf("{%d+}\r\n%s", len(tc.name), tc.name)
			commands := []string{
				"SETMETADATA INBOX (/private/untouched \"original\")",
				"GETMETADATA INBOX " + entry,
				"GETMETADATA INBOX (/private/untouched " + entry + ")",
				"SETMETADATA INBOX (/private/untouched \"changed\" " + entry + " \"value\")",
			}
			for j, command := range commands {
				tag := fmt.Sprintf("m%d_%d", i, j)
				writeRawCommand(t, conn, tag+" "+command+"\r\n")
				_, tagged := collectUntilTag(t, reader, tag+" ")
				want := tag + " BAD"
				if tc.valid || j == 0 {
					want = tag + " OK"
				}
				if !strings.HasPrefix(tagged, want) {
					t.Errorf("%q: response %q, want %s", command, tagged, want)
				}
			}
			if !tc.valid {
				writeRawCommand(t, conn, "g GETMETADATA INBOX /private/untouched\r\n")
				lines, tagged := collectUntilTag(t, reader, "g ")
				if !strings.HasPrefix(tagged, "g OK") || !strings.Contains(strings.Join(lines, " "), "original") {
					t.Errorf("invalid name reached backend and changed another entry: %v %q", lines, tagged)
				}
			}
			writeRawCommand(t, conn, "n NOOP\r\n")
			lines, tagged := collectUntilTag(t, reader, "n ")
			if !strings.HasPrefix(tagged, "n OK") {
				t.Errorf("NOOP = %q", tagged)
			}
			for _, line := range lines {
				if strings.Contains(line, " BAD") {
					t.Errorf("stray BAD: %q", line)
				}
			}
		})
	}
}
