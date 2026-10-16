package imapclient

import (
	"errors"
	"fmt"
	"testing"

	"github.com/kiliant/go-imap"
)

// RFC 7162 sections 3.1.6, 3.1.9 and 7: only nonempty legacy
// SEARCH/SORT results may end in (MODSEQ <positive unsigned 63-bit integer>).
func TestLegacySearchSortModSeq(t *testing.T) {
	for _, mode := range []string{"search", "uid-search", "esearch", "uid-esearch", "sort", "uid-sort"} {
		for _, tc := range []struct {
			name, suffix string
			count        int
			valid        bool
		}{
			{"empty", "", 0, true},
			{"ordinary", " 1 3", 2, true},
			{"minimum", " 1 3 (MODSEQ 1)", 2, true},
			{"maximum", " 1 3 (MODSEQ 9223372036854775807)", 2, true},
			{"case-insensitive", " 1 3 (modseq 42)", 2, true},
			{"empty-with-suffix", " (MODSEQ 42)", 0, false},
			{"zero", " 1 (MODSEQ 0)", 0, false},
			{"overflow", " 1 (MODSEQ 9223372036854775808)", 0, false},
			{"negative", " 1 (MODSEQ -1)", 0, false},
			{"missing-value", " 1 (MODSEQ)", 0, false},
			{"missing-close", " 1 (MODSEQ 42", 0, false},
			{"extra-value", " 1 (MODSEQ 42 43)", 0, false},
			{"unknown", " 1 (OTHER 42)", 0, false},
			{"not-trailing", " 1 (MODSEQ 42) 3", 0, false},
			{"duplicate", " 1 (MODSEQ 42) (MODSEQ 43)", 0, false},
			{"zero-number", " 0 (MODSEQ 42)", 0, false},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				response := "SEARCH"
				if mode == "sort" || mode == "uid-sort" {
					response = "SORT"
				}
				c, ctx := newExtATestClient(t, "* PREAUTH [CAPABILITY IMAP4rev1 SORT CONDSTORE] ready", func(s *extAServer) {
					s.selectInbox("INBOX", "1")
					tag, _ := s.command()
					s.reply("* "+response+tc.suffix, tag+" OK done")
					if tc.valid {
						tag, _ = s.command()
						s.ok(tag)
					}
				})
				if _, err := c.Select("INBOX", nil).Wait(ctx); err != nil {
					t.Fatal(err)
				}
				var err error
				count := 0
				criteria := imap.SearchModSeq{ModSeq: 1}
				switch mode {
				case "search":
					var v []imap.SeqNum
					v, err = c.Search(criteria, nil).All(ctx)
					count = len(v)
					if err == nil && tc.count == 2 && fmt.Sprint(v) != "[1 3]" {
						t.Errorf("number results changed: %v", v)
					}
				case "uid-search":
					var v []imap.UID
					v, err = c.SearchUID(criteria, nil).AllUID(ctx)
					count = len(v)
					if err == nil && tc.count == 2 && fmt.Sprint(v) != "[1 3]" {
						t.Errorf("number results changed: %v", v)
					}
				case "esearch", "uid-esearch":
					var v *ESearchData
					opts := &ESearchOptions{ReturnOptions: []SearchReturnOption{SearchReturnCount, SearchReturnAll}}
					if mode == "esearch" {
						v, err = c.SearchExtended(criteria, opts).Wait(ctx)
					} else {
						v, err = c.SearchExtendedUID(criteria, opts).Wait(ctx)
					}
					if err == nil {
						count = int(v.Count)
						all := v.All.String()
						if mode == "uid-esearch" {
							all = v.AllUIDs.String()
						}
						if tc.count == 2 && all != "1,3" {
							t.Errorf("number results changed: %s", all)
						}
						if !v.Emulated {
							t.Error("expected emulation")
						}
						if tc.name == "maximum" && (!v.HasModSeq || v.ModSeq != 9223372036854775807) {
							t.Errorf("MODSEQ lost: %#v", v)
						}
					}
				case "sort", "uid-sort":
					var v *SortData
					keys := []SortKeySpec{{Key: SortKeyArrival}}
					if mode == "sort" {
						v, err = c.Sort(ctx, keys, criteria, nil)
					} else {
						v, err = c.SortUID(ctx, keys, criteria, nil)
					}
					if err == nil {
						count = len(v.SeqNums) + len(v.UIDs)
						all := fmt.Sprint(v.SeqNums)
						if mode == "uid-sort" {
							all = fmt.Sprint(v.UIDs)
						}
						if tc.count == 2 && all != "[1 3]" {
							t.Errorf("number results changed: %s", all)
						}
					}
				}
				if !tc.valid {
					var protocolErr *imap.Error
					if !errors.As(err, &protocolErr) {
						t.Fatalf("malformed response error = %v, want *imap.Error", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if count != tc.count {
					t.Fatalf("%s result count = %d, want %d", mode, count, tc.count)
				}
				if err := c.Noop(nil).Wait(ctx); err != nil {
					t.Fatal(fmt.Errorf("NOOP after valid suffix: %w", err))
				}
			})
		}
	}
}

// A legacy suffix describes the full match set. For MIN/MAX-only emulation it
// cannot stand in for the subset clock defined by RFC 4731 section 3.2.
func TestLegacySearchModSeqDoesNotMislabelMinMaxClock(t *testing.T) {
	for _, wanted := range [][]SearchReturnOption{{SearchReturnMin}, {SearchReturnMax}, {SearchReturnMin, SearchReturnMax}} {
		t.Run(fmt.Sprint(wanted), func(t *testing.T) {
			c, ctx := newExtATestClient(t, "* PREAUTH [CAPABILITY IMAP4rev1 CONDSTORE] ready", func(s *extAServer) {
				s.selectInbox("INBOX", "1")
				tag, _ := s.command()
				s.reply("* SEARCH 1 2 3 (MODSEQ 42)", tag+" OK done")
				tag, _ = s.command()
				s.ok(tag)
			})
			if _, err := c.Select("INBOX", nil).Wait(ctx); err != nil {
				t.Fatal(err)
			}
			data, err := c.SearchExtended(imap.SearchModSeq{ModSeq: 1}, &ESearchOptions{ReturnOptions: wanted}).Wait(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if data.HasModSeq || data.Values[ESearchReturnKeyModSeq] != "" {
				t.Errorf("full-set clock mislabeled as MIN/MAX subset clock: %#v", data)
			}
			if data.HasMin && data.Min != 1 || data.HasMax && data.Max != 3 {
				t.Errorf("number results changed: %#v", data)
			}
			if err := c.Noop(nil).Wait(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}
