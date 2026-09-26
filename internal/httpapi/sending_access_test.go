package httpapi

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/sendingpolicy"
)

func TestAccountSendingAccessObject(t *testing.T) {
	t.Run("absent when not wired", func(t *testing.T) {
		srv := testServer(t)
		code, body := getJSON(t, srv.URL+"/v1/account", "good")
		if code != 200 {
			t.Fatalf("status %d", code)
		}
		if _, ok := body["sending_access"]; ok {
			t.Fatalf("sending_access must be omitted when unwired: %v", body)
		}
	})
	t.Run("booleans from the status role, bound to the principal", func(t *testing.T) {
		var seen string
		srv := testServer(t, func(d *Deps) {
			d.SendingAccessStatus = func(_ context.Context, userID string) (sendingpolicy.ExternalAccessStatus, error) {
				seen = userID
				return sendingpolicy.ExternalAccessStatus{EnforcementApplies: true, OwnerRecipientVerified: true}, nil
			}
		})
		code, body := getJSON(t, srv.URL+"/v1/account", "good")
		if code != 200 {
			t.Fatalf("status %d", code)
		}
		sa, _ := body["sending_access"].(map[string]any)
		want := map[string]bool{"enforcement_applies": true, "shared_external_approved": false, "paid_external_sending_entitled": false, "owner_recipient_verified": true}
		if len(sa) != len(want) {
			t.Fatalf("sending_access = %v, want exactly %v", sa, want)
		}
		for k, v := range want {
			if sa[k] != v {
				t.Errorf("%s = %v, want %v", k, sa[k], v)
			}
		}
		if seen != "u_1" {
			t.Errorf("status read for %q, want the principal", seen)
		}
	})
	t.Run("a read failure omits the object instead of failing whoami", func(t *testing.T) {
		srv := testServer(t, func(d *Deps) {
			d.SendingAccessStatus = func(context.Context, string) (sendingpolicy.ExternalAccessStatus, error) {
				return sendingpolicy.ExternalAccessStatus{}, errors.New("db down")
			}
		})
		code, body := getJSON(t, srv.URL+"/v1/account", "good")
		if code != 200 {
			t.Fatalf("status %d", code)
		}
		if _, ok := body["sending_access"]; ok {
			t.Fatal("an unreadable state must not be rendered as false booleans")
		}
	})
}

type fakeAccessRequests struct {
	pending  *sendingpolicy.AccessRequest
	userIDs  []string
	notified int
	err      error
}

func (f *fakeAccessRequests) wire(d *Deps) {
	d.SubmitSendingAccessRequest = func(_ context.Context, userID string, in sendingpolicy.AccessRequestInput) (sendingpolicy.AccessRequest, bool, error) {
		f.userIDs = append(f.userIDs, userID)
		if f.err != nil {
			return sendingpolicy.AccessRequest{}, false, f.err
		}
		if f.pending != nil {
			return *f.pending, false, nil
		}
		r := sendingpolicy.AccessRequest{ID: "esar_test", State: "pending", UseCase: in.UseCase, Recipients: in.Recipients, ExpectedDailyVolume: in.ExpectedDailyVolume, CreatedAt: time.Now().UTC()}
		f.pending = &r
		return r, true, nil
	}
	d.LatestSendingAccessRequest = func(_ context.Context, userID string) (*sendingpolicy.AccessRequest, error) {
		f.userIDs = append(f.userIDs, userID)
		return f.pending, nil
	}
	d.NotifySendingAccessRequest = func(context.Context, string, sendingpolicy.AccessRequest) { f.notified++ }
}

func TestSendingAccessRequestEndpoints(t *testing.T) {
	fake := &fakeAccessRequests{}
	srv := testServer(t, fake.wire)
	url := srv.URL + "/v1/account/sending-access/request"

	if code, body := getJSON(t, url, "good"); code != 404 || errCode(body) != "not_found" {
		t.Fatalf("no request yet: %d %v", code, body)
	}
	form := map[string]any{"use_case": "support replies", "recipients": "our customers", "expected_daily_volume": 25}
	code, body := sendJSON(t, http.MethodPost, url, "good", form)
	if code != 201 || body["state"] != "pending" || body["id"] != "esar_test" {
		t.Fatalf("create: %d %v", code, body)
	}
	code, body = sendJSON(t, http.MethodPost, url, "good", form)
	if code != 200 || body["id"] != "esar_test" {
		t.Fatalf("resubmit must return the pending request with 200: %d %v", code, body)
	}
	if fake.notified != 1 {
		t.Fatalf("operator notified %d times, want once for the created request", fake.notified)
	}
	if code, body := getJSON(t, url, "good"); code != 200 || body["use_case"] != "support replies" {
		t.Fatalf("latest: %d %v", code, body)
	}
	for _, id := range fake.userIDs {
		if id != "u_1" {
			t.Fatalf("request bound to %q, want the authenticated account", id)
		}
	}

	// A submitted account id is not part of the contract and is rejected.
	withAccount := map[string]any{"use_case": "x", "recipients": "y", "expected_daily_volume": 1, "user_id": "u_other"}
	if code, _ := sendJSON(t, http.MethodPost, url, "good", withAccount); code != 422 && code != 400 {
		t.Fatalf("unknown account field must be refused, got %d", code)
	}
	for name, bad := range map[string]map[string]any{
		"empty use case": {"use_case": "", "recipients": "y", "expected_daily_volume": 1},
		"zero volume":    {"use_case": "x", "recipients": "y", "expected_daily_volume": 0},
		"huge volume":    {"use_case": "x", "recipients": "y", "expected_daily_volume": 1000001},
	} {
		if code, _ := sendJSON(t, http.MethodPost, url, "good", bad); code != 422 && code != 400 {
			t.Errorf("%s: status %d, want validation failure", name, code)
		}
	}
	if code, _ := sendJSON(t, http.MethodPost, url, "", form); code != 401 {
		t.Fatalf("unauthenticated: %d", code)
	}
}

func TestSendingAccessRequestRateLimitAndUnwired(t *testing.T) {
	fake := &fakeAccessRequests{err: sendingpolicy.ErrAccessRequestRateLimited}
	srv := testServer(t, fake.wire)
	form := map[string]any{"use_case": "x", "recipients": "y", "expected_daily_volume": 1}
	code, body := sendJSON(t, http.MethodPost, srv.URL+"/v1/account/sending-access/request", "good", form)
	if code != 429 || errCode(body) != "rate_limited" {
		t.Fatalf("rate limit: %d %v", code, body)
	}
	unwired := testServer(t)
	if code, _ := sendJSON(t, http.MethodPost, unwired.URL+"/v1/account/sending-access/request", "good", form); code != 501 {
		t.Fatalf("unwired: %d", code)
	}
}

func TestSendingAccessRequestRejectsAgentScopedCredential(t *testing.T) {
	fake := &fakeAccessRequests{}
	srv := testServer(t, fake.wire, func(d *Deps) {
		d.PrincipalAuthenticator = func(r *http.Request) (*identity.Principal, error) {
			return &identity.Principal{User: &identity.User{ID: "u_1"}, Scope: identity.ScopeAgent, AgentID: "agent@example.com"}, nil
		}
	})
	form := map[string]any{"use_case": "x", "recipients": "y", "expected_daily_volume": 1}
	if code, _ := sendJSON(t, http.MethodPost, srv.URL+"/v1/account/sending-access/request", "good", form); code != 403 {
		t.Fatalf("agent-scoped create: %d, want 403", code)
	}
	if len(fake.userIDs) != 0 {
		t.Fatal("an agent-scoped caller must not reach the store")
	}
}

func TestSendingAccessDisabledSurfaces(t *testing.T) {
	srv := testServer(t, func(d *Deps) {
		d.SendingAccessStatus = func(context.Context, string) (sendingpolicy.ExternalAccessStatus, error) {
			return sendingpolicy.ExternalAccessStatus{}, sendingpolicy.ErrExternalAccessDisabled
		}
		d.SubmitSendingAccessRequest = func(context.Context, string, sendingpolicy.AccessRequestInput) (sendingpolicy.AccessRequest, bool, error) {
			return sendingpolicy.AccessRequest{}, false, sendingpolicy.ErrExternalAccessDisabled
		}
		d.LatestSendingAccessRequest = func(context.Context, string) (*sendingpolicy.AccessRequest, error) {
			return nil, sendingpolicy.ErrExternalAccessDisabled
		}
		d.NotifySendingAccessRequest = func(context.Context, string, sendingpolicy.AccessRequest) {
			t.Fatal("a disabled deployment must not notify the operator")
		}
	})
	if code, body := getJSON(t, srv.URL+"/v1/account", "good"); code != 200 || body["sending_access"] != nil {
		t.Fatalf("account: %d %v", code, body)
	}
	if code, body := getJSON(t, srv.URL+"/v1/account/sending-access/request", "good"); code != 501 || errCode(body) != "not_implemented" {
		t.Fatalf("get: %d %v", code, body)
	}
	form := map[string]any{"use_case": "x", "recipients": "y", "expected_daily_volume": 1}
	if code, body := sendJSON(t, http.MethodPost, srv.URL+"/v1/account/sending-access/request", "good", form); code != 501 || errCode(body) != "not_implemented" {
		t.Fatalf("post: %d %v", code, body)
	}
}
