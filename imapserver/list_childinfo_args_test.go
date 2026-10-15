package imapserver

import (
	"reflect"
	"testing"
)

// The framework, not the backend, decides whether CHILDINFO is written and
// what it may say: only under RECURSIVEMATCH, never for LSUB, and only base
// selection options the command requested. RFC 5258 section 3.5.
func TestListResultChildInfo(t *testing.T) {
	recursive := &ListOptions{Subscribed: true, SelectRecursiveMatch: true}
	tests := []struct {
		name      string
		args      *listArgs
		options   *ListOptions
		childInfo []string
		want      []string
	}{
		{"recursive", &listArgs{selection: []string{"SUBSCRIBED", "RECURSIVEMATCH"}}, recursive, []string{"SUBSCRIBED"}, []string{"SUBSCRIBED"}},
		{"case and repeats", &listArgs{selection: []string{"SUBSCRIBED", "RECURSIVEMATCH"}}, recursive, []string{"subscribed", "SUBSCRIBED"}, []string{"SUBSCRIBED"}},
		{"unrequested and unknown dropped", &listArgs{selection: []string{"SUBSCRIBED", "RECURSIVEMATCH"}}, recursive, []string{"REMOTE", "X-FUTURE", "SUBSCRIBED", "SPECIAL-USE"}, []string{"SUBSCRIBED"}},
		{"nothing left", &listArgs{selection: []string{"SUBSCRIBED", "RECURSIVEMATCH"}}, recursive, []string{"REMOTE"}, nil},
		{"without RECURSIVEMATCH", &listArgs{selection: []string{"SUBSCRIBED"}}, &ListOptions{Subscribed: true}, []string{"SUBSCRIBED"}, nil},
		{"LSUB", &listArgs{legacy: true, selection: []string{"SUBSCRIBED"}}, recursive, []string{"SUBSCRIBED"}, nil},
		{"plain LIST", &listArgs{}, &ListOptions{}, []string{"SUBSCRIBED"}, nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := listResultChildInfo(test.args, test.options, test.childInfo); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("listResultChildInfo() = %q, want %q", got, test.want)
			}
		})
	}
}
