package imapnum

import (
	"slices"
	"testing"
)

type testNum uint32

func TestSetArithmetic(t *testing.T) {
	s, err := Parse[testNum]("9,1:3,3:7")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := s.Normalized().String(), "1:7,9"; got != want {
		t.Fatalf("Normalized() = %q, want %q", got, want)
	}
	if !s.Contains(5) || s.Contains(8) || s.Contains(0) {
		t.Fatal("Contains returned incorrect membership")
	}
	if got, ok := s.Nums(); !ok || !slices.Equal(got, []testNum{1, 2, 3, 4, 5, 6, 7, 9}) {
		t.Fatalf("Nums() = %v, %v", got, ok)
	}

	var added Set[testNum]
	added.AddNum(3, 1, 2)
	added.AddRange(4, 8)
	added.AddSet(SetNum[testNum](9))
	if got := added.String(); got != "1:9" {
		t.Fatalf("mutating set methods produced %q", got)
	}
}

func TestSetWildcardAndMax(t *testing.T) {
	tests := []struct {
		in   string
		want string
		dyn  bool
	}{
		{in: "*:4", want: "4:*", dyn: true},
		{in: "1:*,5", want: "1:*", dyn: true},
		{in: "4294967295", want: "4294967295", dyn: false},
		{in: "4294967290:4294967295", want: "4294967290:4294967295", dyn: false},
		{in: "4294967295,*", want: "*", dyn: true},
	}
	for _, tt := range tests {
		s, err := Parse[testNum](tt.in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", tt.in, err)
		}
		if got := s.Normalized().String(); got != tt.want {
			t.Errorf("Parse(%q).Normalized() = %q, want %q", tt.in, got, tt.want)
		}
		if got := s.Dynamic(); got != tt.dyn {
			t.Errorf("Parse(%q).Dynamic() = %v, want %v", tt.in, got, tt.dyn)
		}
	}

	static, err := Parse[testNum]("4294967294:4294967295")
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := static.Nums(); !ok || !slices.Equal(got, []testNum{4294967294, 4294967295}) {
		t.Fatalf("top-of-range Nums() = %v, %v", got, ok)
	}
}

func TestParseErrors(t *testing.T) {
	for _, in := range []string{"", "0", "1:0", "1,,2", "1:2:3", "4294967296", "-1"} {
		if _, err := Parse[testNum](in); err == nil {
			t.Errorf("Parse(%q) succeeded", in)
		}
	}
}
