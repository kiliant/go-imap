package imapserver

import (
	"context"
	"slices"
	"testing"
)

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

// A base selection option the framework accepts must be one its own
// RECURSIVEMATCH evaluation handles, or an unwitnessed backend gets a NO for
// a valid command. Adding a base option to listSelectBaseOptions without
// teaching listRecursiveMatch about it fails here.
func TestListRecursiveMatchCoversBaseOptions(t *testing.T) {
	for _, option := range listSelectBaseOptions {
		if !slices.Contains(listRecursiveMatchBaseOptions, option) {
			t.Errorf("base selection option %s is not evaluated by listRecursiveMatch", option)
		}
	}
}

// Until then, the evaluation refuses a base option it does not know rather
// than passing RECURSIVEMATCH on to a backend that never witnessed it. The
// refusal comes before any backend call, so no session is needed.
func TestListRecursiveMatchRefusesUnknownBaseOption(t *testing.T) {
	saved := listSelectBaseOptions
	listSelectBaseOptions = append(slices.Clone(saved), "X-FUTURE-BASE")
	defer func() { listSelectBaseOptions = saved }()
	args := &listArgs{selection: []string{"X-FUTURE-BASE", "RECURSIVEMATCH"}, patterns: []string{"*"}}
	if err := listRecursiveMatch(context.Background(), nil, nil, args, &ListOptions{SelectRecursiveMatch: true}); err == nil {
		t.Fatal("listRecursiveMatch accepted a base option it does not evaluate")
	}
}

// A routing token decides who evaluates a behaviour, never whether anything
// is advertised. So it must gate nothing: not its own capability, and not
// IMAP4rev2 through rev2Incorporated. Making it a gate later would change
// what false means for every backend already released.
func TestRoutingTokensGateNothing(t *testing.T) {
	for _, token := range []string{listRecursiveMatchWitness} {
		if capabilityWitness(token) != nil {
			t.Errorf("%s is a routing token but gates its own advertisement", token)
		}
		if slices.Contains(rev2Incorporated, token) {
			t.Errorf("%s is a routing token but gates IMAP4rev2", token)
		}
	}
}
