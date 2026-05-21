package cli

import "testing"

func TestRedactJWTSub(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		// Realistic TST shape — UUID is sensitive, prefix is type info.
		{"TST-EE4C16F4-7B16-463E-B050-0026A6E837E4", "TST-EE4C…"},
		{"TST-abcd1234", "TST-abcd…"},
		// Prod-ish dash-separated; keeps first segment + 4 chars.
		{"prod-account-12345", "prod-acco…"},
		// No dash — fallback to first 4 chars + ellipsis.
		{"opaque1234567890", "opaq…"},
		// Short strings pass through unchanged.
		{"abc", "abc"},
		{"abcd", "abcd"},
	}
	for _, c := range cases {
		if got := redactJWTSub(c.in); got != c.want {
			t.Errorf("redactJWTSub(%q)=%q, want %q", c.in, got, c.want)
		}
	}
}
