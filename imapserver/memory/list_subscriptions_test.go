package memory

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/imapserver"
)

func listTestSession(t *testing.T, create ...string) *session {
	t.Helper()
	backend := New(&Options{Users: map[string]string{"alice": "secret"}})
	authenticated, err := backend.Authenticate(t.Context(), nil, &imapserver.Credentials{Mechanism: "LOGIN", Username: "alice", Password: "secret"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = authenticated.Close(context.Background(), nil) })
	s := authenticated.(*session)
	for _, name := range create {
		if err := s.Create(t.Context(), name, nil); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// listResult renders each LIST result as "name attr attr" with sorted
// attributes, one per line in mailbox order.
func listResult(t *testing.T, s *session, pattern string, options *imapserver.ListOptions) string {
	t.Helper()
	var lines []string
	writer := &imapserver.ListWriter{WriteFunc: func(_ context.Context, data *imap.ListData) error {
		attrs := make([]string, 0, len(data.Attrs))
		for _, attr := range data.Attrs {
			attrs = append(attrs, string(attr))
		}
		slices.Sort(attrs)
		lines = append(lines, strings.TrimSpace(data.Mailbox+" "+strings.Join(attrs, " ")))
		return nil
	}}
	if err := s.List(t.Context(), writer, "", []string{pattern}, options); err != nil {
		t.Fatal(err)
	}
	return strings.Join(lines, "\n")
}

// A subscription belongs to the name. DELETE must not remove it (RFC 3501
// section 6.3.9), LIST (SUBSCRIBED) still returns it as \NonExistent
// (RFC 5258 section 3), and UNSUBSCRIBE must still be able to remove it.
func TestSubscriptionOutlivesDelete(t *testing.T) {
	s := listTestSession(t, "a", "a/b")
	for _, name := range []string{"a", "a/b"} {
		if err := s.Subscribe(t.Context(), name, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Delete(t.Context(), "a/b", nil); err != nil {
		t.Fatal(err)
	}
	subscribed := &imapserver.ListOptions{Subscribed: true}
	want := "a \\HasNoChildren \\Subscribed\na/b \\HasNoChildren \\NonExistent \\Subscribed"
	if got := listResult(t, s, "*", subscribed); got != want {
		t.Errorf("LIST (SUBSCRIBED) after DELETE =\n%s\nwant\n%s", got, want)
	}
	if got := listResult(t, s, "*", nil); got != "INBOX \\HasNoChildren\na \\HasNoChildren \\Subscribed" {
		t.Errorf("LIST after DELETE = %q; a deleted name must not reappear", got)
	}
	// Re-creating the name finds it subscribed again.
	if err := s.Create(t.Context(), "a/b", nil); err != nil {
		t.Fatal(err)
	}
	if got := listResult(t, s, "a/b", subscribed); got != "a/b \\HasNoChildren \\Subscribed" {
		t.Errorf("re-created a/b = %q, want it subscribed", got)
	}
	if err := s.Delete(t.Context(), "a/b", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Unsubscribe(t.Context(), "a/b", nil); err != nil {
		t.Fatalf("UNSUBSCRIBE of a stale subscription: %v", err)
	}
	if got := listResult(t, s, "*", subscribed); got != "a \\HasNoChildren \\Subscribed" {
		t.Errorf("LIST (SUBSCRIBED) after UNSUBSCRIBE = %q", got)
	}
	if err := s.Unsubscribe(t.Context(), "never", nil); err == nil {
		t.Error("UNSUBSCRIBE of an unknown name succeeded")
	}
}

// RENAME does not move subscription information (RFC 9051 section 6.3.6).
func TestRenameLeavesSubscription(t *testing.T) {
	s := listTestSession(t, "old")
	if err := s.Subscribe(t.Context(), "old", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Rename(t.Context(), "old", "new", nil); err != nil {
		t.Fatal(err)
	}
	got := listResult(t, s, "*", &imapserver.ListOptions{Subscribed: true})
	if want := "old \\HasNoChildren \\NonExistent \\Subscribed"; got != want {
		t.Errorf("LIST (SUBSCRIBED) after RENAME = %q, want %q", got, want)
	}
}

// "%" returns a hierarchy level that is not a mailbox when a descendant lies
// below the pattern's reach; "*" reaches the descendant itself and does not.
// RFC 3501 section 6.3.8, RFC 5258 section 3.5 and example 11.
func TestListReturnsNonexistentHierarchyLevels(t *testing.T) {
	s := listTestSession(t, "top", "top/mid/leaf", "music/rock")
	for _, tc := range []struct{ pattern, want string }{
		{"%", "INBOX \\HasNoChildren\nmusic \\HasChildren \\NonExistent\ntop \\HasChildren"},
		{"top/%", "top/mid \\HasChildren \\NonExistent"},
		{"top/mid/%", "top/mid/leaf \\HasNoChildren"},
		{"*", "INBOX \\HasNoChildren\nmusic/rock \\HasNoChildren\ntop \\HasChildren\ntop/mid/leaf \\HasNoChildren"},
	} {
		if got := listResult(t, s, tc.pattern, nil); got != tc.want {
			t.Errorf("LIST %q =\n%s\nwant\n%s", tc.pattern, got, tc.want)
		}
	}
	// SPECIAL-USE selects mailboxes with a use; a level that is not a mailbox
	// has none.
	if got := listResult(t, s, "%", &imapserver.ListOptions{SelectSpecialUse: true}); got != "" {
		t.Errorf("LIST (SPECIAL-USE) %% = %q, want nothing", got)
	}
}
