package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tokencanopy/e2a/internal/sendingpolicy"
)

func readinessPolicySecrets(t *testing.T) sendingpolicy.Secrets {
	t.Helper()
	keys, err := sendingpolicy.LoadKeyring(`{"active":1,"keys":{"1":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}}`)
	if err != nil {
		t.Fatal(err)
	}
	recipients, err := sendingpolicy.LoadOperatorRecipients(spPolicyOperatorMap)
	if err != nil {
		t.Fatal(err)
	}
	return sendingpolicy.Secrets{Keyring: keys, Recipients: recipients}
}

func assertReadiness(t *testing.T, m *readinessMonitor, status int) {
	t.Helper()
	rec := httptest.NewRecorder()
	m.handler()(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != status {
		t.Fatalf("readiness=%d %s, want %d", rec.Code, rec.Body.String(), status)
	}
	if status != http.StatusOK && rec.Body.String() != `{"status":"not_ready","reason":"sending protection policy unavailable"}` {
		t.Fatalf("unbounded readiness error: %s", rec.Body.String())
	}
}

func TestSendingReadinessRequiresStoredPolicyAndRegistry(t *testing.T) {
	ctx := context.Background()
	pool := migratedTestDB(t)
	module := sendingpolicy.NewPolicyModule(pool, readinessPolicySecrets(t), sendingpolicy.PolicySourceDatabase, sendingpolicy.DisabledPolicy())
	m := newReadinessMonitorWithConfig(pool, nil, module, time.Hour, time.Hour, 2*time.Second)
	defer m.Stop()
	// Initial missing registry refuses readiness and does not auto-register.
	assertReadiness(t, m, http.StatusServiceUnavailable)
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sending_operator_recipient_versions`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("readiness wrote registry: %d %v", rows, err)
	}
	if _, err := module.RegisterOperatorRecipients(ctx, "synthetic-operator", "bootstrap registry"); err != nil {
		t.Fatal(err)
	}
	m.evaluate()
	assertReadiness(t, m, http.StatusOK)
	before, err := module.InspectPolicy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE sending_protection_runtime_policy SET policy_sha256=repeat('0',64) WHERE singleton`); err != nil {
		t.Fatal(err)
	}
	m.evaluate()
	// A previously healthy slot must not retain the connectivity grace for a bad policy.
	assertReadiness(t, m, http.StatusServiceUnavailable)
	if _, err = pool.Exec(ctx, `UPDATE sending_protection_runtime_policy SET policy_sha256=$1 WHERE singleton`, before.PolicySHA256); err != nil {
		t.Fatal(err)
	}
	m.evaluate()
	assertReadiness(t, m, http.StatusOK)
	p := before.Policy
	p.OperatorNoticeRecipientVersion = 2
	raw, _ := sendingpolicy.CanonicalBytes(p)
	hash, _ := sendingpolicy.Hash(p)
	if _, err = pool.Exec(ctx, `UPDATE sending_protection_runtime_policy SET policy=$1,policy_sha256=$2 WHERE singleton`, raw, hash); err != nil {
		t.Fatal(err)
	}
	m.evaluate()
	assertReadiness(t, m, http.StatusServiceUnavailable)
}

func TestSendingReadinessRegistryMismatchAndConfigFallback(t *testing.T) {
	pool := migratedTestDB(t)
	secrets := readinessPolicySecrets(t)
	module := sendingpolicy.NewPolicyModule(pool, secrets, sendingpolicy.PolicySourceDatabase, sendingpolicy.DisabledPolicy())
	if _, err := module.RegisterOperatorRecipients(context.Background(), "synthetic-operator", "test"); err != nil {
		t.Fatal(err)
	}
	wrong, err := sendingpolicy.LoadOperatorRecipients(strings.ReplaceAll(spPolicyOperatorMap, "policy-operator@example.test", "other-operator@example.test"))
	if err != nil {
		t.Fatal(err)
	}
	secrets.Recipients = wrong
	for _, source := range []sendingpolicy.PolicySource{sendingpolicy.PolicySourceDatabase, sendingpolicy.PolicySourceConfig} {
		t.Run(string(source), func(t *testing.T) {
			m := newReadinessMonitorWithConfig(pool, nil, sendingpolicy.NewPolicyModule(pool, secrets, source, sendingpolicy.DisabledPolicy()), time.Hour, time.Hour, time.Second)
			defer m.Stop()
			status := http.StatusOK
			if source == sendingpolicy.PolicySourceDatabase {
				status = http.StatusServiceUnavailable
			}
			assertReadiness(t, m, status)
		})
	}
}

func TestSendingReadinessDoesNotBorrowSharedPool(t *testing.T) {
	pool := migratedTestDB(t)
	module := sendingpolicy.NewPolicyModule(pool, readinessPolicySecrets(t), sendingpolicy.PolicySourceDatabase, sendingpolicy.DisabledPolicy())
	if _, err := module.RegisterOperatorRecipients(context.Background(), "synthetic-operator", "test"); err != nil {
		t.Fatal(err)
	}
	m := newReadinessMonitorWithConfig(pool, nil, module, time.Hour, 0, 2*time.Second)
	defer m.Stop()
	held := []*pgxpool.Conn{}
	for i := int32(0); i < pool.Config().MaxConns; i++ {
		conn, err := pool.Acquire(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, conn)
	}
	defer func() {
		for _, conn := range held {
			conn.Release()
		}
	}()
	m.evaluate()
	assertReadiness(t, m, http.StatusOK)
}

func TestSendingReadinessMissingStoredPolicy(t *testing.T) {
	pool := migratedTestDB(t)
	module := sendingpolicy.NewPolicyModule(pool, readinessPolicySecrets(t), sendingpolicy.PolicySourceDatabase, sendingpolicy.DisabledPolicy())
	if _, err := pool.Exec(context.Background(), `DELETE FROM sending_protection_runtime_policy WHERE singleton`); err != nil {
		t.Fatal(err)
	}
	m := newReadinessMonitorWithConfig(pool, nil, module, time.Hour, time.Hour, time.Second)
	defer m.Stop()
	assertReadiness(t, m, http.StatusServiceUnavailable)
}

func TestSendingReadinessRequiresSelectedPermanentVersion(t *testing.T) {
	ctx := context.Background()
	pool := migratedTestDB(t)
	secrets := readinessPolicySecrets(t)
	module := sendingpolicy.NewPolicyModule(pool, secrets, sendingpolicy.PolicySourceDatabase, sendingpolicy.DisabledPolicy())
	if _, err := module.RegisterOperatorRecipients(ctx, "synthetic-operator", "test v1"); err != nil {
		t.Fatal(err)
	}
	superset, err := sendingpolicy.LoadOperatorRecipients(`{"commitment_key":"` + spTestCommitmentKey + `","recipients":{"1":"policy-operator@example.test","2":"next-operator@example.test"}}`)
	if err != nil {
		t.Fatal(err)
	}
	secrets.Recipients = superset
	module = sendingpolicy.NewPolicyModule(pool, secrets, sendingpolicy.PolicySourceDatabase, sendingpolicy.DisabledPolicy())
	p := sendingpolicy.DisabledPolicy()
	p.OperatorNoticeRecipientVersion = 2
	raw, _ := sendingpolicy.CanonicalBytes(p)
	hash, _ := sendingpolicy.Hash(p)
	if _, err = pool.Exec(ctx, `UPDATE sending_protection_runtime_policy SET policy=$1,policy_sha256=$2 WHERE singleton`, raw, hash); err != nil {
		t.Fatal(err)
	}
	m := newReadinessMonitorWithConfig(pool, nil, module, time.Hour, time.Hour, time.Second)
	defer m.Stop()
	assertReadiness(t, m, http.StatusServiceUnavailable)
	if _, err = module.RegisterOperatorRecipients(ctx, "synthetic-operator", "test v2"); err != nil {
		t.Fatal(err)
	}
	m.evaluate()
	assertReadiness(t, m, http.StatusOK)
}
