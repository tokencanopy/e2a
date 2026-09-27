package agent

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/gorilla/mux"
	"github.com/tokencanopy/e2a/internal/identity"
)

// Read-only accounts on the legacy (non-/v1) surface
// (docs/design/account-read-only.md). The /v1 surface enforces read-only in
// its own middleware (internal/httpapi/read_only.go); this is the single
// enforcement point for the gorilla/mux routes RegisterRoutes owns: the
// dashboard's legacy account routes and OAuth consent. Two token-authorized
// surfaces that are not account-principal requests check in their handlers:
// the HITL magic links (hitl_magic_api.go) and the internal external-principal
// attach (provisioning_api.go).

// legacyRouteAccess classifies a legacy route's writes.
type legacyRouteAccess int

const (
	// legacyAccountWrite changes the signed-in account's state; refused for
	// a read-only account.
	legacyAccountWrite legacyRouteAccess = iota + 1
	// legacyExempt is not refused here, for the reason recorded with it.
	legacyExempt
)

// legacyWriteRoutes classifies every non-GET route RegisterRoutes registers,
// keyed "METHOD path-template". TestLegacyWriteRoutesAreClassified walks the
// router so a new write route without an entry fails CI.
var legacyWriteRoutes = map[string]legacyRouteAccess{
	// Dashboard account routes (session cookie).
	"PATCH /api/auth/me":                   legacyAccountWrite,
	"PUT /api/dashboard/agents/{email}":    legacyAccountWrite,
	"DELETE /api/dashboard/agents/{email}": legacyAccountWrite,
	"POST /api/keys":                       legacyAccountWrite,
	"DELETE /api/keys/{id}":                legacyAccountWrite,
	// Consent mints a new OAuth grant (agent or account scope) for the
	// signed-in account.
	"POST /oauth2/consent": legacyAccountWrite,

	// Sign-out only ends the caller's own session.
	"POST /api/auth/logout": legacyExempt,
	// Anonymous, IP-limited support channel — it is how a paused customer
	// reaches the operator.
	"POST /api/feedback": legacyExempt,
	// Credential exchange and revocation: the tokens minted here resolve to
	// principals the /v1 guard refuses for writes, and revoking only removes
	// authority.
	"POST /oauth2/token":   legacyExempt,
	"POST /oauth2/revoke":  legacyExempt,
	"POST /agent/identity": legacyExempt,
	// Anonymous dynamic client registration: the client is not tied to any
	// account until consent, which is refused above.
	"POST /oauth2/register": legacyExempt,
	// Operator machine-to-machine endpoints (HMAC). The attach endpoint
	// refuses a read-only target account in its handler (the account is named
	// in the signed body).
	"POST /api/internal/limits/invalidate":                legacyExempt,
	"POST /api/internal/users/provision":                  legacyExempt,
	"POST /api/internal/users/external-principals/attach": legacyExempt,
}

// SetSupportContact sets the support address named in account_read_only
// errors on the legacy surface.
func (a *API) SetSupportContact(contact string) { a.supportContact = contact }

// writeAccountReadOnly writes the canonical 403 account_read_only envelope
// (the same shape and message as /v1).
func (a *API) writeAccountReadOnly(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"code":    identity.AccountReadOnlyCode,
			"message": identity.AccountReadOnlyMessage(a.supportContact),
		},
	})
}

// accountReadOnly reports whether userID is read-only. ok=false means the
// response has been written (the lookup failed; fail closed with 503).
func (a *API) accountReadOnly(w http.ResponseWriter, r *http.Request, userID string) (readOnly, ok bool) {
	ro, err := a.store.AccountReadOnly(r.Context(), userID)
	if err != nil {
		log.Printf("[api] read-only check failed: %v", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{
				"code":    "auth_unavailable",
				"message": "account state is temporarily unavailable; retry",
			},
		})
		return false, false
	}
	return ro, true
}

// refuseIfReadOnly writes the refusal and returns true when userID is
// read-only (or its state could not be read). For the token-authorized
// handlers that are not principal requests.
func (a *API) refuseIfReadOnly(w http.ResponseWriter, r *http.Request, userID string) bool {
	ro, ok := a.accountReadOnly(w, r, userID)
	if !ok {
		return true
	}
	if ro {
		a.writeAccountReadOnly(w)
		return true
	}
	return false
}

// legacyReadOnlyMiddleware is the gorilla middleware enforcing read-only on
// the legacy account-write routes. Reads and exempt routes pass untouched;
// an unauthenticated request falls through so the handler emits its own 401.
func (a *API) legacyReadOnlyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		route := mux.CurrentRoute(r)
		if route == nil {
			next.ServeHTTP(w, r)
			return
		}
		tmpl, err := route.GetPathTemplate()
		if err != nil || legacyWriteRoutes[r.Method+" "+tmpl] != legacyAccountWrite {
			next.ServeHTTP(w, r)
			return
		}
		p, err := a.authenticatePrincipal(r)
		if err != nil || p == nil || p.User == nil {
			next.ServeHTTP(w, r)
			return
		}
		if a.refuseIfReadOnly(w, r, p.User.ID) {
			return
		}
		next.ServeHTTP(w, r)
	})
}
