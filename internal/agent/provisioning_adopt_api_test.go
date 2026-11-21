package agent_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/mux"
	"github.com/jackc/pgx/v5"

	"github.com/tokencanopy/e2a/internal/agent"
	"github.com/tokencanopy/e2a/internal/auth"
	"github.com/tokencanopy/e2a/internal/config"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/outbound"
	"github.com/tokencanopy/e2a/internal/testutil"
	"github.com/tokencanopy/e2a/internal/usage"
)

const adoptAPIGoogleSub = "100000000000000000001"

// provisionRaw sends a signed provision to the attach-configured server (which
// has the delegated issuer adoption requires) and returns status and raw body.
func provisionRaw(t *testing.T, server *httptest.Server, body string) (int, []byte) {
	t.Helper()
	resp := provisionRequest(t, server, attachTestSecret, []byte(body))
	defer resp.Body.Close()
	buf, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, buf
}

func decodeObject(t *testing.T, buf []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(buf, &out); err != nil {
		t.Fatalf("not a JSON object: %q", buf)
	}
	return out
}

func adoptBody(ref, email, subject string) string {
	b, _ := json.Marshal(map[string]string{
		"external_issuer": attachTestIssuer,
		"external_ref":    ref,
		"email":           email,
		"google_subject":  subject,
	})
	return string(b)
}

func directSignup(t *testing.T, store *identity.Store, email string) *identity.User {
	t.Helper()
	u, err := store.CreateOrGetUser(context.Background(), email, "Direct Signup", adoptAPIGoogleSub)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestProvisionUser_AdoptsOnMatchingGoogleSubjectReturns200(t *testing.T) {
	server, store := setupAttachAPI(t, attachTestIssuer)
	existing := directSignup(t, store, "ada@example.test")

	for attempt := 0; attempt < 2; attempt++ {
		status, buf := provisionRaw(t, server, adoptBody("tcusr_ada", "ada@example.test", adoptAPIGoogleSub))
		if status != http.StatusOK {
			t.Fatalf("attempt %d: status = %d, want 200; body=%s", attempt, status, buf)
		}
		got := decodeObject(t, buf)
		if got["user_id"] != existing.ID || got["adopted"] != true {
			t.Fatalf("attempt %d: body = %s, want user_id=%s adopted=true", attempt, buf, existing.ID)
		}
	}
	mapped, err := store.GetUserByExternalPrincipal(context.Background(), attachTestIssuer, "tcusr_ada")
	if err != nil || mapped == nil || mapped.ID != existing.ID {
		t.Fatalf("mapping = %v, %v; want the adopted user", mapped, err)
	}
}

// The evidence must not be probeable: a wrong subject gets exactly the bytes
// a request without any subject gets.
func TestProvisionUser_SubjectMismatchIsIndistinguishableEmailConflict(t *testing.T) {
	server, store := setupAttachAPI(t, attachTestIssuer)
	directSignup(t, store, "ada@example.test")

	noSubject, _ := json.Marshal(map[string]string{
		"external_issuer": attachTestIssuer, "external_ref": "tcusr_a", "email": "ada@example.test",
	})
	wantStatus, want := provisionRaw(t, server, string(noSubject))
	gotStatus, got := provisionRaw(t, server, adoptBody("tcusr_b", "ada@example.test", "100000000000000000999"))
	if wantStatus != http.StatusConflict || gotStatus != wantStatus || string(got) != string(want) {
		t.Fatalf("mismatch = %d %q, no-subject = %d %q; want identical 409s", gotStatus, got, wantStatus, want)
	}
	if mapped, _ := store.GetUserByExternalPrincipal(context.Background(), attachTestIssuer, "tcusr_b"); mapped != nil {
		t.Fatalf("mismatch created a mapping to %s", mapped.ID)
	}
}

func TestProvisionUser_SubjectWithFreeEmailCreatesWithoutAdoptedField(t *testing.T) {
	server, _ := setupAttachAPI(t, attachTestIssuer)

	status, buf := provisionRaw(t, server, adoptBody("tcusr_new", "new@example.test", adoptAPIGoogleSub))
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", status, buf)
	}
	got := decodeObject(t, buf)
	if _, present := got["adopted"]; present || len(got) != 1 {
		t.Fatalf("body = %s, want exactly {user_id}", buf)
	}
}

func TestProvisionUser_AdoptingReadOnlyAccountIs403(t *testing.T) {
	server, store := setupAttachAPI(t, attachTestIssuer)
	existing := directSignup(t, store, "ada@example.test")
	ctx := context.Background()
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO account_sending_controls (user_id, state, reason, actor, pause_class)
			 VALUES ($1, 'paused', 'synthetic abuse', 'op', 'abuse')`, existing.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	status, buf := provisionRaw(t, server, adoptBody("tcusr_ada", "ada@example.test", adoptAPIGoogleSub))
	if status != http.StatusForbidden || decodeObject(t, buf)["error"] != identity.AccountReadOnlyCode {
		t.Fatalf("status = %d body = %s, want 403 %s", status, buf, identity.AccountReadOnlyCode)
	}
}

func TestProvisionUser_GoogleSubjectValidation(t *testing.T) {
	server, _ := setupAttachAPI(t, attachTestIssuer)

	withoutIssuer, _ := json.Marshal(map[string]string{
		"external_ref": "tcusr_x", "email": "x@example.test", "google_subject": adoptAPIGoogleSub,
	})
	cases := map[string]string{
		"synthetic bootstrap subject": adoptBody("tcusr_x", "x@example.test", "bootstrap:tcusr_owner"),
		"no external_issuer":          string(withoutIssuer),
		"oversize subject":            adoptBody("tcusr_x", "x@example.test", strings.Repeat("1", 256)),
		"control character":           adoptBody("tcusr_x", "x@example.test", "1000\n0001"),
		"non-numeric subject":         adoptBody("tcusr_x", "x@example.test", "e2a-prober-system"),
	}
	for name, body := range cases {
		status, buf := provisionRaw(t, server, body)
		if status != http.StatusBadRequest || decodeObject(t, buf)["error"] != "invalid_google_subject" {
			t.Errorf("%s: status = %d body = %s, want 400 invalid_google_subject", name, status, buf)
		}
	}
}

func TestProvisionUser_AdoptionRecordsAdoptedMetric(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	smtpRelay := outbound.NewSMTPRelay(&config.OutboundSMTPConfig{})
	api := agent.NewAPI(store, outbound.NewSender(smtpRelay, "test.e2a.dev"), smtpRelay,
		auth.NewUserAuth(&config.OAuthConfig{}, store, false), usage.NewNoopUsageTracker(),
		"e2a.dev", "test.e2a.dev", "agents.e2a.dev", "", false)
	api.ConfigureProvisioning(true, attachTestSecret)
	api.SetDelegatedIssuer(attachTestIssuer)
	metrics := &provisioningMetricsRecorder{}
	api.SetMetrics(metrics)
	router := mux.NewRouter()
	api.RegisterRoutes(router)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	directSignup(t, store, "ada@example.test")

	if status, buf := provisionRaw(t, server, adoptBody("tcusr_ada", "ada@example.test", adoptAPIGoogleSub)); status != http.StatusOK {
		t.Fatalf("status = %d body = %s", status, buf)
	}
	if got, want := metrics.last(t), (provisioningMetricEvent{outcome: "adopted", trust: "authenticated", statusClass: "2xx"}); got != want {
		t.Fatalf("metric = %+v, want %+v", got, want)
	}
}

// The no-evidence answer is decided by the evidence-less path, so a wrong
// subject matches it byte-for-byte even when that path refuses before the
// email check (here: tombstones on, no key configured).
func TestProvisionUser_SubjectMismatchMatchesNoSubjectWhenRegistrationUnavailable(t *testing.T) {
	server, store := setupAttachAPI(t, attachTestIssuer)
	directSignup(t, store, "ada@example.test")
	store.SetTombstonePolicy(identity.TombstonePolicy{Enabled: true})

	noSubject, _ := json.Marshal(map[string]string{
		"external_issuer": attachTestIssuer, "external_ref": "tcusr_a", "email": "ada@example.test",
	})
	wantStatus, want := provisionRaw(t, server, string(noSubject))
	gotStatus, got := provisionRaw(t, server, adoptBody("tcusr_a", "ada@example.test", "100000000000000000999"))
	if wantStatus != http.StatusServiceUnavailable || gotStatus != wantStatus || string(got) != string(want) {
		t.Fatalf("mismatch = %d %q, no-subject = %d %q; want identical 503s", gotStatus, got, wantStatus, want)
	}
}

func TestProvisionUser_AdoptingPairMappedElsewhereIs409(t *testing.T) {
	server, store := setupAttachAPI(t, attachTestIssuer)
	directSignup(t, store, "ada@example.test")
	other, err := store.BootstrapUser(context.Background(), "other@example.test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AttachExternalPrincipal(context.Background(), attachTestIssuer, "tcusr_ada", other.ID); err != nil {
		t.Fatal(err)
	}

	status, buf := provisionRaw(t, server, adoptBody("tcusr_ada", "ada@example.test", adoptAPIGoogleSub))
	if status != http.StatusConflict || decodeObject(t, buf)["error"] != "external_principal_conflict" {
		t.Fatalf("status = %d body = %s, want 409 external_principal_conflict", status, buf)
	}
}
