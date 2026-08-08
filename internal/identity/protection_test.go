package identity_test

import (
	"context"
	"strings"
	"testing"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
)

func newProtectionAgent(t *testing.T, slug string) (*identity.Store, context.Context, string, string) {
	t.Helper()
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	domain := slug + ".example.com"
	user, err := store.CreateOrGetUser(ctx, "owner@"+domain, "Owner", "google-"+slug)
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	if _, err := store.ClaimOrCreateDomain(ctx, domain, user.ID); err != nil {
		t.Fatalf("ClaimOrCreateDomain: %v", err)
	}
	agent, err := store.CreateAgent(ctx, "agent@"+domain, domain, "", "", "", user.ID)
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	return store, ctx, agent.ID, user.ID
}

// TestUpdateAgentProtectionRoundTrip: a fresh agent defaults to sensitivity 'off';
// UpdateAgentProtection persists the gate, the sensitivity columns, AND the
// derived scan toggle + float thresholds the engine reads, all surfaced by
// GetAgentByID.
func TestUpdateAgentProtectionRoundTrip(t *testing.T) {
	store, ctx, agentID, userID := newProtectionAgent(t, "prot-rt")

	// Migration-045 default.
	fresh, err := store.GetAgentByID(ctx, agentID)
	if err != nil {
		t.Fatalf("GetAgentByID: %v", err)
	}
	if fresh.InboundScanSensitivity != identity.SensitivityOff || fresh.OutboundScanSensitivity != identity.SensitivityOff {
		t.Errorf("default sensitivity = %q/%q, want off/off", fresh.InboundScanSensitivity, fresh.OutboundScanSensitivity)
	}

	cfg := identity.ProtectionConfig{
		InboundGatePolicy:       "allowlist",
		InboundAllowlist:        []string{"partner@acme.com"},
		InboundGateAction:       "review",
		InboundScanSensitivity:  identity.SensitivityHigh,
		OutboundGatePolicy:      "domain",
		OutboundAllowlist:       []string{"acme.com"},
		OutboundGateAction:      "block",
		OutboundScanSensitivity: identity.SensitivityOff,
		HITLTTLSeconds:          3600,
		HITLExpirationAction:    "approve",
		SuppressNotifications:   true,
	}
	if _, err := store.UpdateAgentProtection(ctx, agentID, userID, cfg); err != nil {
		t.Fatalf("UpdateAgentProtection: %v", err)
	}

	got, err := store.GetAgentByID(ctx, agentID)
	if err != nil {
		t.Fatalf("GetAgentByID after update: %v", err)
	}
	// Gate persisted.
	if got.InboundPolicy != "allowlist" || got.InboundPolicyAction != "review" {
		t.Errorf("inbound gate not persisted: policy=%q action=%q", got.InboundPolicy, got.InboundPolicyAction)
	}
	if got.OutboundPolicy != "domain" || got.OutboundPolicyAction != "block" {
		t.Errorf("outbound gate not persisted: policy=%q action=%q", got.OutboundPolicy, got.OutboundPolicyAction)
	}
	if len(got.InboundAllowlist) != 1 || got.InboundAllowlist[0] != "partner@acme.com" {
		t.Errorf("inbound allowlist not persisted: %v", got.InboundAllowlist)
	}
	// Sensitivity (source of truth) persisted.
	if got.InboundScanSensitivity != identity.SensitivityHigh || got.OutboundScanSensitivity != identity.SensitivityOff {
		t.Errorf("sensitivity not persisted: in=%q out=%q", got.InboundScanSensitivity, got.OutboundScanSensitivity)
	}
	// Derived engine columns: high => scan on, 0.30/0.80; off => scan off.
	if got.InboundScan != "on" || !approxEq(got.InboundScanReviewThreshold, 0.30) || !approxEq(got.InboundScanBlockThreshold, 0.80) {
		t.Errorf("inbound high not derived: scan=%q review=%v block=%v", got.InboundScan, got.InboundScanReviewThreshold, got.InboundScanBlockThreshold)
	}
	if got.OutboundScan != "off" {
		t.Errorf("outbound off not derived: scan=%q", got.OutboundScan)
	}
	// Holds persisted.
	if got.HITLTTLSeconds != 3600 || got.HITLExpirationAction != "approve" {
		t.Errorf("holds not persisted: ttl=%d expiry=%q", got.HITLTTLSeconds, got.HITLExpirationAction)
	}
	if !got.SuppressNotifications {
		t.Error("suppress_notifications was not persisted")
	}
}

// TestUpdateAgentProtectionSensitivityMapping pins each level to its derived band.
func TestUpdateAgentProtectionSensitivityMapping(t *testing.T) {
	store, ctx, agentID, userID := newProtectionAgent(t, "prot-map")

	cases := []struct {
		level      string
		wantScan   string
		wantReview float64
		wantBlock  float64
	}{
		{identity.SensitivityOff, "off", 0.5, 0.9},
		{identity.SensitivityLow, "on", 0.70, 0.95},
		{identity.SensitivityMedium, "on", 0.50, 0.90},
		{identity.SensitivityHigh, "on", 0.30, 0.80},
	}
	for _, tc := range cases {
		cfg := identity.ProtectionConfig{
			InboundGatePolicy: "open", InboundGateAction: "flag", InboundScanSensitivity: tc.level,
			OutboundGatePolicy: "open", OutboundGateAction: "flag", OutboundScanSensitivity: tc.level,
			HITLTTLSeconds: 604800, HITLExpirationAction: "reject",
		}
		if _, err := store.UpdateAgentProtection(ctx, agentID, userID, cfg); err != nil {
			t.Fatalf("%s: UpdateAgentProtection: %v", tc.level, err)
		}
		got, err := store.GetAgentByID(ctx, agentID)
		if err != nil {
			t.Fatalf("%s: GetAgentByID: %v", tc.level, err)
		}
		if got.InboundScan != tc.wantScan || !approxEq(got.InboundScanReviewThreshold, tc.wantReview) || !approxEq(got.InboundScanBlockThreshold, tc.wantBlock) {
			t.Errorf("%s: derived scan=%q review=%v block=%v, want %q/%v/%v",
				tc.level, got.InboundScan, got.InboundScanReviewThreshold, got.InboundScanBlockThreshold, tc.wantScan, tc.wantReview, tc.wantBlock)
		}
	}
}

// TestUpdateAgentProtectionValidation rejects invalid postures with a clean error.
func TestUpdateAgentProtectionValidation(t *testing.T) {
	store, ctx, agentID, userID := newProtectionAgent(t, "prot-val")

	base := identity.ProtectionConfig{
		InboundGatePolicy: "open", InboundGateAction: "flag", InboundScanSensitivity: "off",
		OutboundGatePolicy: "open", OutboundGateAction: "flag", OutboundScanSensitivity: "off",
		HITLTTLSeconds: 604800, HITLExpirationAction: "reject",
	}
	mut := func(f func(*identity.ProtectionConfig)) identity.ProtectionConfig {
		c := base
		f(&c)
		return c
	}
	cases := map[string]identity.ProtectionConfig{
		"verified_only not a gate value": mut(func(c *identity.ProtectionConfig) { c.InboundGatePolicy = "verified_only" }),
		"bad inbound gate policy":        mut(func(c *identity.ProtectionConfig) { c.InboundGatePolicy = "nope" }),
		"bad outbound gate policy":       mut(func(c *identity.ProtectionConfig) { c.OutboundGatePolicy = "verified_only" }),
		"bad gate action":                mut(func(c *identity.ProtectionConfig) { c.OutboundGateAction = "drop" }),
		"bad sensitivity":                mut(func(c *identity.ProtectionConfig) { c.InboundScanSensitivity = "extreme" }),
		"negative ttl":                   mut(func(c *identity.ProtectionConfig) { c.HITLTTLSeconds = -1 }),
		"bad on_expiry":                  mut(func(c *identity.ProtectionConfig) { c.HITLExpirationAction = "defer" }),
		"allowlist over cap":             mut(func(c *identity.ProtectionConfig) { c.InboundAllowlist = makeAllowlist(1001) }),
	}
	for name, c := range cases {
		if _, err := store.UpdateAgentProtection(ctx, agentID, userID, c); err == nil {
			t.Errorf("%s: expected validation error, got nil", name)
		}
	}
}

// TestUpdateAgentProtectionWrongOwner: a config write keyed to a non-owner is a
// no-op that errors (tenant isolation at the store layer).
func TestUpdateAgentProtectionWrongOwner(t *testing.T) {
	store, ctx, agentID, _ := newProtectionAgent(t, "prot-owner")
	cfg := identity.ProtectionConfig{
		InboundGatePolicy: "open", InboundGateAction: "flag", InboundScanSensitivity: "off",
		OutboundGatePolicy: "open", OutboundGateAction: "flag", OutboundScanSensitivity: "off",
		HITLTTLSeconds: 604800, HITLExpirationAction: "reject",
	}
	if _, err := store.UpdateAgentProtection(ctx, agentID, "someone-else", cfg); err == nil {
		t.Fatal("expected error updating protection for non-owner, got nil")
	}
}

// TestProtectionSensitivityBackfill guards the migration-045 backfill: a pre-045
// agent with scan='on' whose new sensitivity column defaulted to 'off' must be
// re-derived from its review threshold, so a later read-modify-write PUT doesn't
// silently disable a live scan. (Adversarial/independent review convergent #1.)
func TestProtectionSensitivityBackfill(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	user, err := store.CreateOrGetUser(ctx, "owner@bf.example.com", "Owner", "google-bf")
	if err != nil {
		t.Fatalf("CreateOrGetUser: %v", err)
	}
	if _, err := store.ClaimOrCreateDomain(ctx, "bf.example.com", user.ID); err != nil {
		t.Fatalf("ClaimOrCreateDomain: %v", err)
	}
	agent, err := store.CreateAgent(ctx, "agent@bf.example.com", "bf.example.com", "", "", "", user.ID)
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	// Simulate the pre-045 hazard: scan on with real thresholds, sensitivity
	// stuck at the column default 'off' (inbound default pair → medium, outbound
	// aggressive pair → high).
	if _, err := pool.Exec(ctx, `UPDATE agent_identities SET
	        inbound_scan='on',  inbound_scan_review_threshold=0.5,  inbound_scan_block_threshold=0.9,  inbound_scan_sensitivity='off',
	        outbound_scan='on', outbound_scan_review_threshold=0.3, outbound_scan_block_threshold=0.8, outbound_scan_sensitivity='off'
	      WHERE id=$1`, agent.ID); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}

	// Mirror of the migrations/045 backfill statements.
	backfill := []string{
		`UPDATE agent_identities SET inbound_scan_sensitivity = CASE
		    WHEN inbound_scan_review_threshold <= 0.40 THEN 'high'
		    WHEN inbound_scan_review_threshold <= 0.60 THEN 'medium' ELSE 'low' END
		  WHERE inbound_scan = 'on' AND inbound_scan_sensitivity = 'off'`,
		`UPDATE agent_identities SET outbound_scan_sensitivity = CASE
		    WHEN outbound_scan_review_threshold <= 0.40 THEN 'high'
		    WHEN outbound_scan_review_threshold <= 0.60 THEN 'medium' ELSE 'low' END
		  WHERE outbound_scan = 'on' AND outbound_scan_sensitivity = 'off'`,
	}
	runBackfill := func() {
		for _, q := range backfill {
			if _, err := pool.Exec(ctx, q); err != nil {
				t.Fatalf("backfill: %v", err)
			}
		}
	}
	runBackfill()

	got, err := store.GetAgentByID(ctx, agent.ID)
	if err != nil {
		t.Fatalf("GetAgentByID: %v", err)
	}
	if got.InboundScanSensitivity != identity.SensitivityMedium {
		t.Errorf("inbound backfill = %q, want medium (review 0.5)", got.InboundScanSensitivity)
	}
	if got.OutboundScanSensitivity != identity.SensitivityHigh {
		t.Errorf("outbound backfill = %q, want high (review 0.3)", got.OutboundScanSensitivity)
	}

	// Idempotent: a second run touches nothing (no scan-on row still shows 'off').
	runBackfill()
	got2, _ := store.GetAgentByID(ctx, agent.ID)
	if got2.InboundScanSensitivity != identity.SensitivityMedium || got2.OutboundScanSensitivity != identity.SensitivityHigh {
		t.Errorf("backfill not idempotent: in=%q out=%q", got2.InboundScanSensitivity, got2.OutboundScanSensitivity)
	}
}

func makeAllowlist(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "a" + strings.Repeat("x", i%5) + "@acme.com"
	}
	return out
}
