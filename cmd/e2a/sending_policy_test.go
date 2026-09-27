package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/tokencanopy/e2a/internal/config"
	"github.com/tokencanopy/e2a/internal/sendingpolicy"
	"github.com/tokencanopy/e2a/internal/testutil"
)

// spTestConfig mirrors the generation-zero policy exactly: the ramp block
// carries the hosted 150→2000/30 schedule and every control is disabled, so
// the config-derived hash must equal migration 112's seeded hash. That
// equality is what lets the activation test present a "reviewed" hash it
// computed the same way an operator would.
func spTestConfig() *config.Config {
	return &config.Config{
		SendingRamp: config.SendingRampConfig{
			Enabled:     false,
			StartDaily:  150,
			TargetDaily: 2000,
			RampDays:    30,
		},
		SendingProtect: config.SendingProtectionConfig{
			RuntimePolicySource:              "config",
			BudgetMode:                       "disabled",
			DetectorMode:                     "disabled",
			TenantHeaderMode:                 "disabled",
			TenantProvisioningMode:           "disabled",
			TenantSuppressionSyncMode:        "disabled",
			DefaultAccountDailyRecipients:    100,
			SharedDomainAccountDaily:         50,
			ProbationGlobalDailyRecipients:   150,
			AllCustomerGlobalDailyRecipients: 5000,
			CriticalOperationalDaily:         100,
			ViolationOperationalDaily:        100,
			DailyUnlimitedPlanCodes:          []string{"starter", "pro", "scale"},
			BudgetHoldMaxDays:                7,
			BouncePauseBasisPoints:           400,
			ComplaintPauseBasisPoints:        8,
			BounceMinOutcomes:                50,
			SharedReputationBounceMin:        1,
			DetectorIntervalSeconds:          300,
			DetectorWindowDays:               7,
			AuditRetentionDays:               90,
			FeedbackPostAccountRetention:     30,
			OperatorNoticeRecipientVersion:   1,
		},
	}
}

const (
	spTestCommitmentKey = "AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI"
	spPolicyOperatorMap = `{"commitment_key":"` + spTestCommitmentKey + `","recipients":{"1":"policy-operator@example.test"}}`
	spTestOperatorMap   = `{"commitment_key":"` + spTestCommitmentKey + `","recipients":{"910001":"cmd-operator@example.test"}}`
)

func spBillingDigest(hexDigit string) string {
	return "sha256:" + strings.Repeat(hexDigit, 64)
}

func TestSendingProtectionCommands(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	cfg := spTestConfig()
	module := sendingpolicy.NewModule(pool, sendingpolicy.Secrets{})

	run := func(f *sendingProtectionFlags) (string, error) {
		var out bytes.Buffer
		policy, err := sendingpolicy.FromConfig(cfg)
		if err != nil {
			return "", err
		}
		source, err := sendingpolicy.SourceFromConfig(cfg)
		if err != nil {
			return "", err
		}
		secrets, err := sendingpolicy.LoadSecretsFromEnv(source, policy)
		if err != nil {
			return "", err
		}
		err = runSendingProtectionCommand(ctx, cfg, pool, secrets, f, &out)
		return out.String(), err
	}

	t.Run("exactly one command", func(t *testing.T) {
		if _, err := run(&sendingProtectionFlags{}); err == nil {
			t.Fatal("no command selected must be rejected")
		}
		if _, err := run(&sendingProtectionFlags{inspect: true, capabilities: true}); err == nil {
			t.Fatal("two commands must be rejected")
		}
	})

	// The dry-run readback drives everything else: it tells the operator the
	// stored generation and the hash this config would activate.
	var configHash string
	t.Run("inspect", func(t *testing.T) {
		out, err := run(&sendingProtectionFlags{inspect: true})
		if err != nil {
			t.Fatalf("inspect: %v", err)
		}
		for _, want := range []string{
			"stored_generation:",
			"config_policy_sha256:",
			"config_policy_canonical:",
			"runtime_attestation_revision:",
			"runtime_attestation_sha256:",
			"active_billing_digest:",
			"active_billing_contract:",
			"rollback_billing_digest:",
			"rollback_billing_contract:",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("inspect output missing %q", want)
			}
		}
		policy, err := sendingpolicy.FromConfig(cfg)
		if err != nil {
			t.Fatalf("policy from config: %v", err)
		}
		configHash, err = sendingpolicy.Hash(policy)
		if err != nil {
			t.Fatalf("hash: %v", err)
		}
		if !strings.Contains(out, configHash) {
			t.Errorf("inspect must print the config policy hash %s", configHash)
		}
	})

	t.Run("activate requires explicit arguments", func(t *testing.T) {
		for name, f := range map[string]*sendingProtectionFlags{
			"missing reason":     {activate: true, expectedGeneration: 0, expectedPolicySHA: configHash},
			"missing generation": {activate: true, expectedGeneration: -1, expectedPolicySHA: configHash, reason: "r"},
			"missing hash":       {activate: true, expectedGeneration: 0, reason: "r"},
		} {
			t.Run(name, func(t *testing.T) {
				if _, err := run(f); err == nil {
					t.Fatal("expected rejection")
				}
			})
		}
	})

	t.Run("invalid reviewed hashes are rejected without echoing input", func(t *testing.T) {
		injected := "not-a-hash\nforged-log-line"
		_, err := run(&sendingProtectionFlags{
			activate:           true,
			expectedGeneration: 0,
			expectedPolicySHA:  injected,
			reason:             "invalid hash",
		})
		if err == nil {
			t.Fatal("expected malformed policy hash to be rejected")
		}
		if strings.Contains(err.Error(), "forged-log-line") {
			t.Errorf("policy hash error echoed untrusted input: %v", err)
		}

		_, err = run(&sendingProtectionFlags{
			attest:                      true,
			expectedAttestationRevision: 0,
			expectedAttestationSHA:      injected,
			activeBillingContract:       0,
			rollbackBillingContract:     0,
			reason:                      "invalid hash",
		})
		if err == nil {
			t.Fatal("expected malformed attestation hash to be rejected")
		}
		if strings.Contains(err.Error(), "forged-log-line") {
			t.Errorf("attestation hash error echoed untrusted input: %v", err)
		}
	})

	t.Run("activate rejects a wrong reviewed hash with zero writes", func(t *testing.T) {
		t.Setenv(sendingpolicy.EnvOperatorRecipientsMap, spPolicyOperatorMap)
		before, err := module.InspectPolicy(ctx)
		if err != nil {
			t.Fatalf("inspect: %v", err)
		}
		_, err = run(&sendingProtectionFlags{
			activate:           true,
			expectedGeneration: before.Generation,
			expectedPolicySHA:  strings.Repeat("0", 64),
			reason:             "wrong hash",
		})
		if err == nil || !strings.Contains(err.Error(), "zero writes") {
			t.Fatalf("expected the reviewed-hash mismatch, got %v", err)
		}
		after, err := module.InspectPolicy(ctx)
		if err != nil {
			t.Fatalf("re-inspect: %v", err)
		}
		if after.Generation != before.Generation {
			t.Error("a rejected activation advanced the generation")
		}
	})

	t.Run("activate with the reviewed hash advances one generation", func(t *testing.T) {
		t.Setenv(sendingpolicy.EnvOperatorRecipientsMap, spPolicyOperatorMap)
		recipients, err := sendingpolicy.LoadOperatorRecipients(spPolicyOperatorMap)
		if err != nil {
			t.Fatalf("load policy operator map: %v", err)
		}
		activationModule := sendingpolicy.NewModule(pool, sendingpolicy.Secrets{Recipients: recipients})
		if _, err := activationModule.RegisterOperatorRecipients(ctx, "cmd-test-bootstrap", "register selected policy operator"); err != nil {
			t.Fatalf("register selected policy operator: %v", err)
		}
		before, err := module.InspectPolicy(ctx)
		if err != nil {
			t.Fatalf("inspect: %v", err)
		}
		out, err := run(&sendingProtectionFlags{
			activate:           true,
			expectedGeneration: before.Generation,
			expectedPolicySHA:  strings.ToUpper(configHash), // case-insensitive review input
			reason:             "cmd-test activation",
		})
		if err != nil {
			t.Fatalf("activate: %v", err)
		}
		if !strings.Contains(out, "activated_generation:") {
			t.Errorf("activation output missing the new generation: %s", out)
		}
		after, err := module.InspectPolicy(ctx)
		if err != nil {
			t.Fatalf("re-inspect: %v", err)
		}
		if after.Generation != before.Generation+1 {
			t.Errorf("generation = %d, want %d", after.Generation, before.Generation+1)
		}
		if after.PolicySHA256 != configHash {
			t.Errorf("stored hash = %s, want the reviewed %s", after.PolicySHA256, configHash)
		}
	})

	t.Run("register is gated on the env map and idempotent", func(t *testing.T) {
		if _, err := run(&sendingProtectionFlags{register: true}); err == nil {
			t.Fatal("register without a reason must be rejected")
		}
		clearEnvForTest(t)
		if _, err := run(&sendingProtectionFlags{register: true, reason: "r"}); err == nil {
			t.Fatal("register without the env map must be rejected")
		}

		t.Setenv(sendingpolicy.EnvOperatorRecipientsMap, spTestOperatorMap)
		out, err := run(&sendingProtectionFlags{register: true, reason: "initial synthetic registry"})
		if err != nil {
			t.Fatalf("register: %v", err)
		}
		if !strings.Contains(out, "910001") || strings.Contains(out, "cmd-operator@") {
			t.Errorf("register must print the version and never the mailbox: %s", out)
		}

		replay, err := run(&sendingProtectionFlags{register: true, reason: "replay"})
		if err != nil {
			t.Fatalf("replay: %v", err)
		}
		if !strings.Contains(replay, "idempotent replay") {
			t.Errorf("replay output should say it wrote nothing: %s", replay)
		}
	})

	t.Run("attest CAS round-trip", func(t *testing.T) {
		current, err := module.InspectAttestation(ctx)
		if err != nil {
			t.Fatalf("inspect attestation: %v", err)
		}
		currentHash, err := sendingpolicy.AttestationHash(current)
		if err != nil {
			t.Fatalf("hash attestation: %v", err)
		}

		if _, err := run(&sendingProtectionFlags{
			attest:                      true,
			expectedAttestationRevision: current.Revision,
			expectedAttestationSHA:      currentHash,
			activeBillingContract:       -1, // missing contracts must be rejected
			rollbackBillingContract:     -1,
			reason:                      "missing contracts",
		}); err == nil {
			t.Fatal("attest without explicit contracts must be rejected")
		}

		out, err := run(&sendingProtectionFlags{
			attest:                      true,
			expectedAttestationRevision: current.Revision,
			expectedAttestationSHA:      currentHash,
			activeBillingDigest:         spBillingDigest("a"),
			activeBillingContract:       1,
			rollbackBillingDigest:       spBillingDigest("b"),
			rollbackBillingContract:     1,
			reason:                      "cmd-test attestation",
		})
		if err != nil {
			t.Fatalf("attest: %v", err)
		}
		if !strings.Contains(out, "attestation_revision:") || !strings.Contains(out, "attestation_sha256:") {
			t.Errorf("attest output missing revision/hash: %s", out)
		}
		after, err := module.InspectAttestation(ctx)
		if err != nil {
			t.Fatalf("re-inspect: %v", err)
		}
		if after.Revision != current.Revision+1 {
			t.Errorf("revision = %d, want %d", after.Revision, current.Revision+1)
		}
	})

	t.Run("reconcile-legacy-sending-jobs dispatches", func(t *testing.T) {
		resetRiverJobs(t, pool)
		out, err := run(&sendingProtectionFlags{reconcile: true})
		if err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if !strings.Contains(out, "scanned:   0") || !strings.Contains(out, "remaining: 0") {
			t.Errorf("reconcile output = %q", out)
		}
	})

	t.Run("print-capabilities", func(t *testing.T) {
		clearEnvForTest(t)
		out, err := run(&sendingProtectionFlags{capabilities: true})
		if err != nil {
			t.Fatalf("capabilities: %v", err)
		}
		for _, want := range []string{`"sending_protection_contract":0`, `"runtime_policy_source":"config"`, `"operator_notice_recipient_commitments":{}`, `"runtime_policy_features":["external_sending_access","external_sending_unlocks"]`} {
			if !strings.Contains(out, want) {
				t.Errorf("capabilities missing %s in %s", want, out)
			}
		}

		t.Setenv(sendingpolicy.EnvOperatorRecipientsMap, spTestOperatorMap)
		withMap, err := run(&sendingProtectionFlags{capabilities: true})
		if err != nil {
			t.Fatalf("capabilities with map: %v", err)
		}
		if !strings.Contains(withMap, `"910001":"`) {
			t.Errorf("capabilities should carry the commitment for version 910001: %s", withMap)
		}
		if strings.Contains(withMap, "example.test") || strings.Contains(withMap, spTestCommitmentKey) {
			t.Error("capabilities must never carry an address or key material")
		}
	})
}

// clearEnvForTest unsets both secret variables for the enclosing test,
// restoring whatever the shell carried afterwards.
func clearEnvForTest(t *testing.T) {
	t.Helper()
	for _, key := range []string{sendingpolicy.EnvFeedbackHMACKeys, sendingpolicy.EnvOperatorRecipientsMap} {
		t.Setenv(key, "placeholder")
		if err := unsetEnvKey(key); err != nil {
			t.Fatalf("unset %s: %v", key, err)
		}
	}
}

func unsetEnvKey(key string) error { return os.Unsetenv(key) }

func TestExternalSendingOperatorCommands(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	cfg := spTestConfig()
	cfg.SendingProtect.ExternalSendingAccess = &config.ExternalSendingAccessConfig{Mode: "enforce", AccountsCreatedAtOrAfter: "2026-01-01T00:00:00Z"}
	clearEnvForTest(t)
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, email, google_subject) VALUES ('usr_cmd_esa', 'owner@cmd-esa.example.test', 'sub-cmd-esa')`); err != nil {
		t.Fatal(err)
	}
	run := func(f *sendingProtectionFlags) (string, error) {
		var out bytes.Buffer
		err := runSendingProtectionCommand(ctx, cfg, pool, sendingpolicy.Secrets{}, f, &out)
		return out.String(), err
	}

	if _, err := run(&sendingProtectionFlags{inspectExternal: true}); err == nil {
		t.Fatal("a missing -account-id must be refused")
	}
	out, err := run(&sendingProtectionFlags{inspectExternal: true, accountID: "usr_cmd_esa"})
	if err != nil || !strings.Contains(out, "external_sending_approved: false") || !strings.Contains(out, "external_sending_revision: 0") || !strings.Contains(out, "enforcement_applies:      true") {
		t.Fatalf("inspect = %q err=%v", out, err)
	}
	if strings.Contains(out, "@") {
		t.Fatalf("the readback must never print an address: %q", out)
	}
	if _, err := run(&sendingProtectionFlags{approveExternal: true, accountID: "usr_cmd_esa", expectedExternal: 0}); err == nil {
		t.Fatal("an approval without -reason must be refused")
	}
	if _, err := run(&sendingProtectionFlags{approveExternal: true, accountID: "usr_cmd_esa", expectedExternal: -1, reason: "x"}); err == nil {
		t.Fatal("an approval without the expected revision must be refused")
	}
	if _, err := run(&sendingProtectionFlags{approveExternal: true, accountID: "usr_cmd_esa", expectedExternal: 3, reason: "x"}); !errors.Is(err, sendingpolicy.ErrStaleExternalAccessRevision) {
		t.Fatalf("stale approval err = %v", err)
	}
	out, err = run(&sendingProtectionFlags{approveExternal: true, accountID: "usr_cmd_esa", expectedExternal: 0, reason: "reviewed request"})
	if err != nil || !strings.Contains(out, "external_sending_approved: true") || !strings.Contains(out, "external_sending_revision: 1") {
		t.Fatalf("approve = %q err=%v", out, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO account_limits (user_id, plan_code, max_agents, max_domains, max_messages_month, max_storage_bytes, external_sending_entitled) VALUES ('usr_cmd_esa', 'pro', 1, 1, 1, 1, true)`); err != nil {
		t.Fatal(err)
	}
	out, err = run(&sendingProtectionFlags{revokeExternal: true, accountID: "usr_cmd_esa", expectedExternal: 1, reason: "abuse report"})
	if err != nil || !strings.Contains(out, "external_sending_approved: false") || !strings.Contains(out, "paid-base entitlement") {
		t.Fatalf("revoke = %q err=%v", out, err)
	}
}

type fakeDecisionNotifier struct {
	calls []string
	err   error
}

func (f *fakeDecisionNotifier) NotifyDecision(_ context.Context, requestID string) error {
	f.calls = append(f.calls, requestID)
	return f.err
}

// The decision notice goes out only for a decided request, after the
// decision committed; a failed notice is a warning line and never fails the
// command (the decision stands).
func TestExternalSendingDecisionNotice(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	clearEnvForTest(t)
	policy := sendingpolicy.DisabledPolicy()
	policy.ExternalSendingAccess = &sendingpolicy.ExternalSendingAccessPolicy{Mode: sendingpolicy.ModeEnforce, AccountsCreatedAtOrAfter: "1970-01-01T00:00:00Z",
		Unlocks: []sendingpolicy.ExternalUnlock{sendingpolicy.UnlockOperatorApproval}}
	module := sendingpolicy.NewPolicyModule(pool, sendingpolicy.Secrets{}, sendingpolicy.PolicySourceConfig, policy)
	newRequest := func(user string) string {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO users (id, email, google_subject) VALUES ($1, $1 || '@notice-cmd.example.test', 'sub-' || $1)`, user); err != nil {
			t.Fatal(err)
		}
		req, created, err := module.SubmitAccessRequest(ctx, user, sendingpolicy.AccessRequestInput{UseCase: "synthetic", Recipients: "synthetic", ExpectedDailyVolume: 1})
		if err != nil || !created {
			t.Fatalf("submit: %v", err)
		}
		return req.ID
	}

	// Decline, notice fails: the command succeeds and prints a warning.
	reqA := newRequest("usr_notice_cmd_a")
	failing := &fakeDecisionNotifier{err: errors.New("relay refused")}
	var out bytes.Buffer
	if err := runExternalSendingCommand(ctx, module, failing, &sendingProtectionFlags{declineExternal: true, accountID: "usr_notice_cmd_a", requestID: reqA}, &out); err != nil {
		t.Fatalf("decline with a failing notice must still succeed: %v", err)
	}
	if len(failing.calls) != 1 || failing.calls[0] != reqA || !strings.Contains(out.String(), "warning:") || !strings.Contains(out.String(), "decision stands") {
		t.Fatalf("calls=%v out=%q", failing.calls, out.String())
	}
	if latest, err := module.LatestAccessRequest(ctx, "usr_notice_cmd_a"); err != nil || latest.State != "declined" {
		t.Fatalf("the decline must have committed: %+v %v", latest, err)
	}

	// Approve deciding a request: notice sent.
	reqB := newRequest("usr_notice_cmd_b")
	ok := &fakeDecisionNotifier{}
	out.Reset()
	if err := runExternalSendingCommand(ctx, module, ok, &sendingProtectionFlags{approveExternal: true, accountID: "usr_notice_cmd_b", expectedExternal: 0, reason: "reviewed", requestID: reqB}, &out); err != nil {
		t.Fatal(err)
	}
	if len(ok.calls) != 1 || ok.calls[0] != reqB || !strings.Contains(out.String(), "decision_notice:          sent") {
		t.Fatalf("calls=%v out=%q", ok.calls, out.String())
	}
	if !strings.Contains(out.String(), "available_unlocks:        operator_approval") {
		t.Fatalf("readback must show the unlock set: %q", out.String())
	}

	// A direct grant with no request (pre-granting before a rollout) sends
	// nothing.
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, email, google_subject) VALUES ('usr_notice_cmd_c', 'c@notice-cmd.example.test', 'sub-c')`); err != nil {
		t.Fatal(err)
	}
	direct := &fakeDecisionNotifier{}
	out.Reset()
	if err := runExternalSendingCommand(ctx, module, direct, &sendingProtectionFlags{approveExternal: true, accountID: "usr_notice_cmd_c", expectedExternal: 0, reason: "pre-grant"}, &out); err != nil {
		t.Fatal(err)
	}
	if len(direct.calls) != 0 {
		t.Fatalf("a direct grant must not send a decision notice: %v", direct.calls)
	}

	// No relay configured: warning, success.
	reqD := newRequest("usr_notice_cmd_d")
	out.Reset()
	if err := runExternalSendingCommand(ctx, module, nil, &sendingProtectionFlags{declineExternal: true, accountID: "usr_notice_cmd_d", requestID: reqD}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "no outbound SMTP relay is configured") {
		t.Fatalf("out=%q", out.String())
	}
}

func TestListExternalSendingRequestsAndPendingWarning(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	clearEnvForTest(t)
	cfg := spTestConfig()
	cfg.SendingProtect.ExternalSendingAccess = &config.ExternalSendingAccessConfig{Mode: "enforce", AccountsCreatedAtOrAfter: "1970-01-01T00:00:00Z",
		Unlocks: []string{"operator_approval"}}
	policy, err := sendingpolicy.FromConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	module := sendingpolicy.NewPolicyModule(pool, sendingpolicy.Secrets{}, sendingpolicy.PolicySourceConfig, policy)
	file := func(user string) string {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO users (id, email, google_subject) VALUES ($1, $1 || '@list-cmd.example.test', 'sub-' || $1)`, user); err != nil {
			t.Fatal(err)
		}
		req, _, err := module.SubmitAccessRequest(ctx, user, sendingpolicy.AccessRequestInput{UseCase: "synthetic secret use case", Recipients: "synthetic", ExpectedDailyVolume: 42})
		if err != nil {
			t.Fatal(err)
		}
		return req.ID
	}
	reqA := file("usr_list_a")
	reqB := file("usr_list_b")
	if err := module.DeclineExternalAccessRequest(ctx, "usr_list_b", reqB, "cli:test"); err != nil {
		t.Fatal(err)
	}
	run := func(f *sendingProtectionFlags) string {
		t.Helper()
		var out bytes.Buffer
		if err := runSendingProtectionCommand(ctx, cfg, pool, sendingpolicy.Secrets{}, f, &out); err != nil {
			t.Fatalf("command: %v", err)
		}
		return out.String()
	}

	out := run(&sendingProtectionFlags{listExternal: true})
	for _, want := range []string{"requests (pending, oldest first):           1", "request_id:               " + reqA, "account_id:               usr_list_a",
		"state:                    pending", "expected_daily_volume:    42", "external_sending_approved: false", "created_at:"} {
		if !strings.Contains(out, want) {
			t.Fatalf("pending listing missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, reqB) {
		t.Fatalf("a decided request must not appear without -all:\n%s", out)
	}
	if strings.Contains(out, "secret use case") || strings.Contains(out, "@") {
		t.Fatalf("the listing must not print customer text or addresses:\n%s", out)
	}
	all := run(&sendingProtectionFlags{listExternal: true, listAll: true})
	if !strings.Contains(all, "requests (all, newest first):           2") || strings.Index(all, reqB) > strings.Index(all, reqA) || strings.Contains(all, "truncated:") || !strings.Contains(all, "state:                    declined") || !strings.Contains(all, "decided_at:") {
		t.Fatalf("-all listing:\n%s", all)
	}

	// A direct grant while a request is pending warns and does not decide it.
	var buf bytes.Buffer
	if err := runExternalSendingCommand(ctx, module, &fakeDecisionNotifier{}, &sendingProtectionFlags{approveExternal: true, accountID: "usr_list_a", expectedExternal: 0, reason: "direct"}, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "warning:                  request "+reqA+" is still pending; re-run with -external-sending-request-id "+reqA) {
		t.Fatalf("missing pending warning:\n%s", buf.String())
	}
	if latest, _ := module.LatestAccessRequest(ctx, "usr_list_a"); latest == nil || latest.State != "pending" {
		t.Fatalf("a direct grant must not auto-decide the request: %+v", latest)
	}
}

func TestAllFlagRequiresListCommand(t *testing.T) {
	f := &sendingProtectionFlags{listAll: true}
	if err := f.validateStandalone(); err == nil {
		t.Fatal("-all without -list-external-sending-requests must be rejected before the server starts")
	}
	if err := (&sendingProtectionFlags{listAll: true, listExternal: true}).validateStandalone(); err != nil {
		t.Fatalf("-all with the list command is valid: %v", err)
	}
	if err := (&sendingProtectionFlags{}).validateStandalone(); err != nil {
		t.Fatalf("no flags is valid: %v", err)
	}
}

func TestListExternalSendingRequestsReportsTruncation(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	module := sendingpolicy.NewPolicyModule(pool, sendingpolicy.Secrets{}, sendingpolicy.PolicySourceConfig, sendingpolicy.DisabledPolicy())
	if _, err := pool.Exec(ctx, `
		INSERT INTO users (id, email, google_subject)
		SELECT 'usr_trunc_' || g, 'usr_trunc_' || g || '@trunc.example.test', 'sub_trunc_' || g FROM generate_series(1, $1) AS g`,
		sendingpolicy.MaxAccessRequestListing+1); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO external_sending_access_requests (id, user_id, use_case, recipients, expected_daily_volume, created_at)
		SELECT 'esar_trunc_' || g, 'usr_trunc_' || g, 'synthetic', 'synthetic', 1, now() - make_interval(secs => g)
		  FROM generate_series(1, $1) AS g`, sendingpolicy.MaxAccessRequestListing+1); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runListExternalRequests(ctx, module, true, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), fmt.Sprintf("truncated:                listing stopped at %d requests", sendingpolicy.MaxAccessRequestListing)) {
		t.Fatal("a listing that hit the bound must say so")
	}
	// Newest first: the most recent request (g=1) is shown, the oldest dropped.
	if !strings.Contains(out.String(), "esar_trunc_1\n") || strings.Contains(out.String(), fmt.Sprintf("esar_trunc_%d\n", sendingpolicy.MaxAccessRequestListing+1)) {
		t.Fatal("-all must keep the newest requests when truncating")
	}
}
