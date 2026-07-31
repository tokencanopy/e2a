package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tokencanopy/e2a/internal/identity"
)

// protectionServer builds a server whose UpdateAgentProtection mutates a
// captured agent and returns it — the full PUT → store → view round-trip over
// the real chi+Huma stack. Returning the written row (rather than leaving the
// handler to re-read) mirrors the store, whose read-back runs inside the
// write's own transaction.
func protectionServer(t *testing.T) (*httptest.Server, *identity.AgentIdentity) {
	t.Helper()
	ag := sampleAgent()
	ag.InboundPolicy = "open"
	ag.InboundPolicyAction = "flag"
	ag.OutboundPolicy = "open"
	ag.OutboundPolicyAction = "flag"
	ag.InboundScanSensitivity = "off"
	ag.OutboundScanSensitivity = "off"
	deps := Deps{
		Authenticator: func(r *http.Request) (*identity.User, error) {
			if r.Header.Get("Authorization") == "Bearer good" {
				return &identity.User{ID: "u_1", Email: "owner@acme.com"}, nil
			}
			return nil, errors.New("unauthorized")
		},
		GetAgent: func(ctx context.Context, address string) (*identity.AgentIdentity, error) {
			if address == "support@acme.com" {
				a := ag
				return &a, nil
			}
			return nil, errors.New("not found")
		},
		UpdateAgentProtection: func(ctx context.Context, agentID, userID string, cfg identity.ProtectionConfig) (*identity.AgentIdentity, error) {
			ag.InboundPolicy = cfg.InboundGatePolicy
			ag.InboundAllowlist = cfg.InboundAllowlist
			ag.InboundPolicyAction = cfg.InboundGateAction
			ag.InboundScanSensitivity = cfg.InboundScanSensitivity
			ag.OutboundPolicy = cfg.OutboundGatePolicy
			ag.OutboundPolicyAction = cfg.OutboundGateAction
			ag.OutboundScanSensitivity = cfg.OutboundScanSensitivity
			ag.HITLTTLSeconds = cfg.HITLTTLSeconds
			ag.HITLExpirationAction = cfg.HITLExpirationAction
			ag.SuppressNotifications = cfg.SuppressNotifications
			a := ag
			return &a, nil
		},
		Legacy: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) }),
	}
	srv := httptest.NewServer(New(deps))
	t.Cleanup(srv.Close)
	return srv, &ag
}

// TestProtectionPutGetRoundTrip: PUT a posture, then confirm GET returns the
// nested shape with the written values.
func TestProtectionPutGetRoundTrip(t *testing.T) {
	t.Setenv("E2A_CONTENT_SCAN_ENABLED", "true") // round-trip the scan knob with the feature on
	srv, _ := protectionServer(t)
	put := map[string]any{
		"inbound": map[string]any{
			"gate": map[string]any{"policy": "allowlist", "allowlist": []string{"partner@acme.com"}, "action": "review"},
			"scan": map[string]any{"sensitivity": "high"},
		},
		"outbound": map[string]any{
			"gate": map[string]any{"policy": "domain", "allowlist": []string{"acme.com"}, "action": "block"},
			"scan": map[string]any{"sensitivity": "off"},
		},
		"holds": map[string]any{"ttl_seconds": 3600, "on_expiry": "approve", "suppress_notifications": true},
	}
	code, body := sendJSON(t, "PUT", srv.URL+"/v1/agents/support%40acme.com/protection", "good", put)
	if code != 200 {
		t.Fatalf("PUT status %d body %v", code, body)
	}

	code, got := sendJSON(t, "GET", srv.URL+"/v1/agents/support%40acme.com/protection", "good", nil)
	if code != 200 {
		t.Fatalf("GET status %d body %v", code, got)
	}
	inbound, _ := got["inbound"].(map[string]any)
	gate, _ := inbound["gate"].(map[string]any)
	scan, _ := inbound["scan"].(map[string]any)
	if gate["policy"] != "allowlist" || gate["action"] != "review" {
		t.Errorf("inbound gate not persisted: %v", gate)
	}
	if scan["sensitivity"] != "high" {
		t.Errorf("inbound sensitivity = %v, want high", scan["sensitivity"])
	}
	holds, _ := got["holds"].(map[string]any)
	if holds["on_expiry"] != "approve" {
		t.Errorf("holds.on_expiry = %v, want approve", holds["on_expiry"])
	}
	if holds["suppress_notifications"] != true {
		t.Errorf("holds.suppress_notifications = %v, want true", holds["suppress_notifications"])
	}
}

// With content scan gated off (the GA default), the protection handler clamps
// scan_sensitivity to "off" so a caller never persists a knob that silently
// never runs — get_protection reads back the honest, effective posture.
func TestProtectionPutClampsScanWhenDisabled(t *testing.T) {
	t.Setenv("E2A_CONTENT_SCAN_ENABLED", "false") // explicit: scan disabled
	srv, _ := protectionServer(t)
	put := map[string]any{
		"inbound": map[string]any{
			"gate": map[string]any{"policy": "open", "action": "flag"},
			"scan": map[string]any{"sensitivity": "high"},
		},
		"outbound": map[string]any{
			"gate": map[string]any{"policy": "open", "action": "flag"},
			"scan": map[string]any{"sensitivity": "high"},
		},
		"holds": map[string]any{"ttl_seconds": 3600, "on_expiry": "reject"},
	}
	if code, body := sendJSON(t, "PUT", srv.URL+"/v1/agents/support%40acme.com/protection", "good", put); code != 200 {
		t.Fatalf("PUT status %d body %v", code, body)
	}
	code, got := sendJSON(t, "GET", srv.URL+"/v1/agents/support%40acme.com/protection", "good", nil)
	if code != 200 {
		t.Fatalf("GET status %d body %v", code, got)
	}
	for _, dir := range []string{"inbound", "outbound"} {
		d, _ := got[dir].(map[string]any)
		scan, _ := d["scan"].(map[string]any)
		if scan["sensitivity"] != "off" {
			t.Errorf("%s scan not clamped: sensitivity = %v, want off", dir, scan["sensitivity"])
		}
	}
}

// TestProtectionPutRequiresTopLevelKeys: a body missing a top-level section is a
// 422 (D3 — no silent reset).
func TestProtectionPutRequiresTopLevelKeys(t *testing.T) {
	srv, _ := protectionServer(t)
	// Missing "holds".
	put := map[string]any{
		"inbound":  map[string]any{"gate": map[string]any{}, "scan": map[string]any{}},
		"outbound": map[string]any{"gate": map[string]any{}, "scan": map[string]any{}},
	}
	code, body := sendJSON(t, "PUT", srv.URL+"/v1/agents/support%40acme.com/protection", "good", put)
	if code != 422 {
		t.Fatalf("missing holds: status %d, want 422 (body %v)", code, body)
	}
}

// TestProtectionPutEmptyLeavesDefault: present top-level keys with empty leaves
// fill the safe-permissive defaults.
func TestProtectionPutEmptyLeavesDefault(t *testing.T) {
	srv, _ := protectionServer(t)
	put := map[string]any{
		"inbound":  map[string]any{"gate": map[string]any{}, "scan": map[string]any{}},
		"outbound": map[string]any{"gate": map[string]any{}, "scan": map[string]any{}},
		"holds":    map[string]any{},
	}
	code, body := sendJSON(t, "PUT", srv.URL+"/v1/agents/support%40acme.com/protection", "good", put)
	if code != 200 {
		t.Fatalf("empty leaves: status %d body %v", code, body)
	}
	inbound, _ := body["inbound"].(map[string]any)
	gate, _ := inbound["gate"].(map[string]any)
	if gate["policy"] != "open" || gate["action"] != "flag" {
		t.Errorf("defaults not applied: %v", gate)
	}
	holds, _ := body["holds"].(map[string]any)
	if holds["on_expiry"] != "reject" {
		t.Errorf("holds default on_expiry = %v, want reject", holds["on_expiry"])
	}
}

// TestAgentViewOmitsScanThresholds is the #13 read-layer regression guard: the
// agent-reachable GET /v1/agents/{email} must not carry any detection tuning.
func TestAgentViewOmitsScanThresholds(t *testing.T) {
	srv := testServer(t)
	code, body := sendJSON(t, "GET", srv.URL+"/v1/agents/support%40acme.com", "good", nil)
	if code != 200 {
		t.Fatalf("GET agent status %d body %v", code, body)
	}
	leaky := []string{
		"inbound_scan_review_threshold", "inbound_scan_block_threshold",
		"outbound_scan_review_threshold", "outbound_scan_block_threshold",
		"inbound_scan", "outbound_scan", "inbound_policy", "outbound_policy",
		"inbound_policy_action", "outbound_policy_action",
		"inbound_scan_sensitivity", "outbound_scan_sensitivity",
		"ttl_seconds", "on_expiry",
	}
	for _, k := range leaky {
		if _, present := body[k]; present {
			t.Errorf("AgentView leaks %q to an agent-reachable read (#13)", k)
		}
	}
}
