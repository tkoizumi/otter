package cli

import "testing"

func TestShortURLDropsHTTPSOnly(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://api.example.test/v2/records", "api.example.test/v2/records"},
		// Plaintext HTTP keeps its scheme: that is the case worth noticing.
		{"http://api.example.test/v2/records", "http://api.example.test/v2/records"},
		{"api.example.test/x", "api.example.test/x"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := shortURL(tc.in); got != tc.want {
			t.Errorf("shortURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
