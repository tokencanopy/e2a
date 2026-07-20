package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/agent"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/outbound"
)

// batchTestDeps builds a Deps wired for batch-handler tests: a fixed user,
// one verified agent, and a DeliverBatch stub the caller supplies. The stub
// receives the already-validated + composed SendRequest slice, so tests can
// assert on what the handler produced and control the accept-tx outcome.
func batchTestDeps(deliver func(ctx context.Context, u *identity.User, ag *identity.AgentIdentity, items []outbound.SendRequest, ic agent.BatchAcceptIdemCompleter) (*agent.BatchAcceptResult, *agent.OutboundError)) Deps {
	return Deps{
		Authenticator: func(r *http.Request) (*identity.User, error) { return &identity.User{ID: "u_1"}, nil },
		GetAgent: func(ctx context.Context, address string) (*identity.AgentIdentity, error) {
			if address == "bot@acme.com" {
				return &identity.AgentIdentity{ID: "bot@acme.com", Email: "bot@acme.com", UserID: "u_1", DomainVerified: true}, nil
			}
			return nil, errors.New("not found")
		},
		DeliverBatch: deliver,
	}
}

// postBatch fires a POST /v1/agents/bot@acme.com/batches with the given body
// and returns the decoded status + response map.
func postBatch(t *testing.T, srv *httptest.Server, body any, idemKey string) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", srv.URL+"/v1/agents/bot@acme.com/batches", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer good")
	req.Header.Set("Content-Type", "application/json")
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestSendBatch_HappyPath(t *testing.T) {
	var gotItems []outbound.SendRequest
	srv := httptest.NewServer(New(batchTestDeps(
		func(ctx context.Context, u *identity.User, ag *identity.AgentIdentity, items []outbound.SendRequest, ic agent.BatchAcceptIdemCompleter) (*agent.BatchAcceptResult, *agent.OutboundError) {
			gotItems = items
			res := &agent.BatchAcceptResult{BatchID: "bat_test", Items: make([]agent.BatchAcceptItem, len(items))}
			for i := range items {
				res.Items[i] = agent.BatchAcceptItem{MessageID: "msg_" + string(rune('a'+i))}
			}
			return res, nil
		})))
	t.Cleanup(srv.Close)

	status, out := postBatch(t, srv, map[string]any{
		"messages": []map[string]any{
			{"to": []string{"alice@x.com"}, "subject": "Hi Alice", "text": "hello alice"},
			{"to": []string{"bob@x.com"}, "subject": "Hi Bob", "text": "hello bob"},
		},
	}, "")

	if status != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%v", status, out)
	}
	if out["batch_id"] != "bat_test" {
		t.Errorf("batch_id = %v, want bat_test", out["batch_id"])
	}
	if out["accepted"].(float64) != 2 {
		t.Errorf("accepted = %v, want 2", out["accepted"])
	}
	results, _ := out["results"].([]any)
	if len(results) != 2 {
		t.Fatalf("results len = %d, want 2", len(results))
	}
	// The handler must have mapped each BatchMessage to a SendRequest with
	// the path agent as From.
	if len(gotItems) != 2 || gotItems[0].From != "bot@acme.com" {
		t.Errorf("DeliverBatch got items = %+v", gotItems)
	}
	if gotItems[0].To[0] != "alice@x.com" || gotItems[1].To[0] != "bob@x.com" {
		t.Errorf("item recipients wrong: %+v", gotItems)
	}
}

func TestSendBatch_TooManyMessages(t *testing.T) {
	srv := httptest.NewServer(New(batchTestDeps(
		func(ctx context.Context, u *identity.User, ag *identity.AgentIdentity, items []outbound.SendRequest, ic agent.BatchAcceptIdemCompleter) (*agent.BatchAcceptResult, *agent.OutboundError) {
			t.Fatal("DeliverBatch should not run when validation rejects the batch")
			return nil, nil
		})))
	t.Cleanup(srv.Close)

	msgs := make([]map[string]any, 101)
	for i := range msgs {
		msgs[i] = map[string]any{"to": []string{"x@y.com"}, "subject": "s", "text": "t"}
	}
	status, out := postBatch(t, srv, map[string]any{"messages": msgs}, "")
	// 101 items exceeds the schema maxItems:100 — Huma rejects it at the
	// validation layer with 422 invalid_request (the framework's
	// array-bounds check fires before the handler's too_many_messages).
	if status != http.StatusBadRequest && status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 400/422; body=%v", status, out)
	}
}

func TestSendBatch_DuplicateRecipient(t *testing.T) {
	srv := httptest.NewServer(New(batchTestDeps(
		func(ctx context.Context, u *identity.User, ag *identity.AgentIdentity, items []outbound.SendRequest, ic agent.BatchAcceptIdemCompleter) (*agent.BatchAcceptResult, *agent.OutboundError) {
			t.Fatal("DeliverBatch should not run for a duplicate-recipient batch")
			return nil, nil
		})))
	t.Cleanup(srv.Close)

	status, out := postBatch(t, srv, map[string]any{
		"messages": []map[string]any{
			{"to": []string{"dup@x.com"}, "subject": "a", "text": "a"},
			{"to": []string{"other@x.com"}, "subject": "b", "text": "b"},
			{"to": []string{"dup@x.com"}, "subject": "c", "text": "c"},
		},
	}, "")
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%v", status, out)
	}
	errObj, _ := out["error"].(map[string]any)
	if errObj["code"] != "duplicate_recipient" {
		t.Errorf("code = %v, want duplicate_recipient", errObj["code"])
	}
	details, _ := errObj["details"].(map[string]any)
	if details["address"] != "dup@x.com" {
		t.Errorf("details.address = %v, want dup@x.com", details["address"])
	}
}

func TestSendBatch_BatchAttachmentSumExceeded(t *testing.T) {
	srv := httptest.NewServer(New(batchTestDeps(
		func(ctx context.Context, u *identity.User, ag *identity.AgentIdentity, items []outbound.SendRequest, ic agent.BatchAcceptIdemCompleter) (*agent.BatchAcceptResult, *agent.OutboundError) {
			t.Fatal("DeliverBatch should not run when the batch attachment cap is exceeded")
			return nil, nil
		})))
	t.Cleanup(srv.Close)

	// Two items, each with a ~15 MiB attachment → 30 MiB total > 25 MiB cap.
	// Each item is individually under the per-item 25 MiB cap.
	blob := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("A"), 15*1024*1024))
	att := []map[string]any{{"filename": "big.bin", "content_type": "application/octet-stream", "data": blob}}
	status, out := postBatch(t, srv, map[string]any{
		"messages": []map[string]any{
			{"to": []string{"a@x.com"}, "subject": "a", "text": "a", "attachments": att},
			{"to": []string{"b@x.com"}, "subject": "b", "text": "b", "attachments": att},
		},
	}, "")
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body=%v", status, out)
	}
	errObj, _ := out["error"].(map[string]any)
	if errObj["code"] != "payload_too_large" {
		t.Errorf("code = %v, want payload_too_large", errObj["code"])
	}
	details, _ := errObj["details"].(map[string]any)
	if details["scope"] != "batch" {
		t.Errorf("details.scope = %v, want batch", details["scope"])
	}
}

func TestSendBatch_SuppressionPartialDrop(t *testing.T) {
	srv := httptest.NewServer(New(batchTestDeps(
		func(ctx context.Context, u *identity.User, ag *identity.AgentIdentity, items []outbound.SendRequest, ic agent.BatchAcceptIdemCompleter) (*agent.BatchAcceptResult, *agent.OutboundError) {
			// Simulate item 1 dropped by suppression.
			return &agent.BatchAcceptResult{
				BatchID: "bat_supp",
				Items: []agent.BatchAcceptItem{
					{MessageID: "msg_a"},
					{Suppressed: &agent.SuppressedInfo{Address: "bounced@x.com", Reason: "bounce"}},
					{MessageID: "msg_c"},
				},
			}, nil
		})))
	t.Cleanup(srv.Close)

	status, out := postBatch(t, srv, map[string]any{
		"messages": []map[string]any{
			{"to": []string{"a@x.com"}, "subject": "a", "text": "a"},
			{"to": []string{"bounced@x.com"}, "subject": "b", "text": "b"},
			{"to": []string{"c@x.com"}, "subject": "c", "text": "c"},
		},
	}, "")
	if status != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%v", status, out)
	}
	if out["accepted"].(float64) != 2 {
		t.Errorf("accepted = %v, want 2", out["accepted"])
	}
	if out["suppressed_count"].(float64) != 1 {
		t.Errorf("suppressed_count = %v, want 1", out["suppressed_count"])
	}
	results, _ := out["results"].([]any)
	slot1, _ := results[1].(map[string]any)
	supp, _ := slot1["suppressed"].(map[string]any)
	if supp["address"] != "bounced@x.com" || supp["reason"] != "bounce" {
		t.Errorf("results[1].suppressed = %v", supp)
	}
	if _, hasMsg := slot1["message_id"]; hasMsg {
		t.Errorf("suppressed slot must not carry message_id: %v", slot1)
	}
}

func TestSendBatch_AllSuppressed(t *testing.T) {
	srv := httptest.NewServer(New(batchTestDeps(
		func(ctx context.Context, u *identity.User, ag *identity.AgentIdentity, items []outbound.SendRequest, ic agent.BatchAcceptIdemCompleter) (*agent.BatchAcceptResult, *agent.OutboundError) {
			return &agent.BatchAcceptResult{
				BatchID: "bat_allsupp",
				Items: []agent.BatchAcceptItem{
					{Suppressed: &agent.SuppressedInfo{Address: "a@x.com", Reason: "complaint"}},
				},
			}, nil
		})))
	t.Cleanup(srv.Close)

	status, out := postBatch(t, srv, map[string]any{
		"messages": []map[string]any{{"to": []string{"a@x.com"}, "subject": "a", "text": "a"}},
	}, "")
	// §14 Q9: all-suppressed is still a valid 202 with accepted:0.
	if status != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%v", status, out)
	}
	if out["accepted"].(float64) != 0 {
		t.Errorf("accepted = %v, want 0 for all-suppressed batch", out["accepted"])
	}
}

func TestSendBatch_HITLUnsupported(t *testing.T) {
	srv := httptest.NewServer(New(batchTestDeps(
		func(ctx context.Context, u *identity.User, ag *identity.AgentIdentity, items []outbound.SendRequest, ic agent.BatchAcceptIdemCompleter) (*agent.BatchAcceptResult, *agent.OutboundError) {
			return nil, &agent.OutboundError{Status: http.StatusForbidden, Code: "batch_hitl_unsupported", Msg: "batch send not available for HITL agents"}
		})))
	t.Cleanup(srv.Close)

	status, out := postBatch(t, srv, map[string]any{
		"messages": []map[string]any{{"to": []string{"a@x.com"}, "subject": "a", "text": "a"}},
	}, "")
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%v", status, out)
	}
	errObj, _ := out["error"].(map[string]any)
	if errObj["code"] != "batch_hitl_unsupported" {
		t.Errorf("code = %v, want batch_hitl_unsupported", errObj["code"])
	}
}

func TestSendBatch_BlockedByPolicy(t *testing.T) {
	srv := httptest.NewServer(New(batchTestDeps(
		func(ctx context.Context, u *identity.User, ag *identity.AgentIdentity, items []outbound.SendRequest, ic agent.BatchAcceptIdemCompleter) (*agent.BatchAcceptResult, *agent.OutboundError) {
			return nil, &agent.OutboundError{Status: http.StatusForbidden, Code: "blocked_by_policy", Msg: "batch item 0 blocked by outbound policy"}
		})))
	t.Cleanup(srv.Close)

	status, out := postBatch(t, srv, map[string]any{
		"messages": []map[string]any{{"to": []string{"a@x.com"}, "subject": "a", "text": "a"}},
	}, "")
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%v", status, out)
	}
	errObj, _ := out["error"].(map[string]any)
	if errObj["code"] != "blocked_by_policy" {
		t.Errorf("code = %v, want blocked_by_policy", errObj["code"])
	}
}

func TestSendBatch_RateLimited(t *testing.T) {
	srv := httptest.NewServer(New(batchTestDeps(
		func(ctx context.Context, u *identity.User, ag *identity.AgentIdentity, items []outbound.SendRequest, ic agent.BatchAcceptIdemCompleter) (*agent.BatchAcceptResult, *agent.OutboundError) {
			return nil, &agent.OutboundError{Status: http.StatusTooManyRequests, Code: "rate_limited", Msg: "over cap", Details: map[string]any{"retry_after_seconds": 30}, RetryAfter: 30}
		})))
	t.Cleanup(srv.Close)

	status, out := postBatch(t, srv, map[string]any{
		"messages": []map[string]any{{"to": []string{"a@x.com"}, "subject": "a", "text": "a"}},
	}, "")
	if status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429; body=%v", status, out)
	}
	errObj, _ := out["error"].(map[string]any)
	if errObj["code"] != "rate_limited" {
		t.Errorf("code = %v, want rate_limited", errObj["code"])
	}
}

func TestSendBatch_InvalidRecipientCarriesItemIndex(t *testing.T) {
	srv := httptest.NewServer(New(batchTestDeps(
		func(ctx context.Context, u *identity.User, ag *identity.AgentIdentity, items []outbound.SendRequest, ic agent.BatchAcceptIdemCompleter) (*agent.BatchAcceptResult, *agent.OutboundError) {
			t.Fatal("DeliverBatch should not run when an item has an invalid recipient")
			return nil, nil
		})))
	t.Cleanup(srv.Close)

	status, out := postBatch(t, srv, map[string]any{
		"messages": []map[string]any{
			{"to": []string{"ok@x.com"}, "subject": "a", "text": "a"},
			{"to": []string{"not-an-email"}, "subject": "b", "text": "b"},
		},
	}, "")
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%v", status, out)
	}
	errObj, _ := out["error"].(map[string]any)
	details, _ := errObj["details"].(map[string]any)
	// The per-item error must carry item_index=1 so the caller can find the
	// bad item (§8 item_index extension).
	if details == nil {
		t.Fatalf("expected details with item_index, got none; body=%v", out)
	}
	if idx, ok := details["item_index"].(float64); !ok || int(idx) != 1 {
		t.Errorf("details.item_index = %v, want 1", details["item_index"])
	}
}

func TestSendBatch_NotImplementedWhenDeliverBatchNil(t *testing.T) {
	deps := batchTestDeps(nil) // DeliverBatch nil
	srv := httptest.NewServer(New(deps))
	t.Cleanup(srv.Close)

	status, out := postBatch(t, srv, map[string]any{
		"messages": []map[string]any{{"to": []string{"a@x.com"}, "subject": "a", "text": "a"}},
	}, "")
	if status != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501; body=%v", status, out)
	}
	errObj, _ := out["error"].(map[string]any)
	if errObj["code"] != "not_implemented" {
		t.Errorf("code = %v, want not_implemented", errObj["code"])
	}
}

// TestSendBatch_ReplyToDefault verifies the batch-level reply_to default is
// applied to an item that omits its own, and a per-item value wins.
func TestSendBatch_ReplyToDefault(t *testing.T) {
	var gotItems []outbound.SendRequest
	srv := httptest.NewServer(New(batchTestDeps(
		func(ctx context.Context, u *identity.User, ag *identity.AgentIdentity, items []outbound.SendRequest, ic agent.BatchAcceptIdemCompleter) (*agent.BatchAcceptResult, *agent.OutboundError) {
			gotItems = items
			res := &agent.BatchAcceptResult{BatchID: "bat_rt", Items: make([]agent.BatchAcceptItem, len(items))}
			for i := range items {
				res.Items[i] = agent.BatchAcceptItem{MessageID: "msg_x"}
			}
			return res, nil
		})))
	t.Cleanup(srv.Close)

	status, _ := postBatch(t, srv, map[string]any{
		"reply_to": "support@acme.com",
		"messages": []map[string]any{
			{"to": []string{"a@x.com"}, "subject": "a", "text": "a"},                                     // inherits batch reply_to
			{"to": []string{"b@x.com"}, "subject": "b", "text": "b", "reply_to": "special@acme.com"},      // overrides
		},
	}, "")
	if status != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", status)
	}
	if gotItems[0].ReplyTo != "support@acme.com" {
		t.Errorf("item 0 ReplyTo = %q, want support@acme.com (batch default)", gotItems[0].ReplyTo)
	}
	if gotItems[1].ReplyTo != "special@acme.com" {
		t.Errorf("item 1 ReplyTo = %q, want special@acme.com (per-item wins)", gotItems[1].ReplyTo)
	}
}

// --- getBatch ---

func TestGetBatch_HappyPath(t *testing.T) {
	created := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	deps := Deps{
		Authenticator: func(r *http.Request) (*identity.User, error) { return &identity.User{ID: "u_1"}, nil },
		GetBatch: func(ctx context.Context, batchID string) (*identity.Batch, error) {
			if batchID != "bat_x" {
				return nil, nil
			}
			return &identity.Batch{
				BatchID: "bat_x", UserID: "u_1", AgentID: "bot@acme.com",
				Requested: 3, Accepted: 2, CreatedAt: created,
				SuppressedJSON: []byte(`[{"item_index":1,"address":"b@x.com","reason":"bounce"}]`),
			}, nil
		},
		BatchStatusRollup: func(ctx context.Context, batchID string) (*identity.BatchStatusRollup, error) {
			return &identity.BatchStatusRollup{Accepted: 1, Sent: 1}, nil
		},
	}
	srv := httptest.NewServer(New(deps))
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest("GET", srv.URL+"/v1/batches/bat_x", nil)
	req.Header.Set("Authorization", "Bearer good")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out["batch_id"] != "bat_x" {
		t.Errorf("batch_id = %v", out["batch_id"])
	}
	if out["requested"].(float64) != 3 || out["accepted"].(float64) != 2 {
		t.Errorf("counts wrong: %v", out)
	}
	supp, _ := out["suppressed"].([]any)
	if len(supp) != 1 {
		t.Fatalf("suppressed len = %d, want 1", len(supp))
	}
	rollup, _ := out["status_rollup"].(map[string]any)
	if rollup["sent"].(float64) != 1 || rollup["accepted"].(float64) != 1 {
		t.Errorf("rollup = %v", rollup)
	}
}

func TestGetBatch_NotFoundAndForeign(t *testing.T) {
	deps := Deps{
		Authenticator: func(r *http.Request) (*identity.User, error) { return &identity.User{ID: "u_1"}, nil },
		GetBatch: func(ctx context.Context, batchID string) (*identity.Batch, error) {
			switch batchID {
			case "bat_missing":
				return nil, nil
			case "bat_foreign":
				return &identity.Batch{BatchID: "bat_foreign", UserID: "u_OTHER"}, nil
			}
			return nil, nil
		},
		BatchStatusRollup: func(ctx context.Context, batchID string) (*identity.BatchStatusRollup, error) {
			return &identity.BatchStatusRollup{}, nil
		},
	}
	srv := httptest.NewServer(New(deps))
	t.Cleanup(srv.Close)

	for _, id := range []string{"bat_missing", "bat_foreign"} {
		req, _ := http.NewRequest("GET", srv.URL+"/v1/batches/"+id, nil)
		req.Header.Set("Authorization", "Bearer good")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404 (foreign must not leak existence)", id, resp.StatusCode)
		}
		errObj, _ := out["error"].(map[string]any)
		if errObj["code"] != "not_found" {
			t.Errorf("%s: code = %v, want not_found", id, errObj["code"])
		}
	}
}

// sanity: exercise stripBase64Whitespace directly.
func TestStripBase64Whitespace(t *testing.T) {
	in := "aGVs\r\nbG8g\t d29ybGQ="
	got := stripBase64Whitespace(in)
	if strings.ContainsAny(got, " \r\n\t") {
		t.Errorf("whitespace not stripped: %q", got)
	}
}
