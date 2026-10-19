package imapserver

import (
	"reflect"
	"strings"
	"testing"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

func TestParseSingleFetchItems(t *testing.T) {
	for _, test := range []struct {
		command      string
		item         imap.FetchItem
		changedSince uint64
	}{
		{command: " 1 UID\r\n", item: imap.FetchItemUID},
		{command: " 1 FLAGS\r\n", item: imap.FetchItemFlags},
		{command: " 1 RFC822.SIZE\r\n", item: imap.FetchItemRFC822Size},
		{command: " 1 BODY.PEEK[1.HEADER.FIELDS (SUBJECT FROM)]<0.8>\r\n", item: &imap.FetchItemBodySection{Peek: true, Part: []int{1}, Specifier: imap.PartSpecifierHeader, HeaderFields: []string{"SUBJECT", "FROM"}, Partial: &imap.SectionPartial{Offset: 0, Size: 8}}},
		{command: " 1 MODSEQ (CHANGEDSINCE 7)\r\n", item: imap.FetchItemModSeq, changedSince: 7},
		{command: " 1 PREVIEW (LAZY)\r\n", item: &imap.FetchItemPreview{Lazy: true}},
	} {
		t.Run(strings.TrimSpace(test.command), func(t *testing.T) {
			value, _, err := parseFetch(imapwire.NewDecoder(strings.NewReader(test.command), nil))
			if err != nil {
				t.Fatal(err)
			}
			args := value.(*fetchArgs)
			if !reflect.DeepEqual(args.items, []imap.FetchItem{test.item}) || args.changedSince != test.changedSince {
				t.Fatalf("parsed %#v, changedSince=%d", args.items, args.changedSince)
			}
		})
	}
	for _, command := range []string{" 1 UID FLAGS\r\n", " 1 BODY.PEEK\r\n", " 1 BODY[\r\n"} {
		if _, _, err := parseFetch(imapwire.NewDecoder(strings.NewReader(command), nil)); err == nil {
			t.Errorf("malformed standalone item accepted: %q", command)
		}
	}
}
