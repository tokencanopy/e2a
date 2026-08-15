package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/tokencanopy/e2a/internal/domainteardown"
	"github.com/tokencanopy/e2a/internal/idempotency"
	"github.com/tokencanopy/e2a/internal/identity"
)

func TestDeleteDomainLostResponseCanPollConfirmedReceipt(t *testing.T) {
	state := domainteardown.Pending
	srv := testServer(t, func(deps *Deps) {
		deps.DeleteDomain = func(context.Context, string, string, DomainDeleteIdemCompleter) (domainteardown.Receipt, error) {
			return domainteardown.Receipt{Incarnation: "lost-response-incarnation", State: state}, nil
		}
		deps.LookupDomainTeardownSnapshot = func(context.Context, string, string, string) (domainteardown.State, bool, error) {
			return state, false, nil
		}
	})

	code, body := sendJSON(t, "DELETE", srv.URL+"/v1/domains/lost-response.example.test?confirm=DELETE", "good", nil)
	if code != http.StatusOK || body["sending_teardown"] != SendingTeardownPending {
		t.Fatalf("pending receipt = %d %v", code, body)
	}
	state = domainteardown.Confirmed // durable worker later proves provider absence
	code, body = sendJSON(t, "DELETE", srv.URL+"/v1/domains/lost-response.example.test?confirm=DELETE", "good", nil)
	if code != http.StatusOK || body["sending_teardown"] != SendingTeardownConfirmed {
		t.Fatalf("confirmed receipt = %d %v", code, body)
	}
}

func TestDeleteDomainLostResponseRetryDoesNotDeleteReplacement(t *testing.T) {
	live := true
	deletions := 0
	receiptState := domainteardown.Pending
	const originalIncarnation = "e2a-verify=original-incarnation"
	// Simulate the exact crash window from the review: the business transaction
	// commits, but the generic post-response Complete never reaches Postgres.
	// CompleteTx still works, because it is part of the business transaction.
	idem := &atomicOnlyIdem{memIdem: newMemIdem()}
	srv := testServer(t, func(deps *Deps) {
		deps.Idempotency = idem
		deps.LookupDomain = func(context.Context, string, string) (*identity.Domain, error) {
			// Model an account transfer: the receipt owner cannot read the
			// replacement row, but the ownership-blind safety check still sees it.
			return nil, pgx.ErrNoRows
		}
		deps.LookupDomainTeardownSnapshot = func(_ context.Context, _, incarnation, _ string) (domainteardown.State, bool, error) {
			if incarnation != originalIncarnation {
				return "", false, pgx.ErrNoRows
			}
			return receiptState, live, nil
		}
		deps.DeleteDomain = func(ctx context.Context, _ string, _ string, complete DomainDeleteIdemCompleter) (domainteardown.Receipt, error) {
			deletions++
			live = false
			receipt := domainteardown.Receipt{Incarnation: originalIncarnation, State: receiptState}
			if complete != nil {
				if err := complete(ctx, nil, receipt); err != nil {
					return domainteardown.Receipt{}, err
				}
			}
			return receipt, nil
		}
	})

	deleteWithKey := func(domain, key string) (int, map[string]any) {
		req, err := http.NewRequest(http.MethodDelete, srv.URL+"/v1/domains/"+domain+"?confirm=DELETE", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer good")
		req.Header.Set("Idempotency-Key", key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, body
	}

	code, body := deleteWithKey("replacement.example.test", "delete-original-incarnation")
	if code != http.StatusOK || body["sending_teardown"] != SendingTeardownPending {
		t.Fatalf("first delete = %d %v", code, body)
	}
	code, body = deleteWithKey("different.example.test", "delete-original-incarnation")
	if code != http.StatusUnprocessableEntity || errCode(body) != "idempotency_key_reuse" {
		t.Fatalf("cross-domain key reuse = %d %v", code, body)
	}
	if deletions != 1 {
		t.Fatalf("cross-domain key reuse executed delete: deletions=%d", deletions)
	}

	// The first response is lost, then the same account registers a new domain
	// incarnation under the same name before the SDK retries the old request.
	// Expire only an in-progress claim, mirroring Store's five-minute stale
	// takeover. An atomically completed claim survives this step and replays.
	idem.expireInProgress("u_1", idemUserNS+"delete-original-incarnation")
	live = true
	receiptState = domainteardown.Confirmed
	code, body = deleteWithKey("replacement.example.test", "delete-original-incarnation")
	// The old receipt advanced, but a live replacement means its DNS is in use;
	// the public release signal must fail closed while the replacement exists.
	if code != http.StatusOK || body["sending_teardown"] != SendingTeardownPending {
		t.Fatalf("lost-response retry = %d %v", code, body)
	}
	if deletions != 1 {
		t.Fatalf("old keyed retry deleted the replacement: deletions=%d, want 1", deletions)
	}
	if !live {
		t.Fatal("replacement domain must remain registered after old keyed retry")
	}

	code, body = deleteWithKey("replacement.example.test", "delete-replacement-incarnation")
	if code != http.StatusOK || body["sending_teardown"] != SendingTeardownConfirmed {
		t.Fatalf("fresh-key replacement delete = %d %v", code, body)
	}
	if deletions != 2 || live {
		t.Fatalf("fresh key must delete the replacement: deletions=%d live=%v", deletions, live)
	}

	code, body = deleteWithKey("replacement.example.test", "delete-original-incarnation")
	if code != http.StatusOK || body["sending_teardown"] != SendingTeardownConfirmed {
		t.Fatalf("old keyed poll after replacement teardown = %d %v", code, body)
	}
	if deletions != 2 {
		t.Fatalf("old keyed poll re-executed deletion: deletions=%d", deletions)
	}
}

// atomicOnlyIdem drops generic post-response completion to model request
// cancellation/process death after the domain transaction commits. Its
// transaction-bound completion retains the real memIdem behavior.
type atomicOnlyIdem struct{ *memIdem }

func (m *atomicOnlyIdem) Complete(context.Context, string, string, idempotency.ClaimToken, idempotency.CachedResponse) error {
	return nil
}

func (m *atomicOnlyIdem) expireInProgress(userID, key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := userID + "\x00" + key
	if row := m.rows[k]; row != nil && !row.done {
		delete(m.rows, k)
	}
}
