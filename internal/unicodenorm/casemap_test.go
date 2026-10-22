package unicodenorm

import "testing"

func TestCasemap(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"aZ[_", "AZ[_"},
		{"åÅA\u030a", "A\u030aA\u030aA\u030a"},
		// RFC 5051's titlecase-before-decomposition example preserves the
		// lower-case z introduced by decomposition; uppercasing is different.
		{"\u01c4", "Dz\u030c"},
		{"\ufb03", "ffi"},
		{"a\xffZ", "a\xffZ"},
	} {
		if got := Casemap(tc.in); got != tc.want {
			t.Errorf("Casemap(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
