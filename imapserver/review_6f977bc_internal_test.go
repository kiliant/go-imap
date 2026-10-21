package imapserver

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

func TestReview6fLowerCaseFlagsAnnouncement(t *testing.T) {
	for _, uidOnly := range []bool{false, true} {
		var wire bytes.Buffer
		c := &conn{state: newSessionState(false), encoder: imapwire.NewEncoder(&wire, &imapwire.EncoderOptions{ServerResponse: true})}
		c.state.selected = &selectedState{flags: []imap.Flag{imap.FlagSeen}}
		if uidOnly {
			c.state.enable("UIDONLY")
		}
		data := &imap.FetchMessageData{SeqNum: 1, Items: map[imap.FetchDataKey][]imap.FetchData{"uId": {imap.FetchDataUID(1)}, "flags": {imap.FetchDataFlags{"$fresh"}}}}
		if err := writeFetchLikeResponse(c, data); err != nil {
			t.Fatal(err)
		}
		if err := c.encoder.Flush(); err != nil {
			t.Fatal(err)
		}
		output := wire.String()
		first, fetch := strings.Index(output, "* FLAGS "), strings.Index(output, " FETCH ")
		if uidOnly {
			fetch = strings.Index(output, " UIDFETCH ")
			if strings.Contains(output, "uId ") {
				t.Fatal(output)
			}
		}
		if first < 0 || fetch < first || !strings.Contains(output[:fetch], "$fresh") {
			t.Fatalf("keyword announcement order: %q", output)
		}
	}
}

func TestReview6fAmbiguousFetchUIDRejected(t *testing.T) {
	for _, other := range []imap.FetchDataUID{1, 2} {
		data := &imap.FetchMessageData{Items: map[imap.FetchDataKey][]imap.FetchData{"UID": {imap.FetchDataUID(1)}, "uid": {other}}}
		for i := 0; i < 30; i++ {
			if _, err := mapFetchResponse(&selectedState{uids: []imap.UID{1, 2}}, data, true); err == nil {
				t.Fatal("accepted multiple UID values")
			}
		}
	}
}

func TestEverySortKeyHasCapabilityClassification(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "../searchdata.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	ast.Inspect(file, func(node ast.Node) bool {
		declaration, ok := node.(*ast.ValueSpec)
		if !ok {
			return true
		}
		typeName, ok := declaration.Type.(*ast.Ident)
		if !ok || typeName.Name != "SortKey" {
			return true
		}
		for _, value := range declaration.Values {
			literal, ok := value.(*ast.BasicLit)
			if !ok {
				t.Fatal("unhandled SortKey constant declaration")
			}
			name, err := strconv.Unquote(literal.Value)
			if err != nil {
				t.Fatal(err)
			}
			if gate, ok := sortKeyGates[imap.SortKey(name)]; !ok || gate == nil {
				t.Errorf("SortKey %s has no gate", name)
			}
			count++
		}
		return true
	})
	if count != len(sortKeyGates) {
		t.Fatalf("classified %d keys for %d root declarations", len(sortKeyGates), count)
	}
}
