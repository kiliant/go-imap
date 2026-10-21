package imapserver

import (
	"bytes"
	"context"
	"testing"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

// TestWriteThreadListGrammar pins the RFC 5256 section 5 thread-list grammar.
// A lone reply continues its parent's number run and sibling lists are
// adjacent; "(1 (2))" and "(2 (7) (12))" are not thread-lists, and our own
// client rejected them.
func TestWriteThreadListGrammar(t *testing.T) {
	leaf := func(n uint32) imap.ThreadNode { return imap.ThreadNode{Num: n} }
	node := func(n uint32, children ...imap.ThreadNode) imap.ThreadNode {
		return imap.ThreadNode{Num: n, Children: children}
	}
	for _, tc := range []struct {
		name string
		tree imap.ThreadNode
		want string
	}{
		{"single", leaf(1), "(1)"},
		{"chain", node(1, node(2, leaf(3))), "(1 2 3)"},
		{"siblings", node(2, leaf(7), leaf(12), leaf(13)), "(2 (7)(12)(13))"},
		// The example from RFC 5256 section 4.
		{"rfc-example", node(3, node(6, node(4, leaf(23)), node(44, node(7, leaf(96))))), "(3 6 (4 23)(44 7 96))"},
		{"anonymous-root", node(0, leaf(1), leaf(2)), "((1)(2))"},
		{"anonymous-lone-child", node(0, node(5, leaf(6))), "(5 6)"},
		{"anonymous-reply", node(1, node(0, leaf(2), leaf(3))), "(1 (2)(3))"},
		{"empty-anonymous-sibling", node(1, leaf(2), node(0)), "(1 2)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			c := &conn{ctx: context.Background(), encoder: imapwire.NewEncoder(&output, &imapwire.EncoderOptions{ServerResponse: true})}
			writeThreadList(c, &tc.tree, true)
			if err := c.encoder.Flush(); err != nil {
				t.Fatal(err)
			}
			if got := output.String(); got != tc.want {
				t.Errorf("thread-list = %q, want %q", got, tc.want)
			}
		})
	}
}
