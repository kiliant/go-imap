package imap

import (
	"errors"
	"slices"
	"testing"
)

func TestNumKindString(t *testing.T) {
	for _, tc := range []struct {
		kind NumKind
		want string
	}{
		{NumKindUnknown, "unknown"},
		{NumKindSeq, "seq"},
		{NumKindUID, "uid"},
		{NumKind(42), "unknown"},
	} {
		if got := tc.kind.String(); got != tc.want {
			t.Errorf("NumKind(%d).String() = %q, want %q", tc.kind, got, tc.want)
		}
	}
}

func TestNumSetKind(t *testing.T) {
	if got := SeqSetNum(1).Kind(); got != NumKindSeq {
		t.Errorf("SeqSet.Kind() = %v, want %v", got, NumKindSeq)
	}
	if got := UIDSetNum(1).Kind(); got != NumKindUID {
		t.Errorf("UIDSet.Kind() = %v, want %v", got, NumKindUID)
	}
	// The distinction is in the type system, not in a field: a SeqSet and a
	// UIDSet are different types and this is what refuses to compile if the
	// two are ever confused.
	var _ SeqSet = SeqSetNum(1)
	var _ UIDSet = UIDSetNum(1)
}

func TestParseSeqSet(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want SeqSet
	}{
		{"single", "1", SeqSet{{1, 1}}},
		{"range", "1:5", SeqSet{{1, 5}}},
		{"open range", "5:*", SeqSet{{5, 0}}},
		{"reversed open range", "*:5", SeqSet{{0, 5}}},
		{"bare star", "*", SeqSet{{0, 0}}},
		{"star range", "*:*", SeqSet{{0, 0}}},
		{"list", "1,3,5", SeqSet{{1, 1}, {3, 3}, {5, 5}}},
		{"mixed", "1:5,7,9:*", SeqSet{{1, 5}, {7, 7}, {9, 0}}},
		{"reversed", "5:1", SeqSet{{5, 1}}},
		{"max", "4294967295", SeqSet{{4294967295, 4294967295}}},
		{"unsorted", "9,1:3", SeqSet{{9, 9}, {1, 3}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseSeqSet(tc.in)
			if err != nil {
				t.Fatalf("ParseSeqSet(%q) = error %v", tc.in, err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("ParseSeqSet(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseSeqSetErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"zero", "0"},
		{"zero in range", "1:0"},
		{"zero start", "0:5"},
		{"letters", "abc"},
		{"empty element", "1,,2"},
		{"trailing comma", "1,"},
		{"leading comma", ",1"},
		{"double colon", "1:2:3"},
		{"missing stop", "1:"},
		{"missing start", ":1"},
		{"overflow", "4294967296"},
		{"negative", "-1"},
		{"space", "1 2"},
		{"plus", "+1"},
		{"only colon", ":"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseSeqSet(tc.in)
			if err == nil {
				t.Fatalf("ParseSeqSet(%q) = nil error, want an error", tc.in)
			}
			var ierr *Error
			if !errors.As(err, &ierr) {
				t.Fatalf("ParseSeqSet(%q) error is %T, want *imap.Error", tc.in, err)
			}
			if ierr.Type != ErrorTypeProtocol {
				t.Errorf("error type = %q, want %q", ierr.Type, ErrorTypeProtocol)
			}
		})
	}
}

func TestParseUIDSet(t *testing.T) {
	got, err := ParseUIDSet("1:*")
	if err != nil {
		t.Fatalf("ParseUIDSet: %v", err)
	}
	if want := (UIDSet{{1, 0}}); !slices.Equal(got, want) {
		t.Errorf("ParseUIDSet = %v, want %v", got, want)
	}
}

func TestNumSetString(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  SeqSet
		want string
	}{
		{"empty", nil, ""},
		{"single", SeqSet{{3, 3}}, "3"},
		{"range", SeqSet{{1, 5}}, "1:5"},
		{"open", SeqSet{{5, 0}}, "5:*"},
		{"reversed open", SeqSet{{0, 5}}, "*:5"},
		{"star", SeqSet{{0, 0}}, "*"},
		{"mixed", SeqSet{{1, 5}, {7, 7}, {9, 0}}, "1:5,7,9:*"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.set.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNumSetNormalized(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"empty", "1", "1"},
		{"sorts", "9,1:3", "1:3,9"},
		{"coalesces adjacent", "1:3,4:6", "1:6"},
		{"coalesces overlapping", "1:5,4:8", "1:8"},
		{"coalesces singletons", "1,2,3", "1:3"},
		{"leaves gap", "1,3", "1,3"},
		{"reverses range", "5:1", "1:5"},
		// RFC 3501 section 9: "*:4" and "4:*" denote the same set.
		{"star first", "*:4", "4:*"},
		{"star last", "4:*", "4:*"},
		{"star swallows", "1:*,5:10", "1:*"},
		{"star absorbed", "5:*,7", "5:*"},
		{"bare star kept", "1,*", "1,*"},
		{"bare star merged", "1:*,*", "1:*"},
		{"duplicate", "1,1,1", "1"},
		// The largest representable number and "*" coalesce; see the
		// documented caveat on Normalized.
		{"max meets star", "4294967295,*", "*"},
		{"max range", "4294967290:4294967295", "4294967290:4294967295"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in, err := ParseSeqSet(tc.in)
			if err != nil {
				t.Fatalf("ParseSeqSet(%q): %v", tc.in, err)
			}
			if got := in.Normalized().String(); got != tc.want {
				t.Errorf("ParseSeqSet(%q).Normalized() = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
	if got := SeqSet(nil).Normalized(); got != nil {
		t.Errorf("nil.Normalized() = %v, want nil", got)
	}
}

func TestNumSetContains(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  string
		num  SeqNum
		want bool
	}{
		{"in single", "3", 3, true},
		{"not in single", "3", 4, false},
		{"in range low", "2:5", 2, true},
		{"in range high", "2:5", 5, true},
		{"below range", "2:5", 1, false},
		{"above range", "2:5", 6, false},
		{"in open range", "5:*", 7, true},
		{"far in open range", "5:*", 4294967295, true},
		{"below open range", "5:*", 4, false},
		{"in reversed open range", "*:5", 7, true},
		{"star is the largest", "*", 4294967295, true},
		{"star is not one", "*", 1, false},
		{"zero never", "1:*", 0, false},
		{"second range", "1:3,7:9", 8, true},
		{"between ranges", "1:3,7:9", 5, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := ParseSeqSet(tc.set)
			if err != nil {
				t.Fatalf("ParseSeqSet(%q): %v", tc.set, err)
			}
			if got := s.Contains(tc.num); got != tc.want {
				t.Errorf("%q.Contains(%d) = %v, want %v", tc.set, tc.num, got, tc.want)
			}
		})
	}
}

func TestNumSetDynamic(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"1", false},
		{"1:5", false},
		{"1:*", true},
		{"*", true},
		{"*:5", true},
		{"1:3,9:*", true},
		{"4294967295", false},
	} {
		s, err := ParseSeqSet(tc.in)
		if err != nil {
			t.Fatalf("ParseSeqSet(%q): %v", tc.in, err)
		}
		if got := s.Dynamic(); got != tc.want {
			t.Errorf("%q.Dynamic() = %v, want %v", tc.in, got, tc.want)
		}
	}
	if !SeqSet(nil).IsEmpty() {
		t.Error("nil set is not empty")
	}
}

func TestNumSetIteration(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      string
		want    []SeqNum
		exact   bool
		partial []SeqNum // what All yields, when different from want
	}{
		{name: "single", in: "3", want: []SeqNum{3}, exact: true},
		{name: "range", in: "1:4", want: []SeqNum{1, 2, 3, 4}, exact: true},
		{name: "list", in: "1:3,7", want: []SeqNum{1, 2, 3, 7}, exact: true},
		{name: "unsorted input", in: "7,1:3", want: []SeqNum{1, 2, 3, 7}, exact: true},
		{name: "overlapping", in: "1:3,2:4", want: []SeqNum{1, 2, 3, 4}, exact: true},
		{name: "dynamic", in: "1:*", exact: false, partial: nil},
		{name: "partly dynamic", in: "1:3,9:*", exact: false, partial: []SeqNum{1, 2, 3}},
		{name: "top of range", in: "4294967294:4294967295", want: []SeqNum{4294967294, 4294967295}, exact: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := ParseSeqSet(tc.in)
			if err != nil {
				t.Fatalf("ParseSeqSet(%q): %v", tc.in, err)
			}
			got, ok := s.Nums()
			if ok != tc.exact {
				t.Fatalf("Nums() ok = %v, want %v", ok, tc.exact)
			}
			if tc.exact {
				if !slices.Equal(got, tc.want) {
					t.Errorf("Nums() = %v, want %v", got, tc.want)
				}
			}
			wantAll := tc.want
			if !tc.exact {
				wantAll = tc.partial
			}
			if all := slices.Collect(s.All()); !slices.Equal(all, wantAll) {
				t.Errorf("All() = %v, want %v", all, wantAll)
			}
		})
	}
}

func TestNumSetAllEarlyStop(t *testing.T) {
	s, err := ParseSeqSet("1:1000")
	if err != nil {
		t.Fatal(err)
	}
	var got []SeqNum
	for n := range s.All() {
		got = append(got, n)
		if len(got) == 3 {
			break
		}
	}
	if want := []SeqNum{1, 2, 3}; !slices.Equal(got, want) {
		t.Errorf("All() with break = %v, want %v", got, want)
	}
}

func TestNumSetAdd(t *testing.T) {
	var s SeqSet
	s.AddNum(3, 1, 2)
	if got := s.String(); got != "1:3" {
		t.Errorf("after AddNum = %q, want %q", got, "1:3")
	}
	s.AddNum(0) // ignored: zero is not a message number
	if got := s.String(); got != "1:3" {
		t.Errorf("after AddNum(0) = %q, want %q", got, "1:3")
	}
	s.AddRange(10, 0) // 10:*
	if got := s.String(); got != "1:3,10:*" {
		t.Errorf("after AddRange = %q, want %q", got, "1:3,10:*")
	}
	var other SeqSet
	other.AddRange(4, 9)
	s.AddSet(other)
	if got := s.String(); got != "1:*" {
		t.Errorf("after AddSet = %q, want %q", got, "1:*")
	}

	var u UIDSet
	u.AddNum(7)
	if got := u.String(); got != "7" {
		t.Errorf("UIDSet after AddNum = %q, want %q", got, "7")
	}
}

func TestNumSetConstructors(t *testing.T) {
	if got := SeqSetNum(1, 2, 3).String(); got != "1:3" {
		t.Errorf("SeqSetNum = %q, want %q", got, "1:3")
	}
	if got := SeqSetRange(1, 0).String(); got != "1:*" {
		t.Errorf("SeqSetRange = %q, want %q", got, "1:*")
	}
	if got := UIDSetNum(5, 6).String(); got != "5:6" {
		t.Errorf("UIDSetNum = %q, want %q", got, "5:6")
	}
	if got := UIDSetRange(2, 4).String(); got != "2:4" {
		t.Errorf("UIDSetRange = %q, want %q", got, "2:4")
	}
	if got := SeqSetNum().String(); got != "" {
		t.Errorf("SeqSetNum() = %q, want empty", got)
	}
}

func TestNumSetEqual(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"1,2,3", "1:3", true},
		{"3,1,2", "1:3", true},
		{"1:3", "1:4", false},
		{"*:4", "4:*", true},
		{"1:*", "1:*,5", true},
		{"1", "2", false},
	} {
		a, err := ParseSeqSet(tc.a)
		if err != nil {
			t.Fatal(err)
		}
		b, err := ParseSeqSet(tc.b)
		if err != nil {
			t.Fatal(err)
		}
		if got := a.Equal(b); got != tc.want {
			t.Errorf("%q.Equal(%q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestNumSetRoundTrip(t *testing.T) {
	for _, in := range []string{"1", "1:5", "5:*", "*", "1:5,7,9:*", "1:3,7"} {
		s, err := ParseSeqSet(in)
		if err != nil {
			t.Fatalf("ParseSeqSet(%q): %v", in, err)
		}
		if got := s.String(); got != in {
			t.Errorf("round trip of %q = %q", in, got)
		}
	}
}

func FuzzParseSeqSet(f *testing.F) {
	for _, seed := range []string{
		"", "1", "0", "*", "1:*", "*:1", "1:5,7,9:*", "4294967295",
		"4294967296", "1,,2", "1:2:3", ":", ",", "1:", "abc", "1 2",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		s, err := ParseSeqSet(in)
		if err != nil {
			var ierr *Error
			if !errors.As(err, &ierr) {
				t.Fatalf("ParseSeqSet(%q) returned %T, want *imap.Error", in, err)
			}
			return
		}
		// A successfully parsed set must survive formatting and
		// reparsing, and normalisation must be idempotent.
		out := s.String()
		s2, err := ParseSeqSet(out)
		if err != nil {
			t.Fatalf("ParseSeqSet(%q) -> %q, which fails to reparse: %v", in, out, err)
		}
		if !s.Equal(s2) {
			t.Fatalf("ParseSeqSet(%q) -> %q -> %v, not equal to %v", in, out, s2, s)
		}
		n := s.Normalized()
		if !slices.Equal(n, n.Normalized()) {
			t.Fatalf("Normalized() is not idempotent for %q: %v then %v", in, n, n.Normalized())
		}
		if !n.Equal(s) {
			t.Fatalf("Normalized() changed membership for %q", in)
		}
		for _, r := range n {
			if r.Start == 0 && r.Stop != 0 {
				t.Fatalf("Normalized() left a *:n range for %q: %v", in, n)
			}
		}
	})
}
