package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/tokencanopy/e2a/internal/domainteardown"
	"github.com/tokencanopy/e2a/internal/identity"
)

func TestDeleteDomainLostResponseCanPollConfirmedReceipt(t *testing.T) {
	state := domainteardown.Pending
	srv := testServer(t, func(deps *Deps) {
		deps.LookupDomain = func(context.Context, string, string) (*identity.Domain, error) {
			return nil, pgx.ErrNoRows // first DELETE already committed; response was lost
		}
		deps.LookupDomainTeardown = func(context.Context, string, string) (domainteardown.State, error) {
			return state, nil
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
	srv := testServer(t, func(deps *Deps) {
		deps.Idempotency = newMemIdem()
		deps.LookupDomain = func(context.Context, string, string) (*identity.Domain, error) {
			if live {
				return &identity.Domain{Domain: "replacement.example.test", VerificationToken: "e2a-verify=replacement"}, nil
			}
			return nil, pgx.ErrNoRows
		}
		deps.DeleteDomain = func(context.Context, string, string) (domainteardown.State, error) {
			deletions++
			live = false
			return domainteardown.Confirmed, nil
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
	if code != http.StatusOK || body["sending_teardown"] != SendingTeardownConfirmed {
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
	live = true
	code, body = deleteWithKey("replacement.example.test", "delete-original-incarnation")
	if code != http.StatusOK || body["sending_teardown"] != SendingTeardownConfirmed {
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
}
