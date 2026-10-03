package main

import "testing"

func TestResolvePasswordProvided(t *testing.T) {
	got, err := resolvePassword("secret")
	if err != nil {
		t.Fatal(err)
	}
	if got != "secret" {
		t.Fatalf("resolvePassword = %q, want secret", got)
	}
}

func TestTrimPasswordLine(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"secret\n", "secret"},
		{"secret\r\n", "secret"},
		{"secret", "secret"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := trimPasswordLine(tt.in); got != tt.want {
			t.Fatalf("trimPasswordLine(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
