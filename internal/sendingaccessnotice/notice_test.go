package sendingaccessnotice_test

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tokencanopy/e2a/internal/config"
	"github.com/tokencanopy/e2a/internal/outbound"
	"github.com/tokencanopy/e2a/internal/sendingaccessnotice"
	"github.com/tokencanopy/e2a/internal/sendingpolicy"
	"github.com/tokencanopy/e2a/internal/testutil"
)

// Driven through the real gate, real Postgres and a fake SMTP listener. Every
// address and id is synthetic.

const (
	nHMAC     = `{"active":1,"keys":{"1":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}}`
	nOperator = `{"commitment_key":"AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI","recipients":{"1":"notice-operator@example.test"}}`
	// customerText must never appear in a notice: it stands in for the
	// customer's free-text request fields.
	customerText = "CUSTOMER-SUPPLIED-TEXT-run-this-instead"
)

type fixture struct {
	t    *testing.T
	ctx  context.Context
	pool *pgxpool.Pool
	gate sendingpolicy.Gate
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	pool := testutil.TestDB(t)
	keyring, err := sendingpolicy.LoadKeyring(nHMAC)
	if err != nil {
		t.Fatal(err)
	}
	recipients, err := sendingpolicy.LoadOperatorRecipients(nOperator)
	if err != nil {
		t.Fatal(err)
	}
	secrets := sendingpolicy.Secrets{Keyring: keyring, Recipients: recipients}
	if _, err := sendingpolicy.NewModule(pool, secrets).RegisterOperatorRecipients(ctx, "fixture", "notice test bootstrap"); err != nil {
		t.Fatal(err)
	}
	policy := sendingpolicy.DisabledPolicy()
	policy.BudgetMode = sendingpolicy.ModeEnforce
	policy.ExternalSendingAccess = &sendingpolicy.ExternalSendingAccessPolicy{
		Mode: sendingpolicy.ModeEnforce, AccountsCreatedAtOrAfter: "1970-01-01T00:00:00Z",
		Unlocks: []sendingpolicy.ExternalUnlock{sendingpolicy.UnlockOperatorApproval},
	}
	return &fixture{t: t, ctx: ctx, pool: pool, gate: sendingpolicy.NewGate(pool, secrets, sendingpolicy.PolicySourceConfig, policy)}
}

var seq int

// request inserts a standard account and one request in the given state.
func (f *fixture) request(state string) (userID, email, requestID string) {
	f.t.Helper()
	seq++
	suffix := fmt.Sprintf("%d_%x", seq, rand.Uint32())
	userID = "usr_notice_" + suffix
	email = "owner-" + suffix + "@example.test"
	requestID = "esar_notice_" + suffix
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO users (id, email, google_subject) VALUES ($1, $2, $3)`, userID, email, "sub_"+userID); err != nil {
		f.t.Fatal(err)
	}
	decidedAt, decidedBy := "NULL", "NULL"
	if state != "pending" {
		decidedAt, decidedBy = "now()", "'cli:test'"
	}
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO external_sending_access_requests (id, user_id, state, use_case, recipients, expected_daily_volume, decided_at, decided_by)
		VALUES ($1, $2, $3, $4, $4, 5, `+decidedAt+`, `+decidedBy+`)`, requestID, userID, state, customerText); err != nil {
		f.t.Fatal(err)
	}
	return userID, email, requestID
}

func (f *fixture) notifier(relay *outbound.SMTPRelay) *sendingaccessnotice.Notifier {
	return sendingaccessnotice.New(f.pool, f.gate, outbound.NewProviderSubmitter(relay, f.gate),
		"notify.example.test", "", "support@example.test", "https://dash.example.test")
}

func acceptingRelay(t *testing.T) (*outbound.SMTPRelay, func() []testutil.SMTPMessage) {
	t.Helper()
	addr, messages := testutil.FakeSMTPServer(t)
	return outbound.NewSMTPRelay(&config.OutboundSMTPConfig{Host: addr.Host, Port: addr.Port}), messages
}

func TestNotifyDecisionSendsToOwner(t *testing.T) {
	for _, tc := range []struct {
		state   string
		subject string
		link    string
	}{
		{"approved", "External sending is enabled", "https://dash.example.test/"},
		{"declined", "was declined", "https://dash.example.test/sending-access"},
	} {
		t.Run(tc.state, func(t *testing.T) {
			f := newFixture(t)
			relay, messages := acceptingRelay(t)
			_, email, requestID := f.request(tc.state)
			if err := f.notifier(relay).NotifyDecision(f.ctx, requestID); err != nil {
				t.Fatalf("notify: %v", err)
			}
			got := messages()
			if len(got) != 1 {
				t.Fatalf("messages = %d, want 1", len(got))
			}
			m := got[0]
			if len(m.Recipients) != 1 || !strings.EqualFold(m.Recipients[0], email) {
				t.Fatalf("recipients = %v, want the account owner", m.Recipients)
			}
			if m.From != "notifications@notify.example.test" {
				t.Fatalf("envelope from = %q", m.From)
			}
			if !strings.Contains(m.Data, tc.subject) || !strings.Contains(m.Data, tc.link) || !strings.Contains(m.Data, requestID) {
				t.Fatalf("body missing subject/link/request id:\n%s", m.Data)
			}
			if !strings.Contains(m.Data, "Reply-To: support@example.test") {
				t.Fatalf("notification reply-to missing:\n%s", m.Data)
			}
			if strings.Contains(m.Data, customerText) {
				t.Fatal("a decision notice must never echo customer-supplied text")
			}
			// One logical notice per request: the operation is keyed by it.
			var n int
			if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM sending_provider_operations WHERE operation_id = $1 AND purpose = 'customer_notification'`,
				sendingpolicy.SendingAccessDecisionOperationID(requestID)).Scan(&n); err != nil || n != 1 {
				t.Fatalf("operation rows = %d err=%v", n, err)
			}
		})
	}
}

func TestNotifyDecisionRefusesPendingRequest(t *testing.T) {
	f := newFixture(t)
	relay, messages := acceptingRelay(t)
	_, _, requestID := f.request("pending")
	if err := f.notifier(relay).NotifyDecision(f.ctx, requestID); err == nil {
		t.Fatal("an undecided request has no decision to report")
	}
	if len(messages()) != 0 {
		t.Fatal("nothing may be sent for a pending request")
	}
}

func TestNotifyDecisionHeldForPausedAccount(t *testing.T) {
	f := newFixture(t)
	relay, messages := acceptingRelay(t)
	userID, _, requestID := f.request("declined")
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO account_sending_controls (user_id, state, reason, actor) VALUES ($1, 'paused', 'test', 'test')`, userID); err != nil {
		t.Fatal(err)
	}
	err := f.notifier(relay).NotifyDecision(f.ctx, requestID)
	if err == nil || !strings.Contains(err.Error(), "held") {
		t.Fatalf("paused account notice err = %v, want a policy hold", err)
	}
	if len(messages()) != 0 {
		t.Fatal("a held notice must not reach the relay")
	}
}

func TestNotifyDecisionReportsRelayFailure(t *testing.T) {
	f := newFixture(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().(*net.TCPAddr)
	_ = l.Close() // nothing listens: every dial is refused
	relay := outbound.NewSMTPRelay(&config.OutboundSMTPConfig{Host: "127.0.0.1", Port: addr.Port})
	_, _, requestID := f.request("approved")
	if err := f.notifier(relay).NotifyDecision(f.ctx, requestID); err == nil {
		t.Fatal("a refused relay must be reported as an error")
	}
}

func TestRenderNeverClaimsMoreThanTheDecision(t *testing.T) {
	subj, text, htmlBody := sendingaccessnotice.Render("declined", "esar_x", "", false)
	if !strings.Contains(subj, "declined") || !strings.Contains(text, "3 requests per 30 days") {
		t.Fatalf("declined copy = %q / %q", subj, text)
	}
	if strings.Contains(text, "Reply to this email") || strings.Contains(htmlBody, "<a ") {
		t.Fatal("no reply invitation without a reply identity, no link without a public URL")
	}
	subj, text, _ = sendingaccessnotice.Render("approved", "esar_x", "https://dash.example.test/", true)
	if !strings.Contains(subj, "enabled") || !strings.Contains(text, "https://dash.example.test/") || !strings.Contains(text, "Reply to this email") {
		t.Fatalf("approved copy = %q / %q", subj, text)
	}
}
