package imapcodec

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

func TestSearchStringArgumentsAcceptAstrings(t *testing.T) {
	for _, key := range []string{"BCC", "BODY", "CC", "FROM", "SUBJECT", "TEXT", "TO", "HEADER"} {
		for _, form := range []struct{ name, wire, value string }{
			{"atom", "needle", "needle"},
			{"astring-special", "needle]", "needle]"},
			{"quoted", `"two words"`, "two words"},
			{"escaped", `"a\"b\\c"`, "a\"b\\c"},
			{"literal", "{9}\r\ntwo words", "two words"},
			{"literal-plus", "{9+}\r\ntwo words", "two words"},
			{"empty-quoted", `""`, ""},
			{"empty-literal", "{0}\r\n", ""},
		} {
			t.Run(key+"/"+form.name, func(t *testing.T) {
				prefix := key + " "
				var want imap.SearchCriteria = imap.SearchString{Key: imap.SearchStringKey(key), Value: form.value}
				if key == "HEADER" {
					prefix += "Subject "
					want = imap.SearchHeaderField{Field: "Subject", Value: form.value}
				}
				// A following key detects accidental consumption beyond the astring.
				dec := imapwire.NewDecoderString(prefix+form.wire+" SEEN\r\n", nil)
				got, err := ReadSearchCriteria(dec)
				if err != nil {
					t.Fatal(err)
				}
				if expected := (imap.SearchAnd{want, imap.SearchSeen}); !reflect.DeepEqual(got, expected) {
					t.Fatalf("criteria = %#v, want %#v", got, expected)
				}
				if !dec.ExpectCRLF() {
					t.Fatal(dec.Err())
				}
			})
		}
	}
}

func TestSearchHeaderFieldAcceptsAstrings(t *testing.T) {
	for _, field := range []string{"Subject", `"Subject"`, "{7}\r\nSubject"} {
		t.Run(fmt.Sprintf("%q", field), func(t *testing.T) {
			got, err := ReadSearchCriteria(imapwire.NewDecoderString("HEADER "+field+" needle\r\n", nil))
			want := imap.SearchHeaderField{Field: "Subject", Value: "needle"}
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("criteria = %#v, %v; want %#v", got, err, want)
			}
		})
	}
}
