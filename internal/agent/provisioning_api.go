package agent

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/mail"

	"github.com/tokencanopy/e2a/internal/identity"
)

// provisionUserRequest is the body of POST /api/internal/users/provision.
type provisionUserRequest struct {
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
	email := identity.NormalizeEmail(req.Email)
	parsed, err := mail.ParseAddress(email)
	if err != nil || parsed.Address != email {
		writeProvisionError(w, http.StatusBadRequest, "invalid_email")
		return
	}

	user, created, err := a.store.ProvisionUser(r.Context(), req.ExternalRef, email, req.Name)
	if errors.Is(err, identity.ErrEmailConflict) {
		// A DIFFERENT account already holds this email. Never attach, never
		// merge — reconciliation is the operator's/user's decision.
		writeProvisionError(w, http.StatusConflict, "email_conflict")
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
