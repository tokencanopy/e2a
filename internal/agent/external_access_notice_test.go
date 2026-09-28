package agent

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tokencanopy/e2a/internal/config"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/limits"
	"github.com/tokencanopy/e2a/internal/outbound"
	"github.com/tokencanopy/e2a/internal/sendingpolicy"
	"github.com/tokencanopy/e2a/internal/testutil/testdb"
	"github.com/tokencanopy/e2a/internal/usage"
)

// The operator notice's account-facts block is composed by
// NotifySendingAccessRequest in external_access.go; these tests drive it
// through the same real-Postgres + scripted-SMTP seam feedback_seam_test.go
// uses for the base notice, so the wire body is asserted end to end. Every
// address and account here is synthetic.

// noticeFactsPolicy arms external sending access in enforce mode with a
// cutoff old enough that every test account (created "now") falls inside the
// cohort.
func noticeFactsPolicy() sendingpolicy.RuntimePolicy {
	p := sendingpolicy.DisabledPolicy()
	p.ExternalSendingAccess = &sendingpolicy.ExternalSendingAccessPolicy{Mode: sendingpolicy.ModeEnforce, AccountsCreatedAtOrAfter: "1970-01-01T00:00:00Z"}
	return p
}

// newNoticeAPI is newFeedbackSeamAPI (feedback_seam_test.go) with a real
// identity.Store wired — NotifySendingAccessRequest's account-facts block
// needs to read the store, unlike the base feedback seam it shares.
func newNoticeAPI(t *testing.T, s *scriptedSMTP) (*API, *pgxpool.Pool, *identity.Store) {
	t.Helper()
	pool := testdb.TestDB(t)
	store := identity.NewStore(pool)
	relay := outbound.NewSMTPRelay(&config.OutboundSMTPConfig{Host: s.host, Port: s.port})
	api := NewAPI(store, outbound.NewSender(relay, "test.e2a.dev"), relay, nil, usage.NewNoopUsageTracker(), "e2a.dev", "test.e2a.dev", "agents.e2a.dev", "", false)
	gate := sendingpolicy.NewGate(pool, sendingpolicy.Secrets{}, sendingpolicy.PolicySourceConfig, sendingpolicy.DisabledPolicy())
	api.SetProviderSubmitter(outbound.NewProviderSubmitter(relay, gate), gate)
	return api, pool, store
}

// TestNotifySendingAccessRequestAccountFactsBlock: every server-owned fact —
// owner identity, signed-up age, a non-standard account class, plan, owner
// mailbox verification, live/verified resource counts, a paused sending
// state with its class, and prior decided request history — appears in the
// operator-authored section, above the untrusted fence, without disturbing
// the pre-existing lines or the fence itself.
func TestNotifySendingAccessRequestAccountFactsBlock(t *testing.T) {
	s := startScriptedSMTP(t, "250")
	api, pool, store := newNoticeAPI(t, s)
	module := sendingpolicy.NewPolicyModule(pool, sendingpolicy.Secrets{}, sendingpolicy.PolicySourceConfig, noticeFactsPolicy())
	api.SetExternalAccess(module)
	ctx := context.Background()

	user, err := store.CreateOrGetUser(ctx, "owner-notice-full@example.com", "Jane Q. Doe", "google-notice-full")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	if err := store.SetAccountClass(ctx, user.ID, "demo"); err != nil {
		t.Fatalf("SetAccountClass: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET owner_email_verified_at = now(), owner_email_verified_address = lower(email),
		owner_email_verified_source = 'google_oauth' WHERE id = $1`, user.ID); err != nil {
		t.Fatalf("prove owner: %v", err)
	}
	if err := limits.NewStore(pool).Upsert(ctx, user.ID, limits.Limits{
		PlanCode: "pro", MaxAgents: 25, MaxDomains: 10, MaxMessagesMonth: 100000, MaxStorageBytes: 1 << 30,
	}); err != nil {
		t.Fatalf("seed account_limits: %v", err)
	}

	domain := "noticefull.example.com"
	if _, err := store.ClaimOrCreateDomain(ctx, domain, user.ID); err != nil {
		t.Fatalf("claim verified domain: %v", err)
	}
	if err := store.VerifyDomain(ctx, domain, user.ID); err != nil {
		t.Fatalf("verify domain: %v", err)
	}
	if _, err := store.ClaimOrCreateDomain(ctx, "noticefull-unverified.example.com", user.ID); err != nil {
		t.Fatalf("claim unverified domain: %v", err)
	}
	if _, err := store.CreateAgent(ctx, "live1@"+domain, domain, "", "", "local", user.ID); err != nil {
		t.Fatalf("create live agent 1: %v", err)
	}
	if _, err := store.CreateAgent(ctx, "live2@"+domain, domain, "", "", "local", user.ID); err != nil {
		t.Fatalf("create live agent 2: %v", err)
	}
	trashed, err := store.CreateAgent(ctx, "trashed@"+domain, domain, "", "", "local", user.ID)
	if err != nil {
		t.Fatalf("create trashed agent: %v", err)
	}
	if err := store.SoftDeleteAgent(ctx, trashed.ID, user.ID); err != nil {
		t.Fatalf("trash agent: %v", err)
	}
	if _, err := module.SetAccountPause(ctx, sendingpolicy.AccountPauseChange{
		AccountID: user.ID, Paused: true, Class: sendingpolicy.PauseClassAbuse, Actor: "cli:test", Reason: "test pause",
	}); err != nil {
		t.Fatalf("pause account: %v", err)
	}

	in := sendingpolicy.AccessRequestInput{UseCase: "sending order receipts", Recipients: "our customers", ExpectedDailyVolume: 40}
	appeal1, _, err := module.SubmitAccessRequest(ctx, user.ID, in)
	if err != nil {
		t.Fatalf("submit 1: %v", err)
	}
	if err := module.DeclineExternalAccessRequest(ctx, user.ID, appeal1.ID, "cli:test"); err != nil {
		t.Fatalf("decline 1: %v", err)
	}
	appeal2, _, err := module.SubmitAccessRequest(ctx, user.ID, in)
	if err != nil {
		t.Fatalf("submit 2: %v", err)
	}
	if err := module.DeclineExternalAccessRequest(ctx, user.ID, appeal2.ID, "cli:test"); err != nil {
		t.Fatalf("decline 2: %v", err)
	}
	current, _, err := module.SubmitAccessRequest(ctx, user.ID, in)
	if err != nil {
		t.Fatalf("submit current: %v", err)
	}

	t.Setenv("FEEDBACK_NOTIFY_TO", "ops@example.test")
	api.NotifySendingAccessRequest(ctx, user.ID, current)

	msgs, _ := s.received()
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
	body := msgs[0]

	wantLines := []string{
		"Account: " + user.ID,
		"Request: " + current.ID,
		"Expected daily volume: 40",
		"Owner: owner-notice-full@example.com (Jane Q. Doe)",
		"Signed up: " + user.CreatedAt.UTC().Format(time.RFC3339) + " (0 days ago)",
		"Account class: demo",
		"Plan: pro",
		"Owner mailbox verified: yes",
		"Agents: 2 live",
		"Domains: 1 verified",
		"Sending: paused (abuse)",
		"Prior requests: 2 decided (last: declined)",
		"Review, then decide with the audited local command:",
		"  e2a -inspect-external-sending -account-id " + user.ID,
		"--- customer-supplied (untrusted) ---",
		"> sending order receipts",
		"> our customers",
	}
	for _, want := range wantLines {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\nfull body:\n%s", want, body)
		}
	}

	// The facts block sits between "Expected daily volume" and "Review,
	// then decide", entirely above the untrusted fence.
	volIdx := strings.Index(body, "Expected daily volume:")
	ownerIdx := strings.Index(body, "Owner: ")
	reviewIdx := strings.Index(body, "Review, then decide")
	fenceIdx := strings.Index(body, "--- customer-supplied")
	if volIdx < 0 || ownerIdx < 0 || reviewIdx < 0 || fenceIdx < 0 || !(volIdx < ownerIdx && ownerIdx < reviewIdx && reviewIdx < fenceIdx) {
		t.Fatalf("facts block is not positioned between the volume line and the command block, above the fence: vol=%d owner=%d review=%d fence=%d", volIdx, ownerIdx, reviewIdx, fenceIdx)
	}
}

// TestNotifySendingAccessRequestDisplayNameNeutralized: a display name
// carrying a newline and a bidi override character must not fabricate an
// extra line or visually reorder the operator section.
func TestNotifySendingAccessRequestDisplayNameNeutralized(t *testing.T) {
	s := startScriptedSMTP(t, "250")
	api, pool, store := newNoticeAPI(t, s)
	module := sendingpolicy.NewPolicyModule(pool, sendingpolicy.Secrets{}, sendingpolicy.PolicySourceConfig, noticeFactsPolicy())
	api.SetExternalAccess(module)
	ctx := context.Background()

	rawName := "Evil\u202Eexample\ndoe" // a right-to-left override plus an embedded newline
	user, err := store.CreateOrGetUser(ctx, "owner-notice-name@example.com", rawName, "google-notice-name")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	req, _, err := module.SubmitAccessRequest(ctx, user.ID, sendingpolicy.AccessRequestInput{UseCase: "x", Recipients: "y", ExpectedDailyVolume: 1})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	t.Setenv("FEEDBACK_NOTIFY_TO", "ops@example.test")
	api.NotifySendingAccessRequest(ctx, user.ID, req)

	msgs, _ := s.received()
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
	body := msgs[0]

	if strings.Contains(body, "\u202E") {
		t.Error("body must not carry the raw bidi override character")
	}
	wantOwner := "Owner: owner-notice-name@example.com (Evilexample doe)"
	if !strings.Contains(body, wantOwner) {
		t.Errorf("body does not contain the neutralized owner line %q\nfull body:\n%s", wantOwner, body)
	}
	// The name's embedded newline must not have split the operator section
	// into an extra line — the owner line is exactly one line.
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "Owner: ") && line != wantOwner {
			t.Errorf("owner line = %q, want exactly %q on its own line", line, wantOwner)
		}
	}
}

// TestNotifySendingAccessRequestDisplayNameCapped: an overlong display name
// is capped rather than left to grow the notice unbounded.
func TestNotifySendingAccessRequestDisplayNameCapped(t *testing.T) {
	got := SanitizeOperatorLineForTest(strings.Repeat("x", 200), operatorNoticeNameMaxRunes)
	if len([]rune(got)) != operatorNoticeNameMaxRunes {
		t.Fatalf("sanitized length = %d, want %d", len([]rune(got)), operatorNoticeNameMaxRunes)
	}
}

// TestNotifySendingAccessRequestFactsReadFailureSendsBaseNotice: when the
// account facts cannot be read (here, because the account does not exist),
// the base notice still sends in full — operator lines, commands and the
// untrusted fence unchanged — with the facts block simply absent, and a
// warning is logged rather than the request failing.
func TestNotifySendingAccessRequestFactsReadFailureSendsBaseNotice(t *testing.T) {
	s := startScriptedSMTP(t, "250")
	api, pool, _ := newNoticeAPI(t, s)
	module := sendingpolicy.NewPolicyModule(pool, sendingpolicy.Secrets{}, sendingpolicy.PolicySourceConfig, noticeFactsPolicy())
	api.SetExternalAccess(module)

	var logBuf bytes.Buffer
	prevOut := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(&logBuf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})

	req := sendingpolicy.AccessRequest{ID: "esar_missing_user_test", UseCase: "build a bot", Recipients: "customers", ExpectedDailyVolume: 7}
	t.Setenv("FEEDBACK_NOTIFY_TO", "ops@example.test")
	api.NotifySendingAccessRequest(context.Background(), "usr_does_not_exist", req)

	msgs, _ := s.received()
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1 (a failed fact read must still send the base notice)", len(msgs))
	}
	body := msgs[0]
	for _, want := range []string{
		"Account: usr_does_not_exist",
		"Request: esar_missing_user_test",
		"Expected daily volume: 7",
		"Review, then decide with the audited local command:",
		"  e2a -inspect-external-sending -account-id usr_does_not_exist",
		"--- customer-supplied (untrusted) ---",
		"> build a bot",
		"> customers",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("base notice missing %q\nfull body:\n%s", want, body)
		}
	}
	for _, absent := range []string{"Owner:", "Signed up:", "Plan:", "Prior requests:"} {
		if strings.Contains(body, absent) {
			t.Errorf("body should omit the entire facts block on a failed read; found %q:\n%s", absent, body)
		}
	}
	if !strings.Contains(logBuf.String(), "account facts unavailable") {
		t.Errorf("a failed fact read should log a warning; got: %q", logBuf.String())
	}
}
