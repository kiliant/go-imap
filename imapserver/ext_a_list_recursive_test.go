package imapserver

import "testing"

func TestListWildcardMatch(t *testing.T) {
	tests := []struct {
		name, pattern string
		delimiter     rune
		want          bool
	}{
		{"Foo", "*", '/', true},
		{"Foo/Bar", "*", '/', true},
		{"Foo/Bar", "%", '/', false},
		{"Foo/Bar", "Foo/%", '/', true},
		{"Foo/Bar/Baz", "Foo/%", '/', false},
		{"Foo/Bar/Baz", "Foo/*", '/', true},
		{"Foo/Bar/Baz", "%/%/Baz", '/', true},
		{"Foo", "F%o", '/', true},
		{"Foo", "Fo", '/', false},
		{"Foo", "Fooo", '/', false},
		{"", "%", '/', true},
		{"", "", '/', true},
		{"Foo.Bar", "%", '.', false},
		{"Foo/Bar", "%", 0, true},
	}
	for _, test := range tests {
		if got := listWildcardMatch([]rune(test.name), []rune(test.pattern), test.delimiter); got != test.want {
			t.Errorf("listWildcardMatch(%q, %q, %q) = %t, want %t", test.name, test.pattern, test.delimiter, got, test.want)
		}
	}
}

// The canonical pattern is the reference followed by the pattern, unless the
// pattern starts with the delimiter, as in imapserver/memory.
func TestListPatternMatches(t *testing.T) {
	tests := []struct {
		name, reference string
		patterns        []string
		want            bool
	}{
		{"Work/Foo", "Work/", []string{"%"}, true},
		{"Foo", "Work/", []string{"%"}, false},
		{"Work/Foo", "Work/", []string{"Foo"}, true},
		{"/Foo", "Work/", []string{"/Foo"}, true},
		{"Work//Foo", "Work/", []string{"/Foo"}, false},
		{"Bar", "", []string{"Foo", "B*"}, true},
		{"Bar", "", nil, false},
	}
	for _, test := range tests {
		if got := listPatternMatches(test.name, test.reference, test.patterns, '/'); got != test.want {
			t.Errorf("listPatternMatches(%q, %q, %q) = %t, want %t", test.name, test.reference, test.patterns, got, test.want)
		}
	}
}

func TestListParents(t *testing.T) {
	got := listParents("Deep/Er/Child", '/')
	if len(got) != 2 || got[0] != "Deep" || got[1] != "Deep/Er" {
		t.Errorf("listParents = %q, want Deep and Deep/Er", got)
	}
	if got := listParents("/Rooted", '/'); len(got) != 0 {
		t.Errorf("listParents of a rooted name = %q, want none", got)
	}
	if got := listParents("Flat/Name", 0); got != nil {
		t.Errorf("listParents without a delimiter = %q, want none", got)
	}
}
