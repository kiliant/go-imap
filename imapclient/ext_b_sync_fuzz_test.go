package imapclient

import (
	"testing"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// A hostile or merely broken server must not be able to panic the client
// through the responses this extension group introduces. An error is a fine
// outcome; a panic on the reader goroutine is not recoverable by the caller.

// FuzzReadVanished covers the VANISHED response of RFC 7162 section 3.2.10,
// including the (EARLIER) modifier and the sequence-set argument.
func FuzzReadVanished(f *testing.F) {
	f.Add(" (EARLIER) 41,43:116,118,120:211,214:540\r\n")
	f.Add(" 505,507,510,625\r\n")
	f.Add(" (EARLIER) 1\r\n")
	f.Add(" (EARLIER)\r\n")
	f.Add(" (LATER) 1\r\n")
	f.Add(" 1:*\r\n")
	f.Add(" (EARLIER 1\r\n")
	f.Add("")
	f.Add(" 0\r\n")
	f.Add(" ,,,,\r\n")
	f.Add(" 4294967296\r\n")
	f.Fuzz(func(t *testing.T, s string) {
		dec := imapwire.NewDecoderString(s, nil)
		data, err := readVanished(dec)
		if err == nil && data.UIDs.IsEmpty() {
			t.Fatalf("readVanished(%q) returned an empty set and no error", s)
		}
	})
}

// FuzzReadSyncFetch covers the buffered FETCH parser used while collecting a
// QRESYNC resynchronisation. Its contract is that any input either parses or
// errors, and never blocks or panics — in particular an unexpected literal must
// be consumed rather than left for a reader that will never come.
func FuzzReadSyncFetch(f *testing.F) {
	f.Add(" (UID 117 FLAGS (\\Seen \\Answered) MODSEQ (90060115194045001))\r\n")
	f.Add(" (MODSEQ (0))\r\n")
	f.Add(" (MODSEQ (9223372036854775807))\r\n")
	f.Add(" (MODSEQ (9223372036854775808))\r\n")
	f.Add(" (UID 0)\r\n")
	f.Add(" (X-THING {4}\r\nabcd)\r\n")
	f.Add(" (BODY[HEADER] {2}\r\nhi)\r\n")
	f.Add(" (FLAGS ()\r\n")
	f.Add(" ()\r\n")
	f.Add("(")
	f.Fuzz(func(t *testing.T, s string) {
		dec := imapwire.NewDecoderString(s, nil)
		resp := &untaggedResponse{name: "FETCH", number: 1, hasNum: true, dec: dec}
		data, err := readSyncFetch(resp)
		if err == nil && data == nil {
			t.Fatalf("readSyncFetch(%q) returned no data and no error", s)
		}
	})
}

// FuzzParseObjectID covers the OBJECTID identifier production of RFC 8474
// section 7, which arrives inside a response code and is therefore attacker
// controlled before any length check the decoder applies.
func FuzzParseObjectID(f *testing.F) {
	f.Add("(F2212ea87-6097-4256-9d51-71338625)")
	f.Add("M6d99ac3275bb4e")
	f.Add("()")
	f.Add("(")
	f.Add(")")
	f.Add("(a b)")
	f.Fuzz(func(t *testing.T, s string) {
		id, err := parseObjectID(s)
		if err == nil && id == "" {
			t.Fatalf("parseObjectID(%q) returned an empty identifier and no error", s)
		}
	})
}

// FuzzParseModifiedSet covers the MODIFIED response-code argument of RFC 7162
// section 3.1.3 in both address spaces.
func FuzzParseModifiedSet(f *testing.F) {
	f.Add("7,9")
	f.Add("101,110:111")
	f.Add("12")
	f.Add("1:*")
	f.Add(":")
	f.Add("")
	f.Fuzz(func(t *testing.T, s string) {
		for _, uid := range []bool{false, true} {
			data := &SyncStoreData{}
			if err := data.parseModified(s, uid); err != nil {
				continue
			}
			if uid {
				_ = data.ModifiedUIDs.String()
			} else {
				_ = data.ModifiedSeqNums.String()
			}
		}
	})
}

// FuzzParseReplaceAppendUID covers the APPENDUID response-code argument that
// REPLACE reports the new message's UID with (RFC 8508 section 4.3, RFC 4315
// section 3). A malformed code must be reported, never turned into a plausible
// but wrong UID: a caller storing that UID in a cache would address a different
// message from then on.
func FuzzParseReplaceAppendUID(f *testing.F) {
	f.Add("1 2001")
	f.Add("1521475658 1")
	f.Add("0 5")
	f.Add("1 0")
	f.Add("1")
	f.Add("1 2 3")
	f.Add("")
	f.Add("  1   2001  ")
	f.Add("4294967296 1")
	f.Add("-1 -1")
	f.Fuzz(func(t *testing.T, s string) {
		validity, uid, err := parseReplaceAppendUID(s)
		if err != nil {
			return
		}
		if validity == 0 || uid == 0 {
			t.Fatalf("parseReplaceAppendUID(%q) accepted a zero identifier: (%d, %d)", s, validity, uid)
		}
	})
}

// FuzzWriteNumSet checks that the sequence-set encoder never writes anything the
// decoder cannot read back, which is what keeps a malformed set from
// desynchronising a command line.
func FuzzWriteNumSet(f *testing.F) {
	f.Add("1:*")
	f.Add("41,43:116")
	f.Add("*")
	f.Add("*:*")
	f.Add("1,")
	f.Add("")
	f.Add("1:2:3")
	f.Add("a")
	f.Fuzz(func(t *testing.T, s string) {
		if !numSetSyntax(s) {
			return
		}
		if _, err := imap.ParseSeqSet(s); err != nil {
			t.Fatalf("numSetSyntax accepted %q but ParseSeqSet rejected it: %v", s, err)
		}
	})
}
