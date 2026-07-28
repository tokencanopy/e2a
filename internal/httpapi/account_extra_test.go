package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/tokencanopy/e2a/internal/identity"
)

func TestExportUserData(t *testing.T) {
	srv := testServer(t)
	req, _ := http.NewRequest("GET", srv.URL+"/v1/account/export", nil)
	req.Header.Set("Authorization", "Bearer good")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Fatalf("expected attachment Content-Disposition, got %q", cd)
	}
}

func TestDeleteAccountRequiresConfirm(t *testing.T) {
	srv := testServer(t)
	// The confirm guard is now modeled as a required enum:[DELETE] query param,
	// so Huma rejects a missing/wrong value with 422 before the handler runs.
	code, body := sendJSON(t, "DELETE", srv.URL+"/v1/account", "good", nil)
	if code != 422 || errCode(body) != "invalid_request" {
		t.Fatalf("want 422 invalid_request, got %d %v", code, body)
	}
	code, body = sendJSON(t, "DELETE", srv.URL+"/v1/account?confirm=nope", "good", nil)
	if code != 422 || errCode(body) != "invalid_request" {
		t.Fatalf("want 422 invalid_request for wrong value, got %d %v", code, body)
	}
}

func TestDeleteAccountConfirmed(t *testing.T) {
	srv := testServer(t)
	code, _ := sendJSON(t, "DELETE", srv.URL+"/v1/account?confirm=DELETE", "good", nil)
	if code != 200 {
		t.Fatalf("want 200, got %d", code)
	}
}

func TestDeleteAccountSendInProgress(t *testing.T) {
	srv := testServer(t, func(d *Deps) {
		d.DeleteUserData = func(context.Context, *identity.User) (*identity.DeleteUserDataResult, error) {
			return nil, identity.ErrSendInProgress
		}
	})
	code, body := sendJSON(t, "DELETE", srv.URL+"/v1/account?confirm=DELETE", "good", nil)
	if code != 409 || errCode(body) != "send_in_progress" {
		t.Fatalf("want 409 send_in_progress, got %d %v", code, body)
	}
	response := New(Deps{}).API.OpenAPI().Paths["/v1/account"].Delete.Responses["409"]
	if response == nil {
		t.Fatal("deleteAccount does not declare a 409 response")
	}
	content := response.Content["application/json"]
	if content == nil || content.Schema == nil || content.Schema.Ref != "#/components/schemas/ErrorEnvelope" {
		t.Fatalf("deleteAccount 409 schema = %#v, want ErrorEnvelope", content)
	}
}

func TestDeleteAccountUnauthorized(t *testing.T) {
	srv := testServer(t)
	code, _ := sendJSON(t, "DELETE", srv.URL+"/v1/account?confirm=DELETE", "", nil)
	if code != 401 {
		t.Fatalf("want 401, got %d", code)
	}
}
