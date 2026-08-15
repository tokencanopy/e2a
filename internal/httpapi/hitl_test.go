package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tokencanopy/e2a/internal/agent"
	"github.com/tokencanopy/e2a/internal/idempotency"
	"github.com/tokencanopy/e2a/internal/identity"
)

func TestApproveResultLoopbackOmitsProviderMessageID(t *testing.T) {
	_, view := approveResult(&identity.Message{
		ID: "msg_local", Method: "loopback", ProviderMessageID: "<local@loopback.e2a.test>",
		DeliveryStatus: "sent", SentAs: "own_address",
	})
	if view.MessageID != "msg_local" {
		t.Fatalf("message_id=%q want msg_local", view.MessageID)
	}
	if view.ProviderMessageID != "" {
		t.Fatalf("provider_message_id=%q want omitted for providerless loopback", view.ProviderMessageID)
	}
}

// These exercise the account-scoped review queue approve/reject dispatch
// (/v1/reviews/{id}/approve|reject) — the only approve/reject path since the
// deprecated agent-path (/v1/agents/{email}/messages/{id}/approve|reject) was
// removed in the pre-GA vocabulary freeze.

func TestApproveSent(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+"/v1/reviews/msg_pending/approve", "good", map[string]any{})
	if code != 200 || body["status"] != "sent" || body["message_id"] != "msg_pending" {
		t.Fatalf("want 200 sent, got %d %v", code, body)
	}
	if body["provider_message_id"] != "<prov@ses>" {
		t.Fatalf("expected provider_message_id, got %v", body)
	}
}

// txOnlyIdem records only transaction-level completion, simulating the crash
// window where post-transaction runIdempotent completion never lands.
type txOnlyIdem struct{ *memIdem }

func (m *txOnlyIdem) Complete(context.Context, string, string, idempotency.ClaimToken, idempotency.CachedResponse) error {
	return nil // simulate a crash window where post-transaction completion never lands
}

func (m *txOnlyIdem) CompleteTx(ctx context.Context, _ pgx.Tx, userID, key string, token idempotency.ClaimToken, resp idempotency.CachedResponse) error {
	return m.memIdem.Complete(ctx, userID, key, token, resp)
}

// TestApproveAccepted202AndIdempotentReplay pins the async-accept convention
// for review approval: enqueued delivery returns 202 Accepted, and a
// byte-identical keyed retry replays the transaction-cached 202 without
// approving twice or consuming another rate-limit token. Synchronous sent and
// inbound released outcomes remain terminal 200s.
func TestApproveAccepted202AndIdempotentReplay(t *testing.T) {
	approvals := 0
	rateChecks := 0
	srv := testServer(t, func(d *Deps) {
		d.Idempotency = &txOnlyIdem{memIdem: newMemIdem()}
		d.SendLimit = func(key string) (bool, time.Duration) {
			rateChecks++
			return rateChecks == 1, 7 * time.Second
		}
		d.ApprovePending = func(ctx context.Context, userID, messageID, expectedAgentEmail string, ovr agent.ApproveOverrides, complete agent.ApproveIdemCompleter) (*identity.Message, *agent.OutboundError) {
			approvals++
			sent := &identity.Message{ID: messageID, DeliveryStatus: "accepted", Method: "smtp"}
			if complete == nil {
				t.Fatal("async approval must receive an in-transaction idempotency completer")
			}
			if err := complete(ctx, nil, sent); err != nil {
				t.Fatalf("complete approval idempotency in tx: %v", err)
			}
			return sent, nil
		}
	})

	rawBody := []byte(`{}`)
	approve := func() (int, map[string]any) {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/reviews/msg_pending/approve", bytes.NewReader(rawBody))
		req.Header.Set("Authorization", "Bearer good")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "approve-accepted-1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return resp.StatusCode, body
	}

	for attempt := 1; attempt <= 2; attempt++ {
		code, body := approve()
		if code != http.StatusAccepted || body["status"] != "accepted" || body["message_id"] != "msg_pending" {
			t.Fatalf("attempt %d: want 202 accepted msg_pending, got %d %v", attempt, code, body)
		}
	}
	if approvals != 1 {
		t.Fatalf("ApprovePending ran %d times, want exactly 1 (retry must replay, not re-enqueue)", approvals)
	}
	if rateChecks != 1 {
		t.Fatalf("send rate limit checked %d times, want exactly 1 (cached retry must not consume or require another token)", rateChecks)
	}
}

func TestApproveWithOverrides(t *testing.T) {
	srv := testServer(t)
	// reviewer overrides ride in the body
	code, _ := postJSON(t, srv.URL+"/v1/reviews/msg_pending/approve", "good",
		map[string]any{"subject": "edited", "to": []string{"a@x.com"}})
	if code != 200 {
		t.Fatalf("want 200, got %d", code)
	}
}

func TestApproveNotPending(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+"/v1/reviews/msg_notpending/approve", "good", map[string]any{})
	if code != 409 || errCode(body) != "message_not_pending" {
		t.Fatalf("want 409 message_not_pending, got %d %v", code, body)
	}
}

func TestApproveNotFound(t *testing.T) {
	srv := testServer(t)
	code, _ := postJSON(t, srv.URL+"/v1/reviews/msg_missing/approve", "good", map[string]any{})
	if code != 404 {
		t.Fatalf("want 404, got %d", code)
	}
}

func TestReject(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+"/v1/reviews/msg_pending/reject", "good", map[string]any{"reason": "not now"})
	if code != 200 || body["status"] != "review_rejected" || body["rejection_reason"] != "not now" {
		t.Fatalf("want 200 rejected, got %d %v", code, body)
	}
}

func TestRejectNotFound(t *testing.T) {
	srv := testServer(t)
	code, _ := postJSON(t, srv.URL+"/v1/reviews/msg_missing/reject", "good", map[string]any{})
	if code != 404 {
		t.Fatalf("want 404, got %d", code)
	}
}

// --- inbound review release (slice 3) ---

// TestApproveInboundReleases asserts that approving an INBOUND hold releases it
// (status review_approved, no SES send fields) rather than running the outbound
// send path.
func TestApproveInboundReleases(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+"/v1/reviews/msg_in_held/approve", "good", map[string]any{})
	if code != 200 || body["status"] != "review_approved" || body["message_id"] != "msg_in_held" {
		t.Fatalf("want 200 review_approved, got %d %v", code, body)
	}
	// A release is not a send — no provider_message_id / method / edited.
	if _, ok := body["provider_message_id"]; ok {
		t.Fatalf("inbound release must not carry provider_message_id: %v", body)
	}
	if _, ok := body["edited"]; ok {
		t.Fatalf("inbound release must not carry edited: %v", body)
	}
}

// TestRejectInboundDrops asserts that rejecting an INBOUND hold drops it
// (status review_rejected) with the reviewer reason echoed.
func TestRejectInboundDrops(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+"/v1/reviews/msg_in_held/reject", "good", map[string]any{"reason": "prompt injection"})
	if code != 200 || body["status"] != "review_rejected" || body["rejection_reason"] != "prompt injection" {
		t.Fatalf("want 200 review_rejected, got %d %v", code, body)
	}
}

// TestApproveInboundNotPending asserts the compare-and-set conflict (the hold was
// already resolved by a human or the TTL sweep) surfaces as 409, not a double
// release.
func TestApproveInboundNotPending(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+"/v1/reviews/msg_in_notpending/approve", "good", map[string]any{})
	if code != 409 || errCode(body) != "message_not_pending" {
		t.Fatalf("want 409 message_not_pending, got %d %v", code, body)
	}
}

// TestRejectInboundNotPending mirrors the approve conflict on the reject path.
func TestRejectInboundNotPending(t *testing.T) {
	srv := testServer(t)
	code, body := postJSON(t, srv.URL+"/v1/reviews/msg_in_notpending/reject", "good", map[string]any{"reason": "x"})
	if code != 409 || errCode(body) != "message_not_pending" {
		t.Fatalf("want 409 message_not_pending, got %d %v", code, body)
	}
}
