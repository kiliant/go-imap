package imapcodec

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapwire"
)

func TestContextUpdateChangesRoundTrip(t *testing.T) {
	for _, uid := range []bool{false, true} {
		wire := "(0 9,4:2 1 4294967295 1 7)"
		want, err := ReadContextUpdateChanges(imapwire.NewDecoderString(wire, nil), uid)
		if err != nil {
			t.Fatal(err)
		}
		if len(want) != 3 || want[0].Position != 0 || want[1].Position != 1 || want[2].Position != 1 {
			t.Fatalf("pairs = %#v", want)
		}
		var out bytes.Buffer
		enc := imapwire.NewEncoder(&out, &imapwire.EncoderOptions{ServerResponse: true})
		if err := WriteContextUpdateChanges(enc, uid, want); err != nil {
			t.Fatal(err)
		}
		if err := enc.Flush(); err != nil {
			t.Fatal(err)
		}
		if out.String() != wire {
			t.Fatalf("order/ranges changed: %q", out.String())
		}
		got, err := ReadContextUpdateChanges(imapwire.NewDecoderString(out.String(), nil), uid)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("round trip = %#v, %v", got, err)
		}
	}
}

func TestContextUpdateChangesRejectMalformed(t *testing.T) {
	for _, wire := range []string{"", "()", "(0)", "(0 0)", "(-1 1)", "(4294967296 1)", "(0 1 2)", "(0 (1))", "(0 NIL)", "(0 1:4294967296)"} {
		if _, err := ReadContextUpdateChanges(imapwire.NewDecoderString(wire, nil), false); err == nil {
			t.Errorf("accepted %q", wire)
		}
	}
	var out bytes.Buffer
	enc := imapwire.NewEncoder(&out, nil)
	if err := WriteContextUpdateChanges(enc, true, []imap.ContextUpdateChange{{SeqNums: imap.SeqSetNum(1)}}); err == nil {
		t.Fatal("mixed number modes accepted")
	}
}

// context-position SP sequence-set admits "*", which is not an atom character.
func TestContextUpdateChangesWildcard(t *testing.T) {
	for _, wire := range []string{"(0 1:*)", "(3 *,7)"} {
		changes, err := ReadContextUpdateChanges(imapwire.NewDecoderString(wire, nil), true)
		if err != nil {
			t.Fatalf("%q: %v", wire, err)
		}
		var out bytes.Buffer
		enc := imapwire.NewEncoder(&out, &imapwire.EncoderOptions{ServerResponse: true})
		if err := WriteContextUpdateChanges(enc, true, changes); err != nil {
			t.Fatal(err)
		}
		if err := enc.Flush(); err != nil {
			t.Fatal(err)
		}
		if out.String() != wire {
			t.Fatalf("round trip = %q, want %q", out.String(), wire)
		}
	}
}
