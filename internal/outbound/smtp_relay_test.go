package outbound

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/textproto"
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/config"
)

func TestSMTPRelaySendWithContextCancelsHangingServer(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			accepted <- conn
		}
	}()
	t.Cleanup(func() {
		select {
		case conn := <-accepted:
			_ = conn.Close()
		default:
		}
	})

	addr := listener.Addr().(*net.TCPAddr)
	relay := NewSMTPRelay(&config.OutboundSMTPConfig{Host: addr.IP.String(), Port: addr.Port})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	started := time.Now()
	_, err = relay.SendWithContext(ctx, "noreply@example.com", []string{"feedback@example.com"}, []byte("Subject: test\r\n\r\nbody"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("SendWithContext error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("SendWithContext returned after %s, want cancellation within 1s", elapsed)
	}
}

func TestSMTPRelaySendOnceContextHonorsCancellation(t *testing.T) {
	relay := NewSMTPRelay(&config.OutboundSMTPConfig{Host: "127.0.0.1", Port: 1})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := relay.SendOnceContext(ctx, "noreply@example.com",
		[]string{"recipient@example.com"}, []byte("Subject: test\r\n\r\nbody"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("SendOnceContext error = %v, want context canceled", err)
	}
}

// smtpErr mirrors PRODUCTION's error shape: net/smtp returns a *textproto.Error for
// a non-2xx reply, which sendOnce wraps with a command prefix via %w. The
// classifiers must key on the wrapped code, not the string — a bare
// errors.New("550 ...") is NOT what the relay produces.
func smtpErr(code int, msg string) error {
	return fmt.Errorf("rcpt to nobody@example.com: %w", &textproto.Error{Code: code, Msg: msg})
}

// connErr mirrors a wrapped network failure (dial timeout / refused).
func connErr(inner string) error {
	return fmt.Errorf("dial: %w", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New(inner)})
}

func TestIsPermanentSMTPError(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{smtpErr(550, "5.1.1 User unknown"), true},
		{smtpErr(554, "Message rejected"), true},
		{smtpErr(550, "5.7.1 TLS required"), true}, // 5xx even though text has "tls"
		{smtpErr(421, "too many connections"), false},
		{smtpErr(450, "mailbox busy"), false},
		{connErr("connection refused"), false}, // connection — MUST retry
		{errors.New("outbound SMTP relay not configured"), false},
		{nil, false},
	}
	for _, c := range cases {
		if got := IsPermanentSMTPError(c.err); got != c.want {
			t.Errorf("IsPermanentSMTPError(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

func TestIsTransientSMTPError_4xx(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{smtpErr(421, "Too many connections"), true},
		{smtpErr(450, "Mailbox busy"), true},
		{smtpErr(451, "Temporary failure"), true},
		{smtpErr(550, "Mailbox not found"), false},
		{connErr("connection reset"), false},
		{errors.New("throttling: too many requests"), true}, // codeless throttle phrasing
		{nil, false},
	}
	for _, c := range cases {
		if got := isTransientSMTPError(c.err); got != c.want {
			t.Errorf("isTransientSMTPError(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

// TestIsConnectionError is the adversarial-review guard: a per-message SMTP verdict
// (has a code) is NEVER an outage, even if its text contains "tls"/"timeout";
// only codeless network failures + the not-configured relay are outages.
func TestIsConnectionError(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{connErr("connect: connection refused"), true},
		{connErr("i/o timeout"), true},
		{errors.New("outbound SMTP relay not configured"), true},
		{smtpErr(550, "5.7.1 TLS required"), false}, // has "tls" but it's a 5xx verdict, NOT an outage
		{smtpErr(450, "try again"), false},          // 4xx verdict, not an outage
		{smtpErr(421, "connection timeout on our end"), false},
		{nil, false},
	}
	for _, c := range cases {
		if got := IsConnectionError(c.err); got != c.want {
			t.Errorf("IsConnectionError(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func TestParseMessageIDFromResponse(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Ok <010f019d2bd82cd5-49c4925c@us-east-2.amazonses.com>", "<010f019d2bd82cd5-49c4925c@us-east-2.amazonses.com>"},
		{"<simple@example.com>", "<simple@example.com>"},
		{"Ok no-angle-brackets", "<no-angle-brackets>"},
		// Parse alone only wraps a bare id — sendOnce then appends the provider
		// domain via qualifyMessageIDDomain. The composed capture path is pinned
		// in TestCapturedMessageIDMatchesOnWireHeader.
		{"Ok 010f019d4b3843be-53882e6f-46de-4221-a56a-ba993e8f83e8-000000", "<010f019d4b3843be-53882e6f-46de-4221-a56a-ba993e8f83e8-000000>"},
		{"", ""},
		{"  whitespace  ", "<whitespace>"},
	}

	for _, tt := range tests {
		got := parseMessageIDFromResponse(tt.input)
		if got != tt.want {
			t.Errorf("parseMessageIDFromResponse(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestQualifyMessageIDDomain(t *testing.T) {
	sesCfg := &config.OutboundSMTPConfig{Host: "email-smtp.us-east-2.amazonaws.com"}
	tests := []struct {
		name string
		id   string
		cfg  *config.OutboundSMTPConfig
		want string
	}{
		{"bare id gains the region domain derived from the SES host",
			"<010f-abc-000000>", sesCfg, "<010f-abc-000000@us-east-2.amazonses.com>"},
		{"already-qualified id passes through untouched",
			"<010f-abc-000000@us-east-2.amazonses.com>", sesCfg, "<010f-abc-000000@us-east-2.amazonses.com>"},
		{"explicit message_id_domain wins over host derivation",
			"<010f-abc-000000>",
			&config.OutboundSMTPConfig{Host: "email-smtp.us-east-2.amazonaws.com", MessageIDDomain: "eu-west-1.amazonses.com"},
			"<010f-abc-000000@eu-west-1.amazonses.com>"},
		{"non-SES host with no override leaves the id alone (never fabricate)",
			"<010f-abc-000000>", &config.OutboundSMTPConfig{Host: "smtp.example.com"}, "<010f-abc-000000>"},
		{"empty id stays empty", "", sesCfg, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := qualifyMessageIDDomain(tt.id, tt.cfg); got != tt.want {
				t.Errorf("qualifyMessageIDDomain(%q) = %q, want %q", tt.id, got, tt.want)
			}
		})
	}
}

func TestSESRegionFromHost(t *testing.T) {
	tests := []struct {
		host string
		want string
	}{
		{"email-smtp.us-east-2.amazonaws.com", "us-east-2"},
		{"email-smtp.eu-west-1.amazonaws.com", "eu-west-1"},
		{"smtp.example.com", ""},
		{"email-smtp.amazonaws.com", ""},     // no region segment
		{"email-smtp.a.b.amazonaws.com", ""}, // region can't contain dots
		{"localhost", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := sesRegionFromHost(tt.host); got != tt.want {
			t.Errorf("sesRegionFromHost(%q) = %q, want %q", tt.host, got, tt.want)
		}
	}
}

// TestCapturedMessageIDMatchesOnWireHeader pins the full capture path
// (parse → qualify) on the exact 250 shape production SES SMTP returns: the
// id arrives BARE (no brackets, no domain), while the Message-ID SES stamps
// on the delivered message is <id@<region>.amazonses.com>. The captured value
// is stored as provider_message_id and copied verbatim into a reply's
// In-Reply-To/References — so anything other than the on-wire form forks the
// recipient's thread in every RFC 5322 client.
func TestCapturedMessageIDMatchesOnWireHeader(t *testing.T) {
	cfg := &config.OutboundSMTPConfig{Host: "email-smtp.us-east-2.amazonaws.com"}
	resp := "Ok 010f019d4b3843be-53882e6f-46de-4221-a56a-ba993e8f83e8-000000"
	want := "<010f019d4b3843be-53882e6f-46de-4221-a56a-ba993e8f83e8-000000@us-east-2.amazonses.com>"
	if got := qualifyMessageIDDomain(parseMessageIDFromResponse(resp), cfg); got != want {
		t.Errorf("captured id = %q, want the on-wire Message-ID %q", got, want)
	}
}
