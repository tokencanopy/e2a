package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/tokencanopy/e2a/internal/config"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/telemetry"
)

const (
	oidcStateCookieName    = "e2a_oidc_state"
	oidcNonceCookieName    = "e2a_oidc_nonce"
	oidcVerifierCookieName = "e2a_oidc_verifier"
	oidcResumeCookieName   = "e2a_oidc_resume"
	oidcCookiePath         = "/api/auth/oidc"
	oidcTransactionMaxAge  = 10 * time.Minute

	// oidcDiscoveryInitialBackoff/oidcDiscoveryMaxBackoff govern the
	// background discovery retry loop started by NewOIDCAuth: the first
	// attempt happens immediately, then failures back off exponentially
	// from oidcDiscoveryInitialBackoff, doubling on every failed attempt,
	// capped at oidcDiscoveryMaxBackoff, forever (until ctx is cancelled).
	// Tests override these via WithOIDCDiscoveryBackoff so they don't block
	// on real wall-clock time.
	oidcDiscoveryInitialBackoff = time.Second
	oidcDiscoveryMaxBackoff     = 60 * time.Second
	oidcDiscoveryAttemptTimeout = 10 * time.Second

	// oidcDiscoveryFailureLogGap rate-limits repeated discovery-failure log
	// lines: the first failure always logs, subsequent failures log at most
	// once per this interval so a persistently-down issuer doesn't flood
	// the log.
	oidcDiscoveryFailureLogGap = time.Minute
)

// oidcReadyState holds everything a successful provider discovery produces:
// the OAuth2 client config (carrying the provider's real authorization/token
// endpoints) and the ID-token verifier bound to the provider's JWKS. It is
// published as a single atomic value once discovery succeeds so a concurrent
// HandleLogin/HandleCallback either sees "not ready yet" (nil) or a fully
// consistent, fully-built pair -- never a half-initialized one.
type oidcReadyState struct {
	oauthConfig *oauth2.Config
	verifier    *oidc.IDTokenVerifier
}

// OIDCAuth implements an optional OpenID Connect relying party for browser
// login. It accepts only Authorization Code responses initiated by HandleLogin,
// verifies the returned ID token, and maps a configured claim to an existing
// e2a users.id. It never provisions users.
type OIDCAuth struct {
	cfg     config.OIDCConfig
	store   *identity.Store
	secure  bool
	baseURL string

	// ready is nil until the background discovery goroutine (started in
	// NewOIDCAuth) successfully discovers the issuer, at which point it
	// holds the built *oidcReadyState for the remaining lifetime of oa.
	// HandleLogin/HandleCallback fail closed (503) while it is nil.
	ready atomic.Pointer[oidcReadyState]

	// discoveryBackoff/discoveryMaxBackoff seed the retry loop's backoff
	// schedule; discoveryDone, if set, is closed when the discovery
	// goroutine returns (success or ctx cancellation). Both are test-only
	// hooks set via functional options -- production callers get the
	// package defaults and no completion signal.
	discoveryBackoff    time.Duration
	discoveryMaxBackoff time.Duration
	discoveryTimeout    time.Duration
	discoveryDone       chan<- struct{}
	metrics             telemetry.Metrics
}

// OIDCOption configures optional, non-default behavior of NewOIDCAuth.
// Production callers don't need any; they exist for tests that need the
// background discovery retry loop to run on a compressed timescale, or to
// observe the loop's lifecycle without a sleep-based race.
type OIDCOption func(*OIDCAuth)

// WithOIDCDiscoveryBackoff overrides the discovery retry loop's initial and
// maximum backoff durations (production defaults: 1s initial, doubling,
// capped at 60s). Intended for tests exercising the retry path against a
// short-lived httptest server, where waiting out the real defaults would
// make the suite slow.
func WithOIDCDiscoveryBackoff(initial, maxBackoff time.Duration) OIDCOption {
	return func(oa *OIDCAuth) {
		oa.discoveryBackoff = initial
		oa.discoveryMaxBackoff = maxBackoff
	}
}

// WithOIDCDiscoveryAttemptTimeout overrides the maximum duration of one
// issuer discovery HTTP request (production default: 10s). Intended for tests
// that exercise a provider which accepts a connection but never responds.
func WithOIDCDiscoveryAttemptTimeout(timeout time.Duration) OIDCOption {
	return func(oa *OIDCAuth) {
		oa.discoveryTimeout = timeout
	}
}

// WithOIDCDiscoveryDone registers a channel that the background discovery
// goroutine closes when it returns, whether that's because discovery
// succeeded or because ctx was cancelled. It exists so tests can assert the
// goroutine actually exits (no leak) without guessing at a sleep duration.
func WithOIDCDiscoveryDone(done chan<- struct{}) OIDCOption {
	return func(oa *OIDCAuth) {
		oa.discoveryDone = done
	}
}

// WithOIDCMetrics wires the bounded observability backend used for provider
// discovery and callback outcomes. Nil leaves the default no-op backend.
func WithOIDCMetrics(metrics telemetry.Metrics) OIDCOption {
	return func(oa *OIDCAuth) {
		if metrics != nil {
			oa.metrics = metrics
		}
	}
}

// NewOIDCAuth returns nil without performing discovery when OIDC is disabled.
// Enabled configurations construct the handler synchronously -- this call
// never touches the network -- and start one background goroutine that
// discovers the issuer immediately, then retries with exponential backoff
// (capped, forever) until it succeeds or ctx is cancelled. Until discovery
// succeeds, HandleLogin and HandleCallback fail closed with 503. This keeps
// e2a's boot decoupled from the identity provider's availability (an
// unreachable issuer no longer prevents the whole process -- mail included
// -- from starting) while preserving fail-closed login behavior: there is no
// window where login silently no-ops or half-completes.
//
// Static/config-shaped problems are not this function's concern: they are
// caught by config.OIDCConfig validation before this is ever called. Only
// the network-dependent discovery call moved off the boot path.
func NewOIDCAuth(ctx context.Context, cfg config.OIDCConfig, store *identity.Store, production bool, baseURL string, opts ...OIDCOption) (*OIDCAuth, error) {
	if !cfg.Enabled {
		return nil, nil
	}

	oa := &OIDCAuth{
		cfg:                 cfg,
		store:               store,
		secure:              production,
		baseURL:             strings.TrimRight(baseURL, "/"),
		discoveryBackoff:    oidcDiscoveryInitialBackoff,
		discoveryMaxBackoff: oidcDiscoveryMaxBackoff,
		discoveryTimeout:    oidcDiscoveryAttemptTimeout,
		metrics:             telemetry.NoOp{},
	}
	for _, opt := range opts {
		opt(oa)
	}

	go oa.discoverWithRetry(ctx)

	return oa, nil
}

// discoverWithRetry runs for the lifetime of oa on its own goroutine. It
// attempts oidc.NewProvider immediately, then on failure retries with
// exponential backoff (starting at oa.discoveryBackoff, doubling each
// attempt, capped at oa.discoveryMaxBackoff) until it succeeds or ctx is
// cancelled. On success it builds the oauth2.Config and ID-token verifier
// exactly as the old synchronous constructor did and publishes them
// atomically via oa.ready, then returns -- there is nothing left to retry.
func (oa *OIDCAuth) discoverWithRetry(ctx context.Context) {
	if oa.discoveryDone != nil {
		defer close(oa.discoveryDone)
	}

	backoff := oa.discoveryBackoff
	var lastLogged time.Time
	attempt := 0

	for {
		attempt++

		attemptCtx, cancel := context.WithTimeout(ctx, oa.discoveryTimeout)
		provider, err := oidc.NewProvider(attemptCtx, oa.cfg.IssuerURL)
		cancel()
		if err == nil {
			oa.ready.Store(&oidcReadyState{
				oauthConfig: &oauth2.Config{
					ClientID:     oa.cfg.ClientID,
					ClientSecret: oa.cfg.ClientSecret,
					RedirectURL:  oa.cfg.RedirectURL,
					Endpoint:     provider.Endpoint(),
					Scopes:       []string{oidc.ScopeOpenID},
				},
				verifier: provider.Verifier(&oidc.Config{ClientID: oa.cfg.ClientID}),
			})
			oa.metrics.OIDCDiscovery("success", "2xx")
			log.Printf("[auth] OIDC issuer discovery category=success status_class=2xx")
			return
		}

		if ctx.Err() != nil {
			// Shutdown in progress -- stop retrying without logging the
			// (expected, ctx-cancelled) discovery error as a failure.
			return
		}

		category, statusClass := classifyOIDCDiscoveryFailure(err)
		if attempt == 1 || time.Since(lastLogged) >= oidcDiscoveryFailureLogGap {
			log.Printf("[auth] OIDC issuer discovery failed category=%s status_class=%s", category, statusClass)
			lastLogged = time.Now()
		}
		oa.metrics.OIDCDiscovery(category, statusClass)

		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}

		backoff *= 2
		if backoff > oa.discoveryMaxBackoff {
			backoff = oa.discoveryMaxBackoff
		}
	}
}

// classifyOIDCDiscoveryFailure converts a go-oidc error into a fixed category
// and status class without returning any provider-controlled text. go-oidc's
// non-200 error string includes the full discovery response body, so callers
// must never log err itself.
func classifyOIDCDiscoveryFailure(err error) (category, statusClass string) {
	var mismatch *oidc.IssuerMismatchError
	if errors.As(err, &mismatch) || strings.HasPrefix(err.Error(), "oidc: failed to decode provider discovery object:") {
		return "discovery_invalid", "2xx"
	}

	message := err.Error()
	if len(message) >= 4 && message[3] == ' ' &&
		message[0] >= '0' && message[0] <= '9' &&
		message[1] >= '0' && message[1] <= '9' &&
		message[2] >= '0' && message[2] <= '9' {
		status := int(message[0]-'0')*100 + int(message[1]-'0')*10 + int(message[2]-'0')
		statusClass = oidcStatusClass(status)
		if status == http.StatusTooManyRequests || status >= http.StatusInternalServerError {
			return "issuer_unavailable", statusClass
		}
		return "discovery_invalid", statusClass
	}

	return "issuer_unavailable", "none"
}

func oidcStatusClass(status int) string {
	switch status / 100 {
	case 1:
		return "1xx"
	case 2:
		return "2xx"
	case 3:
		return "3xx"
	case 4:
		return "4xx"
	case 5:
		return "5xx"
	default:
		return "none"
	}
}

func (oa *OIDCAuth) recordCallback(outcome, trust string, status int, logFailure bool) {
	statusClass := oidcStatusClass(status)
	oa.metrics.OIDCCallback(outcome, trust, statusClass)
	if logFailure {
		log.Printf("[auth] OIDC callback outcome category=%s trust=%s status_class=%s", outcome, trust, statusClass)
	}
}

// oidcResume carries the optional post-login instructions HandleLogin
// accepts from the query string through the provider round trip. It mirrors
// the legacy Google door's OAuthState fields: ReturnTo is a same-origin
// server path the user is bounced to after callback success, and the
// CLICallback/CLIState pair drives the loopback CLI handoff. All values are
// validated at HandleLogin time with the same validators the legacy door
// uses (validateReturnToPath / validateCLICallbackURL).
//
// Unlike the legacy door, which folds these into the OAuth state parameter,
// the OIDC door keeps its provider-facing state parameter opaque and carries
// the instructions in a fourth transaction cookie instead. TransactionState
// binds that cookie to the same unpredictable state value as the other OIDC
// transaction cookies. HandleCallback ignores instructions whose binding or
// CLI callback/state pairing does not validate.
type oidcResume struct {
	TransactionState string `json:"s,omitempty"`
	ReturnTo         string `json:"rt,omitempty"`
	CLICallback      string `json:"cb,omitempty"`
	CLIState         string `json:"cs,omitempty"`
}

func (r *oidcResume) empty() bool {
	return r.ReturnTo == "" && r.CLICallback == "" && r.CLIState == ""
}

func (r *oidcResume) encode() string {
	b, _ := json.Marshal(r)
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeOIDCResume returns nil for corrupt instructions, instructions from a
// different OIDC transaction, or an incomplete CLI callback/state pair. Those
// cases degrade to "no post-login instructions" rather than failing an
// otherwise valid login.
func decodeOIDCResume(raw, expectedState string) *oidcResume {
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil
	}
	var r oidcResume
	if err := json.Unmarshal(b, &r); err != nil {
		return nil
	}
	if r.TransactionState == "" ||
		subtle.ConstantTimeCompare([]byte(r.TransactionState), []byte(expectedState)) != 1 {
		return nil
	}
	if (r.CLICallback == "") != (r.CLIState == "") {
		return nil
	}
	return &r
}

// oidcResumeFromQuery validates the optional post-login instructions on a
// login request, applying the exact rules and error semantics of the legacy
// Google door: cli_callback and cli_state must be provided together,
// cli_callback must be a loopback http URL, and return_to must survive the
// /oauth2/ allow-list. Any violation fails the login with 400 before any
// transaction state is created.
func oidcResumeFromQuery(r *http.Request) (*oidcResume, error) {
	cliCallback := r.URL.Query().Get("cli_callback")
	cliState := r.URL.Query().Get("cli_state")
	if (cliCallback == "") != (cliState == "") {
		return nil, errors.New("cli_callback and cli_state must be provided together")
	}

	resume := &oidcResume{}
	if cliCallback != "" {
		callbackURL, err := validateCLICallbackURL(cliCallback)
		if err != nil {
			return nil, err
		}
		resume.CLICallback = callbackURL.String()
		resume.CLIState = cliState
	}

	if returnTo := r.URL.Query().Get("return_to"); returnTo != "" {
		if err := validateReturnToPath(returnTo); err != nil {
			return nil, err
		}
		resume.ReturnTo = returnTo
	}
	return resume, nil
}

// HandleLogin creates a browser-bound OIDC transaction and redirects to the
// provider's authorization endpoint. The PKCE verifier and OIDC nonce never
// appear in application logs or identity-bearing cookies. Until background
// issuer discovery has completed at least once, this fails closed with 503
// rather than attempting a partial or misconfigured flow.
//
// Like the legacy Google door, it accepts the optional query parameters
// return_to (an allow-listed same-origin path to resume after login) and the
// cli_callback/cli_state pair (a loopback handoff for terminal login).
// Invalid values are rejected with 400 before any cookie is set; valid ones
// ride the round trip in the resume transaction cookie.
func (oa *OIDCAuth) HandleLogin(w http.ResponseWriter, r *http.Request) {
	rs := oa.ready.Load()
	if rs == nil {
		http.Error(w, "login temporarily unavailable", http.StatusServiceUnavailable)
		return
	}

	resume, err := oidcResumeFromQuery(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	state, err := randomOIDCValue()
	if err != nil {
		log.Printf("[auth] OIDC login initialization failed: %v", err)
		http.Error(w, "login unavailable", http.StatusInternalServerError)
		return
	}
	state = oa.bindOIDCState(state)
	nonce, err := randomOIDCValue()
	if err != nil {
		log.Printf("[auth] OIDC login initialization failed: %v", err)
		http.Error(w, "login unavailable", http.StatusInternalServerError)
		return
	}
	verifier := oauth2.GenerateVerifier()

	oa.setTransactionCookie(w, oidcStateCookieName, state, int(oidcTransactionMaxAge.Seconds()))
	oa.setTransactionCookie(w, oidcNonceCookieName, nonce, int(oidcTransactionMaxAge.Seconds()))
	oa.setTransactionCookie(w, oidcVerifierCookieName, verifier, int(oidcTransactionMaxAge.Seconds()))
	// The resume cookie is refreshed on every login — set when the caller
	// asked for a post-login action, cleared otherwise — so instructions
	// from an earlier, abandoned transaction can't leak into this one.
	if resume.empty() {
		oa.setTransactionCookie(w, oidcResumeCookieName, "", -1)
	} else {
		resume.TransactionState = state
		oa.setTransactionCookie(w, oidcResumeCookieName, resume.encode(), int(oidcTransactionMaxAge.Seconds()))
	}

	location := rs.oauthConfig.AuthCodeURL(
		state,
		oidc.Nonce(nonce),
		oauth2.S256ChallengeOption(verifier),
	)
	http.Redirect(w, r, location, http.StatusFound)
}

// HandleCallback validates the browser transaction, exchanges the short-lived
// authorization code over the back channel, verifies the ID token, and creates
// the same e2a session used by the legacy Google flow. Until background
// issuer discovery has completed at least once, this fails closed with 503.
//
// Post-login behavior mirrors the legacy door: a CLI-initiated login renders
// the loopback handoff page, a validated return_to bounces the user to that
// path, and anything else lands on /dashboard.
func (oa *OIDCAuth) HandleCallback(w http.ResponseWriter, r *http.Request) {
	rs := oa.ready.Load()
	if rs == nil {
		oa.recordCallback("discovery_unavailable", "public", http.StatusServiceUnavailable, false)
		http.Error(w, "login temporarily unavailable", http.StatusServiceUnavailable)
		return
	}

	stateCookie, stateErr := r.Cookie(oidcStateCookieName)
	nonceCookie, nonceErr := r.Cookie(oidcNonceCookieName)
	verifierCookie, verifierErr := r.Cookie(oidcVerifierCookieName)
	if stateErr != nil || nonceErr != nil || verifierErr != nil {
		oa.clearTransactionCookies(w)
		oa.recordCallback("state_invalid", "public", http.StatusBadRequest, false)
		http.Error(w, "invalid login transaction", http.StatusBadRequest)
		return
	}

	// The resume cookie is optional and read before the transaction cookies
	// are consumed below; an undecodable value is treated as absent (see
	// decodeOIDCResume).
	resume := &oidcResume{}
	if cookie, err := r.Cookie(oidcResumeCookieName); err == nil {
		if decoded := decodeOIDCResume(cookie.Value, stateCookie.Value); decoded != nil {
			resume = decoded
		}
	}

	requestState := r.URL.Query().Get("state")
	if !oa.validOIDCState(stateCookie.Value) || requestState == "" || subtle.ConstantTimeCompare([]byte(requestState), []byte(stateCookie.Value)) != 1 {
		oa.clearTransactionCookies(w)
		oa.recordCallback("state_invalid", "public", http.StatusBadRequest, false)
		http.Error(w, "invalid login transaction", http.StatusBadRequest)
		return
	}

	// Consume the browser transaction before any network or database work.
	// Authorization codes remain single-use at the provider as required by OIDC.
	oa.clearTransactionCookies(w)

	if r.URL.Query().Get("error") != "" {
		oa.recordCallback("provider_rejected", "trusted", http.StatusBadRequest, true)
		http.Error(w, "login rejected", http.StatusBadRequest)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		oa.recordCallback("response_invalid", "trusted", http.StatusBadRequest, true)
		http.Error(w, "invalid login response", http.StatusBadRequest)
		return
	}

	token, err := rs.oauthConfig.Exchange(r.Context(), code, oauth2.VerifierOption(verifierCookie.Value))
	if err != nil {
		// oauth2.RetrieveError contains the provider's full response body and
		// parsed OAuth error code. Neither is safe to log; the bounded outcome
		// is sufficient for alerting and diagnosis.
		oa.recordCallback("token_exchange_failed", "trusted", http.StatusUnauthorized, true)
		http.Error(w, "login verification failed", http.StatusUnauthorized)
		return
	}
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		oa.recordCallback("id_token_invalid", "trusted", http.StatusUnauthorized, true)
		http.Error(w, "login verification failed", http.StatusUnauthorized)
		return
	}
	idToken, err := rs.verifier.Verify(r.Context(), rawIDToken)
	if err != nil {
		oa.recordCallback("id_token_invalid", "trusted", http.StatusUnauthorized, true)
		http.Error(w, "login verification failed", http.StatusUnauthorized)
		return
	}
	if idToken.Nonce == "" || subtle.ConstantTimeCompare([]byte(idToken.Nonce), []byte(nonceCookie.Value)) != 1 {
		oa.recordCallback("id_token_invalid", "trusted", http.StatusUnauthorized, true)
		http.Error(w, "login verification failed", http.StatusUnauthorized)
		return
	}

	var claims map[string]any
	if err := idToken.Claims(&claims); err != nil {
		oa.recordCallback("claim_invalid", "trusted", http.StatusUnauthorized, true)
		http.Error(w, "login verification failed", http.StatusUnauthorized)
		return
	}
	rawUserID, ok := claims[oa.cfg.UserIDClaim].(string)
	userID := strings.TrimSpace(rawUserID)
	if !ok || userID == "" {
		oa.recordCallback("claim_invalid", "trusted", http.StatusUnauthorized, true)
		http.Error(w, "login verification failed", http.StatusUnauthorized)
		return
	}

	user, err := oa.store.GetUserByID(r.Context(), userID)
	if err != nil || user == nil {
		oa.recordCallback("unknown_user", "trusted", http.StatusForbidden, true)
		http.Error(w, "user not found", http.StatusForbidden)
		return
	}
	sessionToken, err := oa.store.CreateUserSession(r.Context(), user.ID)
	if err != nil {
		oa.recordCallback("session_failed", "trusted", http.StatusInternalServerError, true)
		http.Error(w, "failed to create session", http.StatusInternalServerError)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    sessionToken,
		Path:     "/",
		HttpOnly: true,
		Secure:   oa.secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(SessionMaxAge.Seconds()),
	})

	// CLI-initiated login: mint a key and hand it back to the waiting
	// terminal, exactly as the legacy Google door does. The callback URL is
	// re-validated even though HandleLogin already validated it — cheap
	// defense in depth against a tampered resume cookie.
	if resume.CLICallback != "" {
		callbackURL, err := validateCLICallbackURL(resume.CLICallback)
		if err != nil {
			oa.recordCallback("post_login_failed", "trusted", http.StatusBadRequest, true)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		handoff := &cliLoginHandoff{
			CallbackURL: callbackURL.String(),
			State:       resume.CLIState,
		}
		if err := writeCLIHandoffPage(oa.store, w, r, user, handoff); err != nil {
			oa.recordCallback("post_login_failed", "trusted", http.StatusInternalServerError, true)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		oa.recordCallback("success", "trusted", http.StatusOK, false)
		return
	}

	// return_to bounce: validated at HandleLogin time; re-validate
	// defensively here and fall through to /dashboard when it no longer
	// passes — the legacy door's exact fallback.
	if resume.ReturnTo != "" {
		if err := validateReturnToPath(resume.ReturnTo); err == nil {
			oa.recordCallback("success", "trusted", http.StatusFound, false)
			http.Redirect(w, r, oa.baseURL+resume.ReturnTo, http.StatusFound)
			return
		}
	}

	oa.recordCallback("success", "trusted", http.StatusFound, false)
	http.Redirect(w, r, oa.baseURL+"/dashboard", http.StatusFound)
}

// bindOIDCState authenticates the otherwise-opaque state value with the OIDC
// client secret. The provider echoes this exact value and the browser stores it
// in an HttpOnly cookie. Verification on callback means a scanner cannot mint
// a "trusted" outcome merely by choosing matching query and cookie strings.
func (oa *OIDCAuth) bindOIDCState(state string) string {
	mac := hmac.New(sha256.New, []byte(oa.cfg.ClientSecret))
	_, _ = mac.Write([]byte("e2a-oidc-state-v1\x00"))
	_, _ = mac.Write([]byte(state))
	return state + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (oa *OIDCAuth) validOIDCState(bound string) bool {
	state, encodedMAC, ok := strings.Cut(bound, ".")
	if !ok || state == "" || encodedMAC == "" {
		return false
	}
	providedMAC, err := base64.RawURLEncoding.DecodeString(encodedMAC)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(oa.cfg.ClientSecret))
	_, _ = mac.Write([]byte("e2a-oidc-state-v1\x00"))
	_, _ = mac.Write([]byte(state))
	return hmac.Equal(providedMAC, mac.Sum(nil))
}

func randomOIDCValue() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate secure random value: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func (oa *OIDCAuth) setTransactionCookie(w http.ResponseWriter, name, value string, maxAge int) {
	cookie := &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     oidcCookiePath,
		HttpOnly: true,
		Secure:   oa.secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   maxAge,
	}
	if maxAge > 0 {
		cookie.Expires = time.Now().Add(time.Duration(maxAge) * time.Second)
	} else {
		cookie.Expires = time.Unix(1, 0)
	}
	http.SetCookie(w, cookie)
}

func (oa *OIDCAuth) clearTransactionCookies(w http.ResponseWriter) {
	oa.setTransactionCookie(w, oidcStateCookieName, "", -1)
	oa.setTransactionCookie(w, oidcNonceCookieName, "", -1)
	oa.setTransactionCookie(w, oidcVerifierCookieName, "", -1)
	oa.setTransactionCookie(w, oidcResumeCookieName, "", -1)
}
