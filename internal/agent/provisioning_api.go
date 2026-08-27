package agent

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/mail"
	"unicode/utf8"

	"github.com/tokencanopy/e2a/internal/identity"
)

// provisionUserRequest is the body of POST /api/internal/users/provision.
type provisionUserRequest struct {
	// ExternalIssuer, when present, is the control plane's exact
	// configured OIDC issuer: the provision then ALSO inserts/replays the
	// delegated (issuer, external_ref) → user mapping transactionally.
	// Omitted (generic legacy callers) preserves the pre-delegated
	// behavior exactly and creates no mapping.
	ExternalIssuer string `json:"external_issuer"`
	// ExternalRef is the caller's stable idempotency key, 1..128 bytes. It
	// becomes the row's google_subject ("bootstrap:"+ref), so a replay of
	// the same ref always returns the same user.
	ExternalRef string `json:"external_ref"`
	Email       string `json:"email"`
	Name        string `json:"name"` // optional; '' default
}

// handleProvisionUser creates an e2a user on behalf of an external control
// plane, idempotently keyed by the caller's external_ref. The control plane
// calls this ahead of a user's first sign-in; dashboard access then arrives
// through the normal login paths, which resolve the user row. No session,
// no API key, no limits row is created here.
//
// Authentication is a dedicated shared HMAC over the request body (separate
// from the limits internal API secret so each can be rotated and revoked
// independently). The control plane signs with the same secret the OSS
// server is configured with, sends the hex digest in
// X-E2A-Internal-Signature, and the server verifies with a constant-time
// compare.
//
// The endpoint is intentionally not advertised in the OpenAPI spec — it's
// an internal seam between the OSS server and its operator's control plane.
// Self-hosters leave provisioning disabled and the endpoint 503s.
//
// Unlike the text/plain limits-invalidate surface, responses here are JSON
// because the caller must branch on machine-readable outcomes (created vs
// replayed vs email-conflict).
func (a *API) handleProvisionUser(w http.ResponseWriter, r *http.Request) {
	if !a.provisioningEnabled || a.provisioningSecret == "" {
		writeProvisionError(w, http.StatusServiceUnavailable, "provisioning_not_configured")
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1024))
	if err != nil {
		writeProvisionError(w, http.StatusBadRequest, "invalid_request")
		return
	}

	sig := r.Header.Get("X-E2A-Internal-Signature")
	if sig == "" {
		writeProvisionError(w, http.StatusUnauthorized, "missing_signature")
		return
	}
	expected := hmacHexSHA256([]byte(a.provisioningSecret), body)
	if subtle.ConstantTimeCompare([]byte(sig), []byte(expected)) != 1 {
		writeProvisionError(w, http.StatusUnauthorized, "invalid_signature")
		return
	}

	var req provisionUserRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeProvisionError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if len(req.ExternalRef) == 0 || len(req.ExternalRef) > 128 {
		writeProvisionError(w, http.StatusBadRequest, "invalid_external_ref")
		return
	}
	if req.ExternalIssuer != "" && !validExternalIssuer(req.ExternalIssuer) {
		writeProvisionError(w, http.StatusBadRequest, "invalid_external_issuer")
		return
	}
	email := identity.NormalizeEmail(req.Email)
	parsed, err := mail.ParseAddress(email)
	if err != nil || parsed.Address != email {
		writeProvisionError(w, http.StatusBadRequest, "invalid_email")
		return
	}

	user, created, err := a.store.ProvisionUser(r.Context(), req.ExternalRef, email, req.Name, req.ExternalIssuer)
	if errors.Is(err, identity.ErrEmailConflict) {
		// A DIFFERENT account already holds this email. Never attach, never
		// merge — reconciliation is the operator's/user's decision.
		writeProvisionError(w, http.StatusConflict, "email_conflict")
		return
	}
	if errors.Is(err, identity.ErrExternalPrincipalConflict) {
		// The (issuer, ref) pair is mapped to a DIFFERENT user than the one
		// this ref provisions to — a mapping/control-plane disagreement that
		// is never auto-resolved. Nothing was written.
		writeProvisionError(w, http.StatusConflict, "external_principal_conflict")
		return
	}
	if err != nil {
		log.Printf("[api] provision user failed: %v", err)
		writeProvisionError(w, http.StatusInternalServerError, "internal_error")
		return
	}

	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"user_id": user.ID})
}

func writeProvisionError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}

// attachExternalPrincipalRequest is the body of
// POST /api/internal/users/external-principals/attach.
type attachExternalPrincipalRequest struct {
	Issuer      string `json:"issuer"`
	ExternalRef string `json:"external_ref"`
	UserID      string `json:"user_id"`
}

// handleAttachExternalPrincipal maps an external OIDC (issuer, subject)
// pair to an EXISTING user for delegated-token authentication — the
// reconciliation path for accounts that predate delegated provisioning
// (whose google_subject must never be overwritten). Same HMAC boundary
// and bespoke JSON error shape as provisioning; additionally gated on a
// configured delegated issuer, which the body's issuer must equal
// byte-for-byte. Idempotent on the same triple (200); a pair attached to
// another user is 409 external_principal_conflict; an unknown user is
// 404 user_not_found. It never touches email/name/google_subject.
func (a *API) handleAttachExternalPrincipal(w http.ResponseWriter, r *http.Request) {
	if !a.provisioningEnabled || a.provisioningSecret == "" {
		writeProvisionError(w, http.StatusServiceUnavailable, "provisioning_not_configured")
		return
	}
	if a.delegatedIssuer == "" {
		writeProvisionError(w, http.StatusServiceUnavailable, "delegated_verifier_not_configured")
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1024))
	if err != nil {
		writeProvisionError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	sig := r.Header.Get("X-E2A-Internal-Signature")
	if sig == "" {
		writeProvisionError(w, http.StatusUnauthorized, "missing_signature")
		return
	}
	expected := hmacHexSHA256([]byte(a.provisioningSecret), body)
	if subtle.ConstantTimeCompare([]byte(sig), []byte(expected)) != 1 {
		writeProvisionError(w, http.StatusUnauthorized, "invalid_signature")
		return
	}

	var req attachExternalPrincipalRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeProvisionError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if req.Issuer != a.delegatedIssuer {
		writeProvisionError(w, http.StatusBadRequest, "invalid_issuer")
		return
	}
	if !validExternalRef(req.ExternalRef) {
		writeProvisionError(w, http.StatusBadRequest, "invalid_external_ref")
		return
	}
	if req.UserID == "" {
		writeProvisionError(w, http.StatusBadRequest, "invalid_user_id")
		return
	}

	created, err := a.store.AttachExternalPrincipal(r.Context(), req.Issuer, req.ExternalRef, req.UserID)
	switch {
	case errors.Is(err, identity.ErrExternalPrincipalConflict):
		writeProvisionError(w, http.StatusConflict, "external_principal_conflict")
		return
	case errors.Is(err, identity.ErrExternalPrincipalUserNotFound):
		writeProvisionError(w, http.StatusNotFound, "user_not_found")
		return
	case err != nil:
		log.Printf("[api] attach external principal failed: %v", err)
		writeProvisionError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"user_id": req.UserID})
}

// validExternalRef bounds a delegated external reference: 1..128 code
// points, at most 512 UTF-8 bytes, valid UTF-8, no control characters.
func validExternalRef(s string) bool {
	if s == "" || len(s) > 512 || !utf8.ValidString(s) {
		return false
	}
	n := 0
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
		n++
	}
	return n <= 128
}

// validExternalIssuer bounds a provisioning external issuer: 1..512 code
// points and at most 2048 UTF-8 bytes.
func validExternalIssuer(s string) bool {
	return s != "" && len(s) <= 2048 && utf8.ValidString(s) &&
		utf8.RuneCountInString(s) <= 512
}
