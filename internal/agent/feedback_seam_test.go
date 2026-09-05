package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tokencanopy/e2a/internal/config"
	"github.com/tokencanopy/e2a/internal/outbound"
	"github.com/tokencanopy/e2a/internal/sendingpolicy"
	"github.com/tokencanopy/e2a/internal/testutil/testdb"
	"github.com/tokencanopy/e2a/internal/usage"
)

// scriptedSMTP answers one connection per script entry. The entry is the
// reply the server gives after the message body: an SMTP code ("250", "451",
// "554") or "drop", which closes the socket without any reply — the lost-250
// shape the relay reports as ErrProviderAcceptanceUnknown.
type scriptedSMTP struct {
	host string
	port int

	mu       sync.Mutex
	messages []string
	rcpts    [][]string // RCPT TO per connection, in wire order
	conns    int
}

func startScriptedSMTP(t *testing.T, script ...string) *scriptedSMTP {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	addr := listener.Addr().(*net.TCPAddr)
	s := &scriptedSMTP{host: addr.IP.String(), port: addr.Port}

	go func() {
		for _, reply := range script {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			s.mu.Lock()
			s.conns++
			s.mu.Unlock()
			s.serve(conn, reply)
		}
	}()
	return s
}

func (s *scriptedSMTP) serve(conn net.Conn, reply string) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	fmt.Fprint(conn, "220 scripted ready\r\n")
	var data []string
	var rcpts []string
	inData := false
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if inData {
			if line != "." {
				data = append(data, line)
				continue
			}
			s.mu.Lock()
			s.messages = append(s.messages, strings.Join(data, "\n"))
			s.rcpts = append(s.rcpts, rcpts)
			s.mu.Unlock()
			if reply == "drop" {
				return
			}
			fmt.Fprintf(conn, "%s scripted reply\r\n", reply)
			inData = false
			continue
		}
		switch {
		case len(line) > 8 && strings.EqualFold(line[:8], "RCPT TO:"):
			rcpts = append(rcpts, strings.Trim(strings.TrimSpace(line[8:]), "<>"))
			fmt.Fprint(conn, "250 OK\r\n")
		case strings.EqualFold(line, "DATA"):
			inData = true
			fmt.Fprint(conn, "354 Go ahead\r\n")
		case strings.EqualFold(line, "QUIT"):
			fmt.Fprint(conn, "221 Bye\r\n")
			return
		default:
			fmt.Fprint(conn, "250 OK\r\n")
		}
	}
}

func (s *scriptedSMTP) received() ([]string, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.messages...), s.conns
}

func (s *scriptedSMTP) recipients() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]string(nil), s.rcpts...)
}

func attemptHeader(wire string) string {
	for _, line := range strings.Split(wire, "\n") {
		if strings.HasPrefix(line, outbound.ProviderAttemptHeader+": ") {
			return strings.TrimPrefix(line, outbound.ProviderAttemptHeader+": ")
		}
	}
	return ""
}

func countFeedbackAttempts(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM sending_budget_reservations WHERE purpose = 'public_feedback_notification' AND call_state = 'started'`,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func newFeedbackSeamAPI(t *testing.T, s *scriptedSMTP) (*API, *pgxpool.Pool) {
	t.Helper()
	pool := testdb.TestDB(t)
	relay := outbound.NewSMTPRelay(&config.OutboundSMTPConfig{Host: s.host, Port: s.port})
	api := NewAPI(nil, outbound.NewSender(relay, "test.e2a.dev"), relay, nil, usage.NewNoopUsageTracker(), "e2a.dev", "test.e2a.dev", "agents.e2a.dev", "", false)
	gate := sendingpolicy.NewGate(pool, sendingpolicy.Secrets{}, sendingpolicy.PolicySourceConfig, sendingpolicy.DisabledPolicy())
	api.SetProviderSubmitter(outbound.NewProviderSubmitter(relay, gate), gate)
	return api, pool
}

func fastFeedbackBackoff(t *testing.T) {
	t.Helper()
	old := feedbackRetryBackoff
	feedbackRetryBackoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { feedbackRetryBackoff = old })
}

// TestFeedbackSeam_EachPhysicalAttemptIsItsOwnOrdinal: a transient provider
// reply is retried, and the retry is a NEW authorized attempt — a distinct
// ordinal in the ledger and a distinct attempt id on the wire — not a replay
// of the first token.
func TestFeedbackSeam_EachPhysicalAttemptIsItsOwnOrdinal(t *testing.T) {
	fastFeedbackBackoff(t)
	s := startScriptedSMTP(t, "451", "250")
	api, pool := newFeedbackSeamAPI(t, s)
	before := countFeedbackAttempts(t, pool)

	err := api.sendFeedbackEmail(context.Background(), "t", "bug", "m", "", "", []string{"feedback@example.test"}, nil)
	if err != nil {
		t.Fatalf("sendFeedbackEmail: %v", err)
	}
	msgs, conns := s.received()
	if conns != 2 || len(msgs) != 2 {
		t.Fatalf("conns=%d messages=%d, want 2/2 (one retry)", conns, len(msgs))
	}
	a1, a2 := attemptHeader(msgs[0]), attemptHeader(msgs[1])
	if a1 == "" || a2 == "" || a1 == a2 {
		t.Fatalf("attempt ids on the wire = %q / %q, want two distinct non-empty ids", a1, a2)
	}
	if got := countFeedbackAttempts(t, pool) - before; got != 2 {
		t.Fatalf("started feedback attempts = %d, want 2", got)
	}
}

// TestFeedbackSeam_DefiniteRejectionIsNotRetried: a 5xx is the provider's
// answer to the message; retrying it resends nothing.
func TestFeedbackSeam_DefiniteRejectionIsNotRetried(t *testing.T) {
	fastFeedbackBackoff(t)
	s := startScriptedSMTP(t, "554", "250")
	api, pool := newFeedbackSeamAPI(t, s)
	before := countFeedbackAttempts(t, pool)

	err := api.sendFeedbackEmail(context.Background(), "t", "bug", "m", "", "", []string{"feedback@example.test"}, nil)
	if err == nil || !outbound.IsPermanentSMTPError(err) {
		t.Fatalf("err = %v, want the permanent SMTP rejection", err)
	}
	if _, conns := s.received(); conns != 1 {
		t.Fatalf("conns = %d, want 1 (no retry after a definite rejection)", conns)
	}
	if got := countFeedbackAttempts(t, pool) - before; got != 1 {
		t.Fatalf("started feedback attempts = %d, want 1", got)
	}
}

// TestFeedbackSeam_LostAcceptanceIsNotRetried: a body the provider took but
// never answered may already be queued; a retry would be a second copy.
func TestFeedbackSeam_LostAcceptanceIsNotRetried(t *testing.T) {
	fastFeedbackBackoff(t)
	s := startScriptedSMTP(t, "drop", "250")
	api, _ := newFeedbackSeamAPI(t, s)

	err := api.sendFeedbackEmail(context.Background(), "t", "bug", "m", "", "", []string{"feedback@example.test"}, nil)
	if !errors.Is(err, outbound.ErrProviderAcceptanceUnknown) {
		t.Fatalf("err = %v, want ErrProviderAcceptanceUnknown", err)
	}
	if _, conns := s.received(); conns != 1 {
		t.Fatalf("conns = %d, want 1 (no retry after a lost acceptance)", conns)
	}
}

// TestFeedbackSeam_RetriesAreBounded: transient failures stop at the attempt
// cap, each one charged.
func TestFeedbackSeam_RetriesAreBounded(t *testing.T) {
	fastFeedbackBackoff(t)
	s := startScriptedSMTP(t, "451", "451", "451", "451", "250")
	api, pool := newFeedbackSeamAPI(t, s)
	before := countFeedbackAttempts(t, pool)

	err := api.sendFeedbackEmail(context.Background(), "t", "bug", "m", "", "", []string{"feedback@example.test"}, nil)
	if err == nil {
		t.Fatal("expected the exhausted retry loop to fail")
	}
	if _, conns := s.received(); conns != feedbackSendAttempts {
		t.Fatalf("conns = %d, want %d", conns, feedbackSendAttempts)
	}
	if got := countFeedbackAttempts(t, pool) - before; got != feedbackSendAttempts {
		t.Fatalf("started feedback attempts = %d, want %d", got, feedbackSendAttempts)
	}
}

// TestFeedbackSeam_EnvelopeIsConfigurationNotRequest: the recipients on the
// wire are exactly the configured notify set the operation was prepared with;
// the form's own address only ever appears as Reply-To.
func TestFeedbackSeam_EnvelopeIsConfigurationNotRequest(t *testing.T) {
	s := startScriptedSMTP(t, "250")
	api, _ := newFeedbackSeamAPI(t, s)

	err := api.sendFeedbackEmail(context.Background(), "t", "bug", "m", "someone@attacker.test", "", []string{"feedback@example.test"}, []string{"ops@example.test"})
	if err != nil {
		t.Fatalf("sendFeedbackEmail: %v", err)
	}
	msgs, _ := s.received()
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
	if !strings.Contains(msgs[0], "Reply-To: someone@attacker.test") {
		t.Errorf("submitter address should be the Reply-To only")
	}
	if attemptHeader(msgs[0]) == "" {
		t.Errorf("feedback mail left without the provider attempt header: it did not cross the authorized seam")
	}
	got := s.recipients()
	if len(got) != 1 || strings.Join(got[0], ",") != "feedback@example.test,ops@example.test" {
		t.Errorf("RCPT TO = %v, want exactly the configured notify set", got)
	}
}

// TestFeedbackSeam_OverlappingNotifyConfigStillSends: TO and CC naming the
// same mailbox (in any case) is a legal configuration that used to send one
// copy; the seam's canonical recipient set keeps it that way instead of
// refusing every attempt.
func TestFeedbackSeam_OverlappingNotifyConfigStillSends(t *testing.T) {
	s := startScriptedSMTP(t, "250")
	api, _ := newFeedbackSeamAPI(t, s)

	err := api.sendFeedbackEmail(context.Background(), "t", "bug", "m", "", "", []string{"ops@example.test"}, []string{"Ops@example.test"})
	if err != nil {
		t.Fatalf("sendFeedbackEmail: %v", err)
	}
	got := s.recipients()
	if len(got) != 1 || len(got[0]) != 1 || !strings.EqualFold(got[0][0], "ops@example.test") {
		t.Fatalf("RCPT TO = %v, want the one mailbox once", got)
	}
}
