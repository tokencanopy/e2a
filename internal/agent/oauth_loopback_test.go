package agent

import "testing"

func TestIsLoopbackRedirect(t *testing.T) {
	cases := []struct {
		uri  string
		want bool
	}{
		// loopback http → true (the account-scope gate passes)
		{"http://localhost/callback", true},
		{"http://localhost:64793/callback", true},
		{"http://127.0.0.1/callback", true},
		{"http://127.0.0.1:8080/cb", true},
		{"http://[::1]/callback", true},
		{"http://[::1]:5173/cb", true},
		{"http://127.0.0.2/cb", true}, // all of 127.0.0.0/8 is loopback

		// https → false even if host is localhost (no guarantee the
		// callback lands on the user's machine)
		{"https://localhost/callback", false},
		{"https://app.example.com/callback", false},

		// non-loopback http → false
		{"http://example.com/callback", false},
		{"http://0.0.0.0/callback", false},
		{"http://10.0.0.5/callback", false},
		{"http://169.254.169.254/callback", false}, // link-local metadata, not loopback

		// junk → false (fail closed)
		{"", false},
		{"::::not a uri", false},
		{"javascript:alert(1)", false},
		{"file:///etc/passwd", false},
	}
	for _, c := range cases {
		if got := isLoopbackRedirect(c.uri); got != c.want {
			t.Errorf("isLoopbackRedirect(%q) = %v, want %v", c.uri, got, c.want)
		}
	}
}

// TestAccountEligibleRedirect pins the deliberately-relaxed account gate
// (2026-07-10): account (workspace admin) is granted to loopback OR https
// redirects — the latter so hosted connectors (Claude Chat/Cowork) qualify —
// but NOT to custom-scheme or non-loopback http redirects.
func TestAccountEligibleRedirect(t *testing.T) {
	cases := []struct {
		uri  string
		want bool
	}{
		// loopback http → eligible (unchanged)
		{"http://localhost:3118/callback", true},
		{"http://127.0.0.1:8765/cb", true},
		{"http://[::1]/callback", true},

		// https → now eligible (the relaxation)
		{"https://claude.ai/api/mcp/auth_callback", true},
		{"https://app.example.com/callback", true},
		{"https://localhost/callback", true}, // scheme is what matters, not host

		// reverse-domain custom scheme → NOT eligible (valid redirect, but
		// the callback doesn't land on the user's machine and isn't https)
		{"com.example.app:/oauth-callback", false},

		// non-loopback http → not eligible (also rejected at registration)
		{"http://example.com/callback", false},

		// malformed https that validateRedirectURI rejects → NOT eligible
		// (defense-in-depth: the gate is self-contained, not trusting an
		// upstream validator)
		{"https://user@evil.com/cb", false},    // userinfo
		{"https:///cb", false},                 // empty host
		{"https://example.com/cb#frag", false}, // fragment

		// junk → not eligible (fail closed)
		{"", false},
		{"::::not a uri", false},
		{"javascript:alert(1)", false},
	}
	for _, c := range cases {
		if got := accountEligibleRedirect(c.uri); got != c.want {
			t.Errorf("accountEligibleRedirect(%q) = %v, want %v", c.uri, got, c.want)
		}
	}
}
