package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tokencanopy/e2a/internal/domainteardown"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/sendramp"
)

func TestGetDomainIncludesReadOnlySendingRamp(t *testing.T) {
	srv := testServer(t, func(deps *Deps) {
		deps.SendingRampSnapshot = func(context.Context, string, string, time.Time) (sendramp.Snapshot, error) {
			return sendramp.Snapshot{Status: sendramp.StatusRamping, ActiveDays: 2, RampDays: 30, DailyLimit: 115, UsedToday: 93}, nil
		}
	})
	code, body := getJSON(t, srv.URL+"/v1/domains/acme.com", "good")
	if code != http.StatusOK {
		t.Fatalf("status %d body %v", code, body)
	}
	ramp, ok := body["sending_ramp"].(map[string]any)
	if !ok || ramp["status"] != "ramping" || ramp["daily_recipient_limit"] != float64(115) || ramp["recipients_used_today"] != float64(93) {
		t.Fatalf("sending_ramp = %#v, want ramping 93/115", body["sending_ramp"])
	}
}

func TestListDomains(t *testing.T) {
	srv := testServer(t)
	code, body := getJSON(t, srv.URL+"/v1/domains", "good")
	if code != 200 {
		t.Fatalf("status %d body %v", code, body)
	}
	doms, _ := body["items"].([]any)
	if len(doms) != 1 {
		t.Fatalf("want 1 domain, got %d", len(doms))
	}
	d := doms[0].(map[string]any)
	// dns_records is now a unified, purpose-tagged array (the legacy
	// mx/txt/dkim object + sending_dns_records array were collapsed).
	recs, _ := d["dns_records"].([]any)
	byPurpose := map[string]map[string]any{}
	for _, r := range recs {
		rec := r.(map[string]any)
		byPurpose[rec["purpose"].(string)] = rec
	}
	mx, ok := byPurpose["inbound_mx"]
	if !ok || mx["value"] != "mx.e2a.dev" || mx["type"] != "MX" {
		t.Fatalf("unexpected inbound_mx record: %v", recs)
	}
	// Verified domain ⇒ inbound records verified; no sending feature in the
	// base test server ⇒ no mail_from records.
	if mx["status"] != "verified" {
		t.Fatalf("want verified inbound_mx on a verified domain, got %v", mx["status"])
	}
	if _, present := byPurpose["mail_from_mx"]; present {
		t.Fatalf("sending feature is off in the base test server; mail_from records must be absent: %v", recs)
	}
	// Domains are single-page at GA (no server-side cursoring yet): the Page
	// envelope is present but next_cursor is always null. Locks the contract so
	// wiring real cursoring later forces an update here (and in the SDK pagers).
	if body["next_cursor"] != nil {
		t.Fatalf("expected null next_cursor on single page, got %v", body["next_cursor"])
	}
}

func TestGetDomain(t *testing.T) {
	srv := testServer(t)
	code, body := getJSON(t, srv.URL+"/v1/domains/acme.com", "good")
	if code != 200 || body["domain"] != "acme.com" || body["verified"] != true {
		t.Fatalf("unexpected get domain: %d %v", code, body)
	}
}

func TestGetDomainNotFound(t *testing.T) {
	srv := testServer(t)
	code, _ := getJSON(t, srv.URL+"/v1/domains/unknown.com", "good")
	if code != 404 {
		t.Fatalf("want 404, got %d", code)
	}
}

func TestRegisterDomain(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+"/v1/domains", "good", map[string]any{"domain": "new.com"})
	if code != 201 || body["domain"] != "new.com" {
		t.Fatalf("want 201 new.com, got %d %v", code, body)
	}
}

func TestRegisterDomainReserved(t *testing.T) {
	srv := testServer(t)
	// SharedDomain is "agents.e2a.dev" in the test server.
	code, body := postJSON(t, srv.URL+"/v1/domains", "good", map[string]any{"domain": "agents.e2a.dev"})
	if code != 400 || errCode(body) != "reserved_domain" {
		t.Fatalf("want 400 reserved_domain, got %d %v", code, body)
	}
}

func TestRegisterDomainReservedMailFromSubtree(t *testing.T) {
	srv := testServer(t, func(d *Deps) {
		d.ClaimDomain = func(ctx context.Context, domain, userID string) (*identity.Domain, error) {
			return nil, identity.ErrReservedDomain
		}
	})
	code, body := postJSON(t, srv.URL+"/v1/domains", "good", map[string]any{"domain": "bounce.acme.com"})
	if code != 400 || errCode(body) != "reserved_domain" {
		t.Fatalf("want 400 reserved_domain, got %d %v", code, body)
	}
}

func TestRegisterDomainInvalid(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+"/v1/domains", "good", map[string]any{"domain": "nodot"})
	if code != 400 || errCode(body) != "invalid_domain" {
		t.Fatalf("want 400 invalid_domain, got %d %v", code, body)
	}
}

func TestRegisterDomainOverCap(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+"/v1/domains", "overcap", map[string]any{"domain": "new.com"})
	if code != 402 || errCode(body) != "limit_exceeded" {
		t.Fatalf("want 402 limit_exceeded, got %d %v", code, body)
	}
}

func TestDeleteDomain(t *testing.T) {
	srv := testServer(t)
	req, _ := http.NewRequest("DELETE", srv.URL+"/v1/domains/acme.com?confirm=DELETE", nil)
	req.Header.Set("Authorization", "Bearer good")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["deleted"] != true || body["domain"] != "acme.com" || body["sending_teardown"] != SendingTeardownConfirmed {
		t.Fatalf("want {deleted:true, domain:acme.com, sending_teardown:confirmed}, got %v", body)
	}
}

func TestDeleteDomainRequiresConfirm(t *testing.T) {
	srv := testServer(t)
	// confirm is now a required enum:[DELETE] query param — Huma rejects a
	// missing/wrong value with 422 before the handler runs.
	code, body := sendJSON(t, "DELETE", srv.URL+"/v1/domains/acme.com", "good", nil)
	if code != 422 || errCode(body) != "invalid_request" {
		t.Fatalf("want 422 invalid_request, got %d %v", code, body)
	}
}

func TestDeleteDomainWithAgents(t *testing.T) {
	srv := testServer(t)
	code, body := sendJSON(t, "DELETE", srv.URL+"/v1/domains/busy.com?confirm=DELETE", "good", nil)
	if code != 400 || errCode(body) != "domain_has_agents" {
		t.Fatalf("want 400 domain_has_agents, got %d %v", code, body)
	}
}

func TestDeleteDomainNotFound(t *testing.T) {
	srv := testServer(t)
	code, _ := sendJSON(t, "DELETE", srv.URL+"/v1/domains/unknown.com?confirm=DELETE", "good", nil)
	if code != 404 {
		t.Fatalf("want 404, got %d", code)
	}
}

func TestDeleteDomainReturnsOwnerScopedReceiptAfterRowIsGone(t *testing.T) {
	srv := testServer(t, func(deps *Deps) {
		deps.LookupDomain = func(context.Context, string, string) (*identity.Domain, error) {
			return nil, pgx.ErrNoRows
		}
		deps.LookupDomainTeardown = func(_ context.Context, domain, userID string) (domainteardown.State, error) {
			if domain != "already-deleted.example.test" || userID != "u_1" {
				t.Fatalf("receipt lookup = %q, %q", domain, userID)
			}
			return domainteardown.Pending, nil
		}
		deps.DeleteDomain = func(context.Context, string, string) (domainteardown.State, error) {
			t.Fatal("durable receipt retry must not execute deletion again")
			return "", nil
		}
	})

	code, body := sendJSON(t, "DELETE", srv.URL+"/v1/domains/already-deleted.example.test?confirm=DELETE", "good", nil)
	if code != http.StatusOK || body["deleted"] != true || body["sending_teardown"] != SendingTeardownPending {
		t.Fatalf("receipt response = %d %v, want 200 pending deletion receipt", code, body)
	}
}

func TestVerifyDomainAlreadyVerified(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+"/v1/domains/acme.com/verify", "good", nil)
	if code != 200 || body["verified"] != true {
		t.Fatalf("want 200 verified, got %d %v", code, body)
	}
}

func TestVerifyDomainTXTMissing(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+"/v1/domains/pending.com/verify", "good", nil)
	// Not-yet-published is a normal 200 with verified:false — NOT a 412. Clients
	// branch on the body's `verified` boolean, not the HTTP status.
	if code != 200 || body["verified"] != false {
		t.Fatalf("want 200 not-verified, got %d %v", code, body)
	}
	if body["mx"] != "missing" {
		t.Fatalf("expected diagnostic in 200 body, got %v", body)
	}
}

// TestVerifyDomainMXMissing — verification now requires the inbound MX too, not
// just the ownership TXT, so that inbound_mx.status="verified" (derived from the
// domain's verified flag) is honest. TXT present but MX missing ⇒ verified:false
// (returned as a normal 200, not a 412).
func TestVerifyDomainMXMissing(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+"/v1/domains/nomx.com/verify", "good", nil)
	if code != 200 || body["verified"] != false {
		t.Fatalf("want 200 not-verified (MX missing), got %d %v", code, body)
	}
	if body["mx"] != "missing" {
		t.Fatalf("expected mx=missing diagnostic in 200 body, got %v", body)
	}
}

func TestVerifyDomainSuccess(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+"/v1/domains/fresh.com/verify", "good", nil)
	if code != 200 || body["verified"] != true {
		t.Fatalf("want 200 verified, got %d %v", code, body)
	}
}

func TestVerifyDomainNotFound(t *testing.T) {
	srv := testServer(t)
	code, _ := postJSON(t, srv.URL+"/v1/domains/unknown.com/verify", "good", nil)
	if code != 404 {
		t.Fatalf("want 404, got %d", code)
	}
}
