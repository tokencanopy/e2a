package agent

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
)

// LimitsInfo is the response body for GET /v1/account. It
// bundles the user's resolved caps with their current usage so the
// dashboard renders the "you're using X of Y" surface in one round
// trip. plan_code and upgrade_url come straight from the limits row
// (opaque to OSS — written by whatever provisions the row); when no
// row exists, plan_code is the operator default and upgrade_url is
// empty.
type LimitsInfo struct {
	PlanCode   string      `json:"plan_code"`
	Limits     LimitsCaps  `json:"limits"`
	Usage      LimitsUsage `json:"usage"`
	UpgradeURL string      `json:"upgrade_url"`
} // @name LimitsInfo

type LimitsCaps struct {
	MaxAgents        int   `json:"max_agents"`
	MaxDomains       int   `json:"max_domains"`
	MaxMessagesMonth int   `json:"max_messages_month"`
	MaxStorageBytes  int64 `json:"max_storage_bytes"`
} // @name LimitsCaps

type LimitsUsage struct {
	Agents        int   `json:"agents"`
	Domains       int   `json:"domains"`
	MessagesMonth int   `json:"messages_month"`
	StorageBytes  int64 `json:"storage_bytes"`
} // @name LimitsUsage

// invalidateLimitsRequest is the body of POST /api/internal/limits/invalidate.
type invalidateLimitsRequest struct {
	UserID string `json:"user_id"`
}

// handleInvalidateLimits busts the in-process limits cache for the
// given user. Called by the external provisioner (billing sidecar)
// immediately after it writes account_limits, so the next request from
// that user sees the new caps without waiting ~60s for natural TTL
// expiry.
//
// Authentication is a shared HMAC over the request body. The sidecar
// signs with the same secret the OSS server is configured with, sends
// the hex digest in X-E2A-Internal-Signature, and the OSS server
// verifies with a constant-time compare. Anything else (bearer tokens,
// API keys) would either tangle this machine-to-machine endpoint with
// user-scoped auth or require a separate credential store.
//
// The endpoint is intentionally not advertised in the OpenAPI spec —
// it's an internal seam between the OSS server and its operator's
// provisioner. Self-hosters who don't run a provisioner simply leave
// InternalAPISecret empty and the endpoint 503s.
//
// Scope caveat: the cache this busts is PER PROCESS. One POST clears it
// in exactly the server process that handled the request. An operator
// running several server replicas behind a load balancer must fan the
// call out to every replica (or accept that non-targeted replicas keep
// serving the old caps until limits.cache_ttl_seconds expires); a
// single POST through the load balancer only reaches one of them. The
// enforcer's generation guard makes each individual invalidation
// reliable — it cannot make one invalidation reach processes it was
// never sent to.
func (a *API) handleInvalidateLimits(w http.ResponseWriter, r *http.Request) {
	if a.internalAPISecret == "" {
		http.Error(w, "internal api not configured", http.StatusServiceUnavailable)
		return
	}
	if a.enforcer == nil {
		http.Error(w, "limits subsystem not configured", http.StatusServiceUnavailable)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1024))
	if err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	sig := r.Header.Get("X-E2A-Internal-Signature")
	if sig == "" {
		http.Error(w, "missing signature", http.StatusUnauthorized)
		return
	}
	expected := hmacHexSHA256([]byte(a.internalAPISecret), body)
	if subtle.ConstantTimeCompare([]byte(sig), []byte(expected)) != 1 {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	var req invalidateLimitsRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.UserID == "" {
		http.Error(w, "user_id is required", http.StatusBadRequest)
		return
	}

	a.enforcer.Invalidate(req.UserID)
	w.WriteHeader(http.StatusNoContent)
}

func hmacHexSHA256(key, body []byte) string {
	h := hmac.New(sha256.New, key)
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}
