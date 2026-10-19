package imapmessage

import (
	"bytes"
	"io"
	"testing"

	"github.com/kiliant/go-imap"
)

func TestNestedMessageAndDigestSections(t *testing.T) {
	innerHeader := "From: nested@example.test\r\nSubject: nested\r\n\r\n"
	inner := innerHeader + "body\r\n"
	outerHeader := "From: outer@example.test\r\nContent-Type: message/rfc822\r\n\r\n"
	digestHeader := "Content-Type: multipart/digest; boundary=b\r\n\r\n"
	digest := digestHeader + "--b\r\nX-Mime: wrapper\r\n\r\n" + inner + "\r\n--b--\r\n"
	for _, tc := range []struct {
		name, raw string
		part      []int
		spec      imap.PartSpecifier
		want      string
	}{
		{"outer header", outerHeader + inner, nil, imap.PartSpecifierHeader, outerHeader},
		{"outer text", outerHeader + inner, nil, imap.PartSpecifierText, inner},
		{"implicit message", outerHeader + inner, []int{1}, imap.PartSpecifierNone, inner},
		{"encapsulated text", outerHeader + inner, []int{1, 1}, imap.PartSpecifierNone, "body\r\n"},
		{"nested header", outerHeader + inner, []int{1}, imap.PartSpecifierHeader, innerHeader},
		{"nested text", outerHeader + inner, []int{1}, imap.PartSpecifierText, "body\r\n"},
		{"digest mime", digest, []int{1}, imap.PartSpecifierMIME, "X-Mime: wrapper\r\n\r\n"},
		{"digest header", digest, []int{1}, imap.PartSpecifierHeader, innerHeader},
		{"digest text", digest, []int{1}, imap.PartSpecifierText, "body\r\n"},
		{"nested digest header", outerHeader + digest, []int{1, 1}, imap.PartSpecifierHeader, innerHeader},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := Analyze(bytes.NewReader([]byte(tc.raw)), int64(len(tc.raw)))
			if err != nil {
				t.Fatal(err)
			}
			r, n, err := parsed.OpenBodySection(&imap.FetchItemBodySection{Part: tc.part, Specifier: tc.spec})
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(r)
			if err != nil || string(got) != tc.want || n != int64(len(tc.want)) {
				t.Fatalf("section = %q (%d), want %q: %v", got, n, tc.want, err)
			}
		})
	}
}

func TestObsoleteAddressesPreserveRoutesAndSearchMailbox(t *testing.T) {
	for _, raw := range []string{"<@relay:user@example.test>", "Name <@one,@two:user@example.test>", "<user (comment)@ (comment) example.test>"} {
		t.Run(raw, func(t *testing.T) {
			source := []byte("From: " + raw + "\r\n\r\nbody\r\n")
			parsed, err := Analyze(bytes.NewReader(source), int64(len(source)))
			if err != nil {
				t.Fatal(err)
			}
			addresses := parsed.Envelope.From
			if len(addresses) != 1 || addresses[0].Mailbox != "user" || addresses[0].Host != "example.test" {
				t.Fatalf("addresses = %#v", addresses)
			}
			if raw == "<@relay:user@example.test>" && addresses[0].SourceRoute != "@relay" {
				t.Fatalf("lost source route: %#v", addresses)
			}
			matches, err := Match(parsed, Metadata{}, imap.SearchString{Key: imap.SearchKeyFrom, Value: "user@example.test"}, nil)
			if err != nil || !matches {
				t.Fatalf("address search = %v, %v", matches, err)
			}
		})
	}
}

func TestFlagModSeqUsesGlobalClockUnlessEntryClocksProvided(t *testing.T) {
	parsed, err := Analyze(bytes.NewReader(nil), 0)
	if err != nil {
		t.Fatal(err)
	}
	criterion := imap.SearchModSeq{EntryName: "/flags/\\seen", EntryType: imap.SearchModSeqMetadataAll, ModSeq: 7}
	for _, tc := range []struct {
		metadata Metadata
		want     bool
	}{
		{Metadata{ModSeq: 7}, true}, {Metadata{ModSeq: 6}, false},
		{Metadata{ModSeq: 9, PrivateModSeqs: map[string]uint64{}, SharedModSeqs: map[string]uint64{}}, false},
	} {
		got, err := Match(parsed, tc.metadata, criterion, nil)
		if err != nil || got != tc.want {
			t.Fatalf("Match(%#v) = %v,%v", tc.metadata, got, err)
		}
	}
}
