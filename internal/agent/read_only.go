package agent

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/gorilla/mux"
	"github.com/tokencanopy/e2a/internal/auth"
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
	// legacyAccountWrite changes the signed-in account's state and
	// authenticates the dashboard session cookie ONLY; refused for a
	// read-only account. Classify a route this way only if its handler
	// authenticates with a.userAuth.AuthenticateRequest — the guard resolves
	// the caller the same way.
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
	// signed-in account, so a read-only account's "allow" is refused — in
	// handleOAuthConsent, after the provider/authorize-request handling and
	// the session check (same cookie lookup), so unauthenticated callers keep
	// consent's own 404/503/authorize errors and "deny" still returns
	// fosite's access_denied redirect to the client.
	"POST /oauth2/consent": legacyExempt,

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
	writeLegacyError(w, http.StatusForbidden, identity.AccountReadOnlyCode, identity.AccountReadOnlyMessage(a.supportContact))
}

// accountReadOnly reports whether userID is read-only. ok=false means the
// response has been written (the lookup failed; fail closed with 503).
func (a *API) accountReadOnly(w http.ResponseWriter, r *http.Request, userID string) (readOnly, ok bool) {
	ro, err := a.store.AccountReadOnly(r.Context(), userID)
	if err != nil {
		log.Printf("[api] read-only check failed: %v", err)
		writeLegacyError(w, http.StatusServiceUnavailable, "auth_unavailable", "account state is temporarily unavailable; retry")
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

// writeLegacyError writes a JSON error envelope in the legacy surface's shape.
func writeLegacyError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"code": code, "message": message},
	})
}

// legacyReadOnlyMiddleware is the gorilla middleware enforcing read-only on
// the legacy write routes. Reads and exempt routes pass untouched.
//
// legacyAccountWrite routes authenticate the dashboard session cookie ONLY
// (auth.UserAuth handlers and OAuth consent), so the guard resolves the
// caller exactly that way and checks read-only for that user. An
// Authorization header on such a route is refused outright
// (400 ambiguous_credentials): the guard and the handler must never be able
// to disagree about who is calling. A request with no valid session is
// refused with 401 here rather than passed on.
//
// A write route missing from legacyWriteRoutes (which
// TestLegacyWriteRoutesAreClassified forbids for RegisterRoutes' own routes)
// is treated as an account write of unknown authentication: every credential
// presented is resolved and the request is refused if any of them fails or
// belongs to a read-only account. An anonymous request carries no account and
// passes — that keeps routes the binary mounts on this router outside
// RegisterRoutes (cmd/e2a's SNS POST /webhooks/ses) working.
func (a *API) legacyReadOnlyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		// gorilla runs middleware on matched routes only; unmatched requests
		// go to the 404/405 handlers without reaching here.
		route := mux.CurrentRoute(r)
		if route == nil {
			next.ServeHTTP(w, r)
			return
		}
		tmpl, err := route.GetPathTemplate()
		if err != nil {
			writeLegacyError(w, http.StatusInternalServerError, "internal_error", "route could not be classified")
			return
		}
		switch legacyWriteRoutes[r.Method+" "+tmpl] {
		case legacyExempt:
			next.ServeHTTP(w, r)
		case legacyAccountWrite:
			a.guardSessionWrite(w, r, next)
		default:
			a.guardUnclassifiedWrite(w, r, next)
		}
	})
}

// guardSessionWrite enforces read-only on a session-cookie-only write route.
func (a *API) guardSessionWrite(w http.ResponseWriter, r *http.Request, next http.Handler) {
	if r.Header.Get("Authorization") != "" {
		writeLegacyError(w, http.StatusBadRequest, "ambiguous_credentials",
			"this endpoint authenticates with the dashboard session only; do not send an Authorization header")
		return
	}
	if a.userAuth == nil {
		// No session authentication is configured, so the handler can
		// authenticate nobody (it answers 503/404 itself).
		next.ServeHTTP(w, r)
		return
	}
	user := a.userAuth.AuthenticateRequest(r)
	if user == nil {
		http.Error(w, "not authenticated", http.StatusUnauthorized)
		return
	}
	if a.refuseIfReadOnly(w, r, user.ID) {
		return
	}
	// Hand the handler the session this guard resolved: one lookup, and the
	// handler acts for exactly the user checked here.
	next.ServeHTTP(w, auth.WithSessionUser(r, user))
}

// guardUnclassifiedWrite enforces read-only on a write route with no
// classification: a presented Authorization credential must resolve, and no
// resolved credential (bearer or session) may belong to a read-only account.
func (a *API) guardUnclassifiedWrite(w http.ResponseWriter, r *http.Request, next http.Handler) {
	var userIDs []string
	if r.Header.Get("Authorization") != "" {
		p, err := a.authenticatePrincipal(r)
		if err != nil || p == nil || p.User == nil {
			http.Error(w, "not authenticated", http.StatusUnauthorized)
			return
		}
		userIDs = append(userIDs, p.User.ID)
	}
	// A stale or unknown session cookie names no account; a handler that
	// needs the session rejects it itself with the same lookup.
	if a.userAuth != nil {
		if user := a.userAuth.AuthenticateRequest(r); user != nil {
			userIDs = append(userIDs, user.ID)
		}
	}
	for _, id := range userIDs {
		if a.refuseIfReadOnly(w, r, id) {
			return
		}
	}
	next.ServeHTTP(w, r)
}
