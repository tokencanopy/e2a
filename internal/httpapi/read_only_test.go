package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/tokencanopy/e2a/internal/identity"
	"gopkg.in/yaml.v3"
)

// Read-only accounts (docs/design/account-read-only.md): the spec walk.
//
// Expected classes are derived INDEPENDENTLY of operationAccess: the method
// rule (GET/HEAD read, everything else write) plus the exceptions below, each
// a deliberate product decision. The production table must agree with this
// derivation for every operation in the committed spec, and the live server
// must behave accordingly — so flipping one entry of operationAccess, or
// adding an operation without classifying it, fails this test.
var readOnlyExpectedExceptions = map[string]opAccess{
	// POST that validates a draft template and stores nothing.
	"validateTemplate": accessRead,
	// Moving the account to the trash stays available to a read-only account
	// (the permanent erase is refused separately with 409 erase_held).
	"deleteAccount": accessWriteAllowedReadOnly,
	// Public deployment discovery: no principal, no account.
	"getInfo": accessPublic,
}

type roSpecOp struct {
	ID, Method, Path string
}

func loadSpecOperations(t *testing.T) []roSpecOp {
	t.Helper()
	raw, err := os.ReadFile(specGoldenPath)
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Paths map[string]map[string]struct {
			OperationID string `yaml:"operationId"`
		} `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	var ops []roSpecOp
	for path, item := range spec.Paths {
		for method, op := range item {
			m := strings.ToUpper(method)
			switch m {
			case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
			default:
				continue
			}
			if op.OperationID == "" {
				t.Fatalf("%s %s has no operationId", m, path)
			}
			ops = append(ops, roSpecOp{ID: op.OperationID, Method: m, Path: path})
		}
	}
	sort.Slice(ops, func(i, j int) bool { return ops[i].ID < ops[j].ID })
	if len(ops) < 50 {
		t.Fatalf("spec walk found only %d operations; the spec parse is broken", len(ops))
	}
	return ops
}

func expectedAccess(op roSpecOp) opAccess {
	if a, ok := readOnlyExpectedExceptions[op.ID]; ok {
		return a
	}
	if op.Method == http.MethodGet || op.Method == http.MethodHead {
		return accessRead
	}
	return accessWrite
}

func (a opAccess) String() string {
	switch a {
	case accessRead:
		return "read"
	case accessWrite:
		return "write"
	case accessWriteAllowedReadOnly:
		return "write-allowed-while-read-only"
	case accessPublic:
		return "public"
	}
	return fmt.Sprintf("opAccess(%d)", int(a))
}

// TestReadOnlyClassificationCoversTheSpec: every operation in the committed
// spec is classified explicitly, the class matches the method rule and its
// named exceptions, and the table carries no stale entries.
func TestReadOnlyClassificationCoversTheSpec(t *testing.T) {
	ops := loadSpecOperations(t)
	inSpec := map[string]bool{}
	for _, op := range ops {
		inSpec[op.ID] = true
		got, ok := operationAccess[op.ID]
		if !ok {
			t.Errorf("%s (%s %s) is not classified in operationAccess: add it as read or write (read_only.go)", op.ID, op.Method, op.Path)
			continue
		}
		if want := expectedAccess(op); got != want {
			t.Errorf("%s (%s %s) is classified %s, want %s (method rule + readOnlyExpectedExceptions)", op.ID, op.Method, op.Path, got, want)
		}
	}
	for id := range operationAccess {
		if !inSpec[id] {
			t.Errorf("operationAccess classifies %q, which is not an operation in the spec (stale entry)", id)
		}
	}
	for id := range readOnlyExpectedExceptions {
		if !inSpec[id] {
			t.Errorf("readOnlyExpectedExceptions names %q, which is not an operation in the spec", id)
		}
	}
}

// readOnlyTestServer builds the real /v1 server with a principal
// authenticator keyed on the bearer and an AccountReadOnly stub reporting the
// given state. Every other dependency is nil: a write refused by the guard
// never reaches its handler, and a request that does reach a handler with a
// nil dependency is caught by the recover wrapper (status 599) — the walk only
// asserts what the guard did, not what the handler returned.
func readOnlyTestServer(t *testing.T, state func(userID string) (bool, error), calls *atomic.Int64) *httptest.Server {
	t.Helper()
	deps := Deps{
		PrincipalAuthenticator: func(r *http.Request) (*identity.Principal, error) {
			switch r.Header.Get("Authorization") {
			case "Bearer account":
				return &identity.Principal{User: &identity.User{ID: "u_ro", Email: "owner@example.test"}, Scope: identity.ScopeAccount}, nil
			case "Bearer nouser":
				// A resolver contract violation: no error, no account.
				return &identity.Principal{Scope: identity.ScopeAccount}, nil
			case "Bearer agent":
				// An agent access token / agent-scoped key / delegated token
				// all resolve to a principal owned by the same account.
				return &identity.Principal{User: &identity.User{ID: "u_ro", Email: "owner@example.test"}, Scope: identity.ScopeAgent, AgentID: "ro-bot@example.test"}, nil
			}
			return nil, errors.New("unauthorized")
		},
		AccountReadOnly: func(ctx context.Context, userID string) (bool, error) {
			calls.Add(1)
			return state(userID)
		},
		SupportContact: "help@example.test",
	}
	s := New(deps)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				w.WriteHeader(599)
			}
		}()
		s.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

var specParamValues = map[string]string{
	"{email}":    "ro-bot%40example.test",
	"{address}":  "someone%40example.test",
	"{domain}":   "example.test",
	"{id}":       "x_1",
	"{index}":    "0",
	"{batch_id}": "imp_1",
	"{alias}":    "welcome",
}

func concretePath(t *testing.T, specPath string) string {
	t.Helper()
	p := specParamPattern.ReplaceAllStringFunc(specPath, func(param string) string {
		v, ok := specParamValues[param]
		if !ok {
			t.Fatalf("no test value for path parameter %s in %s", param, specPath)
		}
		return v
	})
	return p
}

func doReadOnlyRequest(t *testing.T, srv *httptest.Server, op roSpecOp, bearer string) (int, string) {
	t.Helper()
	url := srv.URL + concretePath(t, op.Path)
	if op.Method == http.MethodDelete {
		url += "?confirm=DELETE"
	}
	var body *bytes.Reader
	if op.Method == http.MethodGet || op.Method == http.MethodHead {
		body = bytes.NewReader(nil)
	} else {
		body = bytes.NewReader([]byte(`{}`))
	}
	req, err := http.NewRequest(op.Method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", op.Method, url, err)
	}
	defer resp.Body.Close()
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&env)
	return resp.StatusCode, env.Error.Code
}

// TestReadOnlySpecWalk drives EVERY operation in the committed spec against
// the live router as a read-only account, with both an account-scoped and an
// agent-scoped principal: every write answers 403 account_read_only, every
// read, public operation and allowlisted write passes the guard untouched —
// and a read never even pays the read-only lookup.
func TestReadOnlySpecWalk(t *testing.T) {
	ops := loadSpecOperations(t)
	var calls atomic.Int64
	srv := readOnlyTestServer(t, func(string) (bool, error) { return true, nil }, &calls)

	for _, bearer := range []string{"account", "agent"} {
		for _, op := range ops {
			op := op
			t.Run(bearer+"/"+op.ID, func(t *testing.T) {
				before := calls.Load()
				status, code := doReadOnlyRequest(t, srv, op, bearer)
				consulted := calls.Load() > before
				switch expectedAccess(op) {
				case accessWrite:
					if status != http.StatusForbidden || code != "account_read_only" {
						t.Fatalf("%s %s as a read-only account = %d %q, want 403 account_read_only", op.Method, op.Path, status, code)
					}
				default:
					if code == "account_read_only" {
						t.Fatalf("%s %s (%s) was refused as read-only", op.Method, op.Path, expectedAccess(op))
					}
					if consulted {
						t.Fatalf("%s %s (%s) consulted the read-only state; only writes may pay that lookup", op.Method, op.Path, expectedAccess(op))
					}
				}
			})
		}
	}
}

// TestReadOnlyGuardLetsWritesThroughForAWritableAccount: the same walk for an
// account that is NOT read-only never produces account_read_only (the guard
// consults the state instead of refusing blanket), and a write for an
// unauthenticated caller falls through to the handler's canonical 401.
func TestReadOnlyGuardLetsWritesThroughForAWritableAccount(t *testing.T) {
	ops := loadSpecOperations(t)
	var calls atomic.Int64
	srv := readOnlyTestServer(t, func(string) (bool, error) { return false, nil }, &calls)
	for _, op := range ops {
		if expectedAccess(op) != accessWrite {
			continue
		}
		status, code := doReadOnlyRequest(t, srv, op, "account")
		if code == "account_read_only" {
			t.Errorf("%s %s refused a writable account (%d)", op.Method, op.Path, status)
		}
	}
	if calls.Load() == 0 {
		t.Fatal("no write consulted the read-only state")
	}

	op := roSpecOp{ID: "deleteApiKey", Method: http.MethodDelete, Path: "/v1/account/api-keys/{id}"}
	before := calls.Load()
	status, code := doReadOnlyRequest(t, srv, op, "nobody")
	if status != http.StatusUnauthorized || code != "unauthorized" {
		t.Fatalf("unauthenticated write = %d %q, want the handler's 401 unauthorized", status, code)
	}
	if calls.Load() != before {
		t.Fatal("an unauthenticated write consulted the read-only state")
	}
}

// TestReadOnlyGuardFailsClosed: when the read-only state cannot be read, a
// write is refused with a retryable 503 and never runs; a read is unaffected.
func TestReadOnlyGuardFailsClosed(t *testing.T) {
	var calls atomic.Int64
	srv := readOnlyTestServer(t, func(string) (bool, error) { return false, errors.New("db down") }, &calls)

	status, code := doReadOnlyRequest(t, srv, roSpecOp{ID: "createContact", Method: http.MethodPost, Path: "/v1/contacts"}, "account")
	if status != http.StatusServiceUnavailable || code != "auth_unavailable" {
		t.Fatalf("write with an unreadable account state = %d %q, want 503 auth_unavailable", status, code)
	}
	status, code = doReadOnlyRequest(t, srv, roSpecOp{ID: "getAccount", Method: http.MethodGet, Path: "/v1/account"}, "account")
	if status == http.StatusServiceUnavailable && code == "auth_unavailable" {
		t.Fatalf("a read failed on the read-only lookup (%d %q); reads must not consult it", status, code)
	}
}

// TestReadOnlyGuardRefusesAPrincipalWithoutAnAccount: a principal that
// resolved without error but carries no account is refused (503), never
// dereferenced or let through.
func TestReadOnlyGuardRefusesAPrincipalWithoutAnAccount(t *testing.T) {
	var calls atomic.Int64
	srv := readOnlyTestServer(t, func(string) (bool, error) { return false, nil }, &calls)
	status, code := doReadOnlyRequest(t, srv, roSpecOp{ID: "createContact", Method: http.MethodPost, Path: "/v1/contacts"}, "nouser")
	if status != http.StatusServiceUnavailable || code != "auth_unavailable" {
		t.Fatalf("write by a principal without an account = %d %q, want 503 auth_unavailable", status, code)
	}
	if calls.Load() != 0 {
		t.Fatal("the read-only state was consulted without an account id")
	}
}

// TestAccountReadOnlyErrorMessage: the message names the state and the way
// out, carries the configured support contact, and falls back cleanly.
func TestAccountReadOnlyErrorMessage(t *testing.T) {
	with := identity.AccountReadOnlyMessage("help@example.test")
	for _, want := range []string{"sending is paused", "abuse review", "read-only", "help@example.test"} {
		if !strings.Contains(with, want) {
			t.Errorf("message %q lacks %q", with, want)
		}
	}
	without := identity.AccountReadOnlyMessage("")
	if !strings.Contains(without, "Contact support to appeal.") || strings.Contains(without, "()") {
		t.Errorf("message without a contact = %q", without)
	}
}

// TestClassifyOperationFallsBackToTheMethodRule: an operation missing from the
// table (which the coverage test forbids) is still refused when it writes.
func TestClassifyOperationFallsBackToTheMethodRule(t *testing.T) {
	cases := map[string]opAccess{
		http.MethodGet: accessRead, http.MethodHead: accessRead,
		http.MethodPost: accessWrite, http.MethodPut: accessWrite,
		http.MethodPatch: accessWrite, http.MethodDelete: accessWrite,
	}
	for method, want := range cases {
		if got := classifyOperation(&huma.Operation{OperationID: "notInTheTable", Method: method}); got != want {
			t.Errorf("unclassified %s = %s, want %s", method, got, want)
		}
	}
}
