package webhook

import (
	"context"
	"net"
	"strings"
	"testing"
)

func TestIsDisallowedWebhookIP(t *testing.T) {
	cases := []struct {
		ip      string
		blocked bool
	}{
		{"127.0.0.1", true},       // loopback
		{"::1", true},             // loopback v6
		{"10.0.0.5", true},        // RFC-1918
		{"172.16.3.4", true},      // RFC-1918
		{"192.168.1.1", true},     // RFC-1918
		{"169.254.169.254", true}, // cloud metadata (link-local)
		{"100.64.1.2", true},      // CGNAT (RFC 6598)
		{"100.127.255.255", true}, // CGNAT upper edge
		{"0.0.0.0", true},         // unspecified
		{"224.0.0.1", true},       // multicast
		{"fc00::1", true},         // IPv6 ULA
		{"fd00:ec2::254", true},   // IPv6 ULA (fd00, IMDSv6-style)
		{"fe80::1", true},         // IPv6 link-local
		{"::", true},              // unspecified v6
		// IPv4-mapped IPv6 (::ffff:a.b.c.d) — the classic guard-bypass class:
		// an internal v4 target smuggled through a v6 literal. Go's IP.To4()
		// unwraps it so the same private/loopback/link-local checks apply.
		{"::ffff:127.0.0.1", true},       // v4-mapped loopback
		{"::ffff:10.0.0.1", true},        // v4-mapped RFC-1918
		{"::ffff:169.254.169.254", true}, // v4-mapped cloud metadata
		{"8.8.8.8", false},               // public
		{"1.1.1.1", false},               // public
		{"100.63.255.255", false},        // just below CGNAT
		{"100.128.0.0", false},           // just above CGNAT
		{"2606:4700:4700::1111", false},  // public v6 (Cloudflare)
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		if ip == nil {
			t.Fatalf("bad test IP %q", c.ip)
		}
		if got := IsDisallowedWebhookIP(ip); got != c.blocked {
			t.Errorf("IsDisallowedWebhookIP(%s) = %v, want %v", c.ip, got, c.blocked)
		}
	}
	// nil is treated as disallowed (fail closed).
	if !IsDisallowedWebhookIP(nil) {
		t.Error("IsDisallowedWebhookIP(nil) = false, want true (fail closed)")
	}
}

func TestGuardedDialControl(t *testing.T) {
	if err := guardedDialControl("tcp", "127.0.0.1:443", nil); err == nil {
		t.Error("dial control allowed loopback; want blocked")
	}
	if err := guardedDialControl("tcp", "169.254.169.254:80", nil); err == nil {
		t.Error("dial control allowed metadata IP; want blocked")
	}
	// Bracketed IPv6 literals, including the v4-mapped metadata bypass, must be
	// split and blocked exactly like their v4 forms.
	if err := guardedDialControl("tcp", "[::1]:443", nil); err == nil {
		t.Error("dial control allowed IPv6 loopback; want blocked")
	}
	if err := guardedDialControl("tcp", "[::ffff:169.254.169.254]:80", nil); err == nil {
		t.Error("dial control allowed v4-mapped metadata IP; want blocked")
	}
	if err := guardedDialControl("tcp", "8.8.8.8:443", nil); err != nil {
		t.Errorf("dial control blocked a public IP: %v", err)
	}
}

// The production deliverer (requireHTTPS=true) installs the dial guard, so a
// webhook URL that resolves to an internal IP — the DNS-rebinding payload — is
// refused at connect time even though it passed registration validation.
func TestSubscriberDeliverer_ProdBlocksInternalIP(t *testing.T) {
	d := NewSubscriberDeliverer(true, "")
	// A literal-IP HTTPS URL clears the scheme check, then the dial guard
	// rejects the resolved loopback address before any connection is made.
	out := d.Deliver(context.Background(), "https://127.0.0.1:9/hook", []byte(`{}`), "whsec_x", "", "email.received", "1")
	if out.Success {
		t.Fatal("delivery to loopback succeeded; want blocked by dial guard")
	}
	if !strings.Contains(out.Error, "disallowed IP") {
		t.Errorf("error = %q, want a dial-guard 'disallowed IP' message", out.Error)
	}
}

// The non-production deliverer keeps the default transport (no guard) so local
// and CI deliveries to 127.0.0.1 still work.
func TestSubscriberDeliverer_DevAllowsLoopbackTransport(t *testing.T) {
	d := NewSubscriberDeliverer(false, "")
	if d.client.Transport != nil {
		t.Error("dev deliverer installed a custom transport; want default (no IP guard)")
	}
}
