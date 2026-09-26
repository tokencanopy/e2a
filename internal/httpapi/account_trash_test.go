package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tokencanopy/e2a/internal/identity"
)

func TestDeleteAccountForwardsPermanentAndDefaultsToTrash(t *testing.T) {
	var got []bool
	purgeAfter := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	srv := testServer(t, func(d *Deps) {
		d.DeleteUserData = func(_ context.Context, _ *identity.User, permanent bool) (*identity.DeleteUserDataResult, error) {
			got = append(got, permanent)
			if permanent {
				return &identity.DeleteUserDataResult{Mode: identity.AccountDeleteModePermanent, MessagesDeleted: 7, UserDeleted: true}, nil
			}
			return &identity.DeleteUserDataResult{Mode: identity.AccountDeleteModeTrash, PurgeAfter: &purgeAfter, AgentsDeleted: 2}, nil
		}
	})
	code, body := sendJSON(t, "DELETE", srv.URL+"/v1/account?confirm=DELETE", "good", nil)
	if code != 200 || body["mode"] != "trash" || body["deleted"] != true || body["user_deleted"] != false || body["messages_deleted"] != float64(0) {
		t.Fatalf("trash receipt = %d %v", code, body)
	}
	if body["purge_after"] != "2026-10-26T00:00:00Z" {
		t.Fatalf("purge_after = %v", body["purge_after"])
	}
	code, body = sendJSON(t, "DELETE", srv.URL+"/v1/account?confirm=DELETE&permanent=true", "good", nil)
	if code != 200 || body["mode"] != "permanent" || body["user_deleted"] != true || body["messages_deleted"] != float64(7) {
		t.Fatalf("permanent receipt = %d %v", code, body)
	}
	if _, ok := body["purge_after"]; ok {
		t.Fatal("a permanent receipt carries purge_after")
	}
	if len(got) != 2 || got[0] || !got[1] {
		t.Fatalf("permanent forwarded as %v, want [false true]", got)
	}
}

func TestDeleteAccountMapsEraseFailures(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		code int
		want string
	}{
		"tombstone key": {identity.ErrTombstoneKeyUnavailable, 503, "internal_error"},
		"purge claimed": {identity.ErrPurgeInProgress, 409, "purge_in_progress"},
		"send lease":    {identity.ErrSendInProgress, 409, "send_in_progress"},
	} {
		t.Run(name, func(t *testing.T) {
			srv := testServer(t, func(d *Deps) {
				d.DeleteUserData = func(context.Context, *identity.User, bool) (*identity.DeleteUserDataResult, error) {
					return nil, tc.err
				}
			})
			code, body := sendJSON(t, "DELETE", srv.URL+"/v1/account?confirm=DELETE&permanent=true", "good", nil)
			if code != tc.code || errCode(body) != tc.want {
				t.Fatalf("got %d %v, want %d %s", code, body, tc.code, tc.want)
			}
		})
	}
}

func TestAccountStatusCarriesRestoredAt(t *testing.T) {
	restored := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	srv := testServer(t, func(d *Deps) {
		d.Authenticator = func(r *http.Request) (*identity.User, error) {
			return &identity.User{ID: "u_1", Email: "owner@acme.com", RestoredAt: &restored}, nil
		}
	})
	code, body := sendJSON(t, "GET", srv.URL+"/v1/account", "good", nil)
	if code != 200 || body["restored_at"] != "2026-09-01T12:00:00Z" {
		t.Fatalf("account status = %d %v", code, body)
	}
	if _, ok := body["deleted_at"]; ok {
		t.Fatal("a live account reports deleted_at")
	}
}

// restoreServer wires the interstitial routes around a fake restricted session.
func restoreServer(t *testing.T, restoreErr error, eraseErr error) (*http.Client, string, *[]string) {
	t.Helper()
	calls := &[]string{}
	deleted := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	srv := testServer(t, func(d *Deps) {
		d.RestrictedSession = func(r *http.Request) (*identity.User, string, error) {
			c, err := r.Cookie("e2a_session")
			if err != nil {
				return nil, "", err
			}
			if c.Value != "sess_restricted" {
				return nil, "", pgx.ErrNoRows
			}
			return &identity.User{ID: "u_trashed", Email: "gone@example.test", DeletedAt: &deleted}, c.Value, nil
		}
		d.RestoreAccount = func(_ context.Context, userID, token string) (*identity.User, error) {
			*calls = append(*calls, "restore:"+userID+":"+token)
			if restoreErr != nil {
				return nil, restoreErr
			}
			now := time.Now()
			return &identity.User{ID: userID, RestoredAt: &now}, nil
		}
		d.DeleteUserData = func(_ context.Context, u *identity.User, permanent bool) (*identity.DeleteUserDataResult, error) {
			if !permanent {
				t.Error("the interstitial erase must be permanent")
			}
			*calls = append(*calls, "erase:"+u.ID)
			if eraseErr != nil {
				return nil, eraseErr
			}
			return &identity.DeleteUserDataResult{Mode: identity.AccountDeleteModePermanent, UserDeleted: true}, nil
		}
		d.ClearSessionCookie = func(w http.ResponseWriter) {
			http.SetCookie(w, &http.Cookie{Name: "e2a_session", Value: "", MaxAge: -1})
		}
	})
	return srv.Client(), srv.URL, calls
}

func doCookie(t *testing.T, c *http.Client, method, url, cookie string) (int, map[string]any, *http.Response) {
	t.Helper()
	req, _ := http.NewRequest(method, url, nil)
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: "e2a_session", Value: cookie})
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	return resp.StatusCode, body, resp
}

func TestAccountRestoreInterstitial(t *testing.T) {
	c, base, calls := restoreServer(t, nil, nil)

	// No session, or an ordinary one: nothing to restore.
	for _, cookie := range []string{"", "sess_ordinary"} {
		if code, body, _ := doCookie(t, c, "POST", base+"/api/account/restore", cookie); code != 401 || errCode(body) != "unauthorized" {
			t.Fatalf("cookie %q: %d %v, want 401", cookie, code, body)
		}
	}
	code, body, _ := doCookie(t, c, "GET", base+"/api/account/deletion", "sess_restricted")
	if code != 200 || body["email"] != "gone@example.test" || body["deleted_at"] != "2026-09-20T00:00:00Z" || body["purge_after"] == nil {
		t.Fatalf("deletion state = %d %v", code, body)
	}
	code, body, _ = doCookie(t, c, "POST", base+"/api/account/restore", "sess_restricted")
	if code != 200 || body["restored"] != true {
		t.Fatalf("restore = %d %v", code, body)
	}
	if len(*calls) != 1 || (*calls)[0] != "restore:u_trashed:sess_restricted" {
		t.Fatalf("restore calls = %v", *calls)
	}
	// The restricted session cannot reach the /v1 surface.
	if code, _, _ := doCookie(t, c, "GET", base+"/v1/account", "sess_restricted"); code != 401 {
		t.Fatalf("restricted session reached /v1/account: %d", code)
	}
}

func TestAccountRestoreRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		code int
		want string
	}{
		"closed identity": {identity.ErrRegistrationRefused, 403, "registration_refused"},
		"purge claimed":   {identity.ErrPurgeInProgress, 409, "purge_in_progress"},
		"not trashed":     {identity.ErrNotInTrash, 409, "not_in_trash"},
		"gone":            {pgx.ErrNoRows, 401, "unauthorized"},
		"store failure":   {errors.New("boom"), 500, "internal_error"},
	} {
		t.Run(name, func(t *testing.T) {
			c, base, _ := restoreServer(t, tc.err, nil)
			code, body, _ := doCookie(t, c, "POST", base+"/api/account/restore", "sess_restricted")
			if code != tc.code || errCode(body) != tc.want {
				t.Fatalf("got %d %v, want %d %s", code, body, tc.code, tc.want)
			}
		})
	}
}

func TestAccountEraseThroughRestrictedSession(t *testing.T) {
	c, base, calls := restoreServer(t, nil, nil)
	code, body, resp := doCookie(t, c, "POST", base+"/api/account/erase", "sess_restricted")
	if code != 200 || body["mode"] != "permanent" || body["deleted"] != true {
		t.Fatalf("erase = %d %v", code, body)
	}
	if len(*calls) != 1 || (*calls)[0] != "erase:u_trashed" {
		t.Fatalf("erase calls = %v", *calls)
	}
	cleared := false
	for _, ck := range resp.Cookies() {
		if ck.Name == "e2a_session" && ck.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("erase did not clear the session cookie")
	}

	c2, base2, _ := restoreServer(t, nil, identity.ErrTombstoneKeyUnavailable)
	if code, body, _ := doCookie(t, c2, "POST", base2+"/api/account/erase", "sess_restricted"); code != 503 || errCode(body) != "internal_error" {
		t.Fatalf("erase without a tombstone key = %d %v", code, body)
	}
}
