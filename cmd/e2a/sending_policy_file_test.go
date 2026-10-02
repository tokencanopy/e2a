package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tokencanopy/e2a/internal/sendingpolicy"
	"github.com/tokencanopy/e2a/internal/testutil"
)

func writePolicyFixture(t *testing.T, raw []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "reviewed-policy.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPolicyFileValidation(t *testing.T) {
	p := sendingpolicy.DisabledPolicy()
	raw, _ := sendingpolicy.CanonicalBytes(p)
	good := writePolicyFixture(t, raw)
	got, err := readSendingPolicyFile(good)
	if err != nil {
		t.Fatal(err)
	}
	wantHash, _ := sendingpolicy.Hash(p)
	gotHash, _ := sendingpolicy.Hash(got)
	if gotHash != wantHash {
		t.Fatal("file changed the reviewed policy")
	}
	for name, input := range map[string][]byte{
		"unknown":       []byte(`{"private-secret-field":"secret-value"}`),
		"trailing":      append(append([]byte{}, raw...), ']'),
		"second object": append(append([]byte{}, raw...), raw...),
		"oversized":     []byte(strings.Repeat(" ", 65537)),
		"null":          []byte("null"),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := readSendingPolicyFile(writePolicyFixture(t, input))
			if err == nil {
				t.Fatal("invalid policy file accepted")
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("file content leaked: %v", err)
			}
		})
	}
	if _, err := readSendingPolicyFile(t.TempDir()); err == nil {
		t.Fatal("directory accepted")
	}
	if err := (&sendingProtectionFlags{policyFile: good}).validateStandalone(); err == nil {
		t.Fatal("orphan file flag accepted")
	}
	if err := (&sendingProtectionFlags{register: true, policyFile: good}).validateStandalone(); err == nil {
		t.Fatal("file ignored by unrelated command")
	}
}

func TestPolicyFileCarriesApprovalGateIntoDatabase(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	cfg := spTestConfig()
	recipients, err := sendingpolicy.LoadOperatorRecipients(spPolicyOperatorMap)
	if err != nil {
		t.Fatal(err)
	}
	secrets := sendingpolicy.Secrets{Recipients: recipients}
	module := sendingpolicy.NewModule(pool, secrets)
	if _, err := module.RegisterOperatorRecipients(ctx, "synthetic-operator", "test registration"); err != nil {
		t.Fatal(err)
	}
	// The server's config remains unchanged; only the reviewed file carries the gate.
	p, err := sendingpolicy.FromConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.ExternalSendingAccess = &sendingpolicy.ExternalSendingAccessPolicy{Mode: sendingpolicy.ModeEnforce, AccountsCreatedAtOrAfter: "2000-01-01T00:00:00Z", Unlocks: []sendingpolicy.ExternalUnlock{sendingpolicy.UnlockOperatorApproval}}
	raw, err := sendingpolicy.CanonicalBytes(p)
	if err != nil {
		t.Fatal(err)
	}
	path := writePolicyFixture(t, raw)
	hash, _ := sendingpolicy.Hash(p)
	var out bytes.Buffer
	if err := runSendingProtectionCommand(ctx, cfg, pool, secrets, &sendingProtectionFlags{inspect: true, policyFile: path}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "candidate_policy_sha256:") || !strings.Contains(out.String(), hash) {
		t.Fatalf("missing candidate review: %s", out.String())
	}
	before, _ := module.InspectPolicy(ctx)
	if before.Generation != 0 {
		t.Fatal("inspect wrote policy")
	}
	flags := &sendingProtectionFlags{activate: true, policyFile: path, expectedGeneration: 0, expectedPolicySHA: strings.Repeat("0", 64), reason: "carry admission gate"}
	if err := runSendingProtectionCommand(ctx, cfg, pool, secrets, flags, &out); err == nil {
		t.Fatal("wrong reviewed hash accepted")
	}
	still, _ := module.InspectPolicy(ctx)
	if still.Generation != 0 {
		t.Fatal("bad hash wrote policy")
	}
	flags.expectedPolicySHA = hash
	if err := runSendingProtectionCommand(ctx, cfg, pool, secrets, flags, &out); err != nil {
		t.Fatal(err)
	}
	stored, err := module.InspectPolicy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Generation != 1 || stored.Policy.ExternalSendingMode() != sendingpolicy.ModeEnforce || stored.Policy.BudgetMode != sendingpolicy.ModeDisabled || !stored.Policy.ExternalSendingAccess.Allows(sendingpolicy.UnlockOperatorApproval) || stored.Policy.ExternalSendingAccess.Allows(sendingpolicy.UnlockPaidEntitlement) {
		t.Fatalf("carryover changed controls: %+v", stored)
	}
	if err := runSendingProtectionCommand(ctx, cfg, pool, secrets, flags, &out); err == nil {
		t.Fatal("stale file replay accepted")
	}
}

func TestPolicyFileActivationRequiresTrustRootsForEnabledControls(t *testing.T) {
	ctx := context.Background()
	pool := testutil.TestDB(t)
	recipients, err := sendingpolicy.LoadOperatorRecipients(spPolicyOperatorMap)
	if err != nil {
		t.Fatal(err)
	}
	secrets := sendingpolicy.Secrets{Recipients: recipients}
	module := sendingpolicy.NewModule(pool, secrets)
	if _, err = module.RegisterOperatorRecipients(ctx, "synthetic-operator", "test"); err != nil {
		t.Fatal(err)
	}
	p := sendingpolicy.DisabledPolicy()
	p.BudgetMode = sendingpolicy.ModeShadow
	raw, _ := sendingpolicy.CanonicalBytes(p)
	hash, _ := sendingpolicy.Hash(p)
	var out bytes.Buffer
	err = runSendingProtectionCommand(ctx, spTestConfig(), pool, secrets, &sendingProtectionFlags{activate: true, policyFile: writePolicyFixture(t, raw), expectedGeneration: 0, expectedPolicySHA: hash, reason: "test"}, &out)
	if err == nil {
		t.Fatal("file enabled budgets without the signing keyring")
	}
	stored, err := module.InspectPolicy(ctx)
	if err != nil || stored.Generation != 0 {
		t.Fatalf("missing trust root mutated policy: %+v %v", stored, err)
	}
}
