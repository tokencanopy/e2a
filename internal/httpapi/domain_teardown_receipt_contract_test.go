package httpapi

import (
	"context"
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
