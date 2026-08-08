package webhook

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// timeoutErr implements net.Error with Timeout() = true.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "dial tcp 10.0.0.5:443: i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// The strings stored in last_error are customer-facing (delivery history
// API, auto_disable_reason, health emails). Transport errors must therefore
// map to a SMALL FIXED VOCABULARY — raw err.Error() leaks Go-internal
// detail such as resolver addresses ("lookup x on 127.0.0.11:53: no such
// host") and, via ProxyFromEnvironment, any configured proxy address.
func TestTransportErrorLabel(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"dns failure", &url.Error{Op: "Post", URL: "https://x.test", Err: &net.OpError{Op: "dial", Err: &net.DNSError{Name: "x.test", Server: "127.0.0.11:53", IsNotFound: true}}}, "DNS resolution failed"},
		{"timeout", &url.Error{Op: "Post", URL: "https://x.test", Err: timeoutErr{}}, "request timed out"},
		{"context deadline", &url.Error{Op: "Post", URL: "https://x.test", Err: context.DeadlineExceeded}, "request timed out"},
		{"tls failure", &url.Error{Op: "Post", URL: "https://x.test", Err: fmt.Errorf("tls: handshake failure")}, "TLS handshake failed"},
		{"connection refused", &url.Error{Op: "Post", URL: "https://x.test", Err: &net.OpError{Op: "dial", Err: &os.SyscallError{Syscall: "connect", Err: errors.New("connection refused")}}}, "connection failed"},
		{"anything else", errors.New("read tcp 10.1.2.3:9 -> 10.9.8.7:443: read: connection reset by peer"), "connection failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := transportErrorLabel(tc.err)
			if got != tc.want {
				t.Errorf("transportErrorLabel(%v) = %q, want %q", tc.err, got, tc.want)
			}
			// Whatever the mapping, no raw address material may survive.
			for _, leak := range []string{"127.0", "10.", ":53", ":443"} {
				if strings.Contains(got, leak) {
					t.Errorf("label %q leaks address material %q", got, leak)
				}
			}
		})
	}
}

// End-to-end through Deliver: a refused connection stores the vocabulary
// label, not the raw dial error with its host:port.
func TestDeliver_TransportErrorIsSanitized(t *testing.T) {
	d := NewSubscriberDeliverer(false, "")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // free the port → connection refused

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out := d.Deliver(ctx, "http://"+addr+"/hook", []byte("{}"), "whsec_x", "", "email.received", "1")
	if out.Success {
		t.Fatal("expected failure against a closed port")
	}
	if out.Error != "connection failed" {
		t.Errorf("Error = %q, want the sanitized vocabulary label %q", out.Error, "connection failed")
	}
}

// The SSRF dial guard keeps its own vocabulary entry — the class is
// diagnostic for the customer — but never echoes the resolved IP.
func TestTransportErrorLabel_DialGuard(t *testing.T) {
	guardErr := fmt.Errorf("webhook dial blocked (%s): %w", "203.0.113.9", ErrDisallowedWebhookIP)
	wrapped := &url.Error{Op: "Post", URL: "https://x.test", Err: guardErr}
	got := transportErrorLabel(wrapped)
	if got != "delivery blocked: URL resolved to a disallowed IP" {
		t.Errorf("label = %q", got)
	}
	if strings.Contains(got, "203.0.113.9") {
		t.Errorf("label echoes the resolved IP")
	}
}
