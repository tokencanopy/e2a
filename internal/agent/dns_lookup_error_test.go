package agent

import (
	"context"
	"net"
	"strings"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

// startFakeServFailDNSServer answers every query on a local UDP socket with
// SERVFAIL: a genuine resolver failure, as opposed to NXDOMAIN/NODATA (the
// resolver actually reached a server and that server said "I can't answer
// this," which is a different condition from "this name/record does not
// exist").
func startFakeServFailDNSServer(t *testing.T) string {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	go func() {
		buf := make([]byte, 512)
		for {
			n, addr, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			var p dnsmessage.Parser
			hdr, err := p.Start(buf[:n])
			if err != nil {
				continue
			}
			q, err := p.Question()
			if err != nil {
				continue
			}
			b := dnsmessage.NewBuilder(nil, dnsmessage.Header{
				ID:       hdr.ID,
				Response: true,
				RCode:    dnsmessage.RCodeServerFailure,
			})
			// The stub resolver validates the echoed question against what
			// it sent; an answer with no question section is dropped as
			// malformed and the lookup times out instead of failing fast.
			b.StartQuestions()
			b.Question(q)
			out, err := b.Finish()
			if err != nil {
				continue
			}
			_, _ = conn.WriteToUDP(out, addr)
		}
	}()
	return conn.LocalAddr().String()
}

// withFakeDNSServer points the process's default resolver at addr for the
// duration of the test and restores it afterward. checkDomainRecords calls
// the unqualified net.LookupTXT/net.LookupMX, which both resolve through
// net.DefaultResolver, so this is the seam available without changing
// checkDomainRecords's signature.
func withFakeDNSServer(t *testing.T, addr string) {
	t.Helper()
	prevPreferGo := net.DefaultResolver.PreferGo
	prevDial := net.DefaultResolver.Dial
	net.DefaultResolver.PreferGo = true
	net.DefaultResolver.Dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return net.Dial("udp", addr)
	}
	t.Cleanup(func() {
		net.DefaultResolver.PreferGo = prevPreferGo
		net.DefaultResolver.Dial = prevDial
	})
}

// TestCheckDomainRecordsGenuineFailureSetsDNSError is the regression test for
// e2a#129 item 2: a resolver that actually answers with SERVFAIL (a genuine
// infrastructure failure) must be visible in DNSError, not collapsed into
// the same "missing" result an unpublished record produces.
func TestCheckDomainRecordsGenuineFailureSetsDNSError(t *testing.T) {
	withFakeDNSServer(t, startFakeServFailDNSServer(t))

	c := checkDomainRecords("example.com", "mx.e2a.test", "tok", "e2a", "MIIBkeyAQAB", true /* production */)

	if c.DNSError == "" {
		t.Fatal("SERVFAIL on every probe should set DNSError; got empty")
	}
	if c.MX != "missing" || c.SPF != "missing" || c.DKIM != "missing" {
		t.Errorf("a resolver failure still reports the ordinary missing defaults so existing MX/SPF/DKIM consumers are unaffected: got MX=%q SPF=%q DKIM=%q", c.MX, c.SPF, c.DKIM)
	}
}

// TestCheckDomainRecordsNotFoundLeavesDNSErrorEmpty pins the other side of
// the same boundary: an ordinary not-yet-published record (NXDOMAIN/NODATA)
// must NOT be reported as a DNS error, or every unverified domain would show
// one. Reuses the invalid-label technique from
// TestCheckDomainRecordsDevShortCircuitLogsWarning (label > 63 chars is
// rejected by the resolver client-side as not-found, with no network
// traffic), so this needs no fake server.
func TestCheckDomainRecordsNotFoundLeavesDNSErrorEmpty(t *testing.T) {
	invalid := strings.Repeat("x", 64) + ".invalid"
	c := checkDomainRecords(invalid, "mx.e2a.test", "tok", "e2a", "MIIBkeyAQAB", true /* production */)
	if c.DNSError != "" {
		t.Errorf("an ordinary not-found answer must not be reported as a DNS error, got %q", c.DNSError)
	}
}

// TestIsDNSNotFound pins the classification the fix depends on: NXDOMAIN and
// NODATA both set IsNotFound (Go's resolver reports both as errNoSuchHost).
// Everything else (timeouts, SERVFAIL, a non-DNSError transport failure)
// does not.
func TestIsDNSNotFound(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"NXDOMAIN/NODATA", &net.DNSError{Err: "no such host", Name: "example.com", IsNotFound: true}, true},
		{"timeout", &net.DNSError{Err: "i/o timeout", Name: "example.com", IsTimeout: true}, false},
		{"SERVFAIL-shaped temporary failure", &net.DNSError{Err: "server misbehaving", Name: "example.com", IsTemporary: true}, false},
		{"non-DNSError", context.DeadlineExceeded, false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isDNSNotFound(c.err); got != c.want {
				t.Errorf("isDNSNotFound(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}
