package apiserver_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/sendingpolicy"
	"github.com/tokencanopy/e2a/internal/testutil"
)

// Read-only accounts over real HTTP against the full production composition
// (testutil.TestServer: apiserver.BuildDeps + the legacy mux) and real
// Postgres, with the pause applied the way an operator applies it
// (`-pause-account-sending -pause-class abuse` → sendingpolicy.SetAccountPause).
// docs/design/account-read-only.md.

func roDo(t *testing.T, method, url, key string, body any) (int, map[string]any) {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, url, rdr)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func operatorPause(t *testing.T, module *sendingpolicy.Module, userID, class string, paused bool) {
	t.Helper()
	if _, err := module.SetAccountPause(context.Background(), sendingpolicy.AccountPauseChange{
		AccountID: userID, Paused: paused, Class: class, Actor: "test-operator", Reason: "synthetic review reason",
	}); err != nil {
		t.Fatalf("SetAccountPause(%s, paused=%v): %v", class, paused, err)
	}
}

func TestReadOnlyAccountOverHTTP(t *testing.T) {
	pool := testutil.TestDB(t)
	ts := testutil.TestServer(t, pool)
	ctx := context.Background()
	base := ts.HTTPServer.URL
	module := sendingpolicy.NewModule(pool, sendingpolicy.Secrets{})

	user, err := ts.Store.CreateOrGetUser(ctx, "ro-http@example.test", "RO", "sub-ro-http")
	if err != nil {
		t.Fatal(err)
	}
	ag, err := ts.Store.CreateAgent(ctx, "ro-http-bot@agents.localhost", "agents.localhost", "RO Bot", "", "cloud", user.ID)
	if err != nil {
		t.Fatal(err)
	}
	accountKey, err := ts.Store.CreateAPIKey(ctx, user.ID, "account", nil)
	if err != nil {
		t.Fatal(err)
	}
	agentKey, err := ts.Store.CreateScopedAPIKey(ctx, user.ID, "agent", identity.ScopeAgent, ag.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	agentPath := base + "/v1/agents/" + strings.ReplaceAll(ag.EmailAddress(), "@", "%40")

	// Writable before the pause.
	if status, body := roDo(t, http.MethodPost, base+"/v1/contacts", accountKey.PlaintextKey, map[string]any{"address": "before@example.test"}); status != http.StatusCreated {
		t.Fatalf("create contact before the pause = %d %v", status, body)
	}
	if status, body := roDo(t, http.MethodGet, base+"/v1/account", accountKey.PlaintextKey, nil); status != 200 || body["read_only"] != false {
		t.Fatalf("GET /v1/account before the pause = %d read_only=%v, want 200 false", status, body["read_only"])
	}

	// An operator-class pause refuses only sending; writes keep working.
	operatorPause(t, module, user.ID, sendingpolicy.PauseClassOperator, true)
	if status, body := roDo(t, http.MethodPatch, agentPath, accountKey.PlaintextKey, map[string]any{"name": "Renamed Under Operator Pause"}); status != 200 {
		t.Fatalf("rename under an operator pause = %d %v, want 200", status, body)
	}

	// The abuse pause makes the account read-only, effective immediately.
	operatorPause(t, module, user.ID, sendingpolicy.PauseClassAbuse, true)

	type call struct {
		method, url string
		body        any
	}
	writes := []call{
		{http.MethodPost, base + "/v1/agents", map[string]any{"email": "brand-support@agents.localhost"}},
		{http.MethodPatch, agentPath, map[string]any{"name": "Brand Support"}},
		{http.MethodDelete, agentPath + "?confirm=DELETE", nil},
		{http.MethodPost, agentPath + "/messages", map[string]any{"to": []string{"x@example.test"}, "subject": "s", "text": "t"}},
		{http.MethodPost, base + "/v1/domains", map[string]any{"domain": "brand.example.test"}},
		{http.MethodPost, base + "/v1/account/api-keys", map[string]any{"name": "k"}},
		{http.MethodPost, base + "/v1/webhooks", map[string]any{"url": "https://hooks.example.test/x", "events": []string{"email.received"}}},
		{http.MethodPost, base + "/v1/contacts", map[string]any{"address": "after@example.test"}},
		{http.MethodPost, base + "/v1/templates", map[string]any{"name": "n", "subject": "s", "body": "b"}},
		{http.MethodPut, agentPath + "/protection", map[string]any{}},
		{http.MethodPost, base + "/v1/account/sending-access/request", map[string]any{"use_case": "x"}},
	}
	for _, key := range []string{accountKey.PlaintextKey, agentKey.PlaintextKey} {
		for _, c := range writes {
			status, body := roDo(t, c.method, c.url, key, c.body)
			e, _ := body["error"].(map[string]any)
			if status != http.StatusForbidden || e["code"] != "account_read_only" {
				t.Errorf("%s %s = %d %v, want 403 account_read_only", c.method, c.url, status, body)
				continue
			}
			if msg, _ := e["message"].(string); strings.Contains(msg, "synthetic review reason") {
				t.Errorf("%s %s leaked the operator reason: %q", c.method, c.url, msg)
			}
		}
	}

	// Nothing the refused writes named was changed.
	got, err := ts.Store.GetAgentByEmail(ctx, ag.EmailAddress())
	if err != nil || got == nil || got.Name != "Renamed Under Operator Pause" {
		t.Fatalf("agent changed under refused writes: %+v err=%v", got, err)
	}

	// Reads keep working, for both scopes.
	reads := []string{base + "/v1/account", agentPath, agentPath + "/messages", base + "/v1/account/export", base + "/v1/contacts"}
	for _, u := range reads {
		if status, body := roDo(t, http.MethodGet, u, accountKey.PlaintextKey, nil); status != 200 {
			t.Errorf("GET %s = %d %v, want 200", u, status, body)
		}
	}
	if status, body := roDo(t, http.MethodGet, base+"/v1/account", agentKey.PlaintextKey, nil); status != 200 || body["read_only"] != true {
		t.Errorf("GET /v1/account (agent scope) = %d read_only=%v, want 200 true", status, body["read_only"])
	}

	// A resume lifts read-only on the next request.
	operatorPause(t, module, user.ID, "", false)
	if status, body := roDo(t, http.MethodPost, base+"/v1/contacts", accountKey.PlaintextKey, map[string]any{"address": "resumed@example.test"}); status != http.StatusCreated {
		t.Fatalf("write after resume = %d %v, want 201", status, body)
	}

	// Pause again: the permanent erase stays held, and the trash stays open.
	operatorPause(t, module, user.ID, sendingpolicy.PauseClassAbuse, true)
	if status, body := roDo(t, http.MethodDelete, base+"/v1/account?confirm=DELETE&permanent=true", accountKey.PlaintextKey, nil); status != http.StatusConflict {
		t.Fatalf("permanent account delete while read-only = %d %v, want 409 erase_held", status, body)
	} else if e, _ := body["error"].(map[string]any); e["code"] != "erase_held" {
		t.Fatalf("permanent account delete code = %v, want erase_held", e["code"])
	}
	status, body := roDo(t, http.MethodDelete, base+"/v1/account?confirm=DELETE", accountKey.PlaintextKey, nil)
	if status != 200 || body["mode"] != "trash" {
		t.Fatalf("account trash while read-only = %d %v, want 200 mode=trash", status, body)
	}
}
