package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
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
	"github.com/tokencanopy/e2a/internal/logredact"
)

const (
	oidcStateCookieName    = "e2a_oidc_state"
	oidcNonceCookieName    = "e2a_oidc_nonce"
	oidcVerifierCookieName = "e2a_oidc_verifier"
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
			log.Printf("[auth] OIDC issuer discovered: %s", oa.cfg.IssuerURL)
			return
		}

		if ctx.Err() != nil {
			// Shutdown in progress -- stop retrying without logging the
			// (expected, ctx-cancelled) discovery error as a failure.
			return
		}

		if attempt == 1 || time.Since(lastLogged) >= oidcDiscoveryFailureLogGap {
			log.Printf("[auth] OIDC issuer discovery failed, retrying in background (attempt %d): %v", attempt, err)
			lastLogged = time.Now()
		}

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

// HandleLogin creates a browser-bound OIDC transaction and redirects to the
// provider's authorization endpoint. The PKCE verifier and OIDC nonce never
// appear in application logs or identity-bearing cookies. Until background
// issuer discovery has completed at least once, this fails closed with 503
// rather than attempting a partial or misconfigured flow.
func (oa *OIDCAuth) HandleLogin(w http.ResponseWriter, r *http.Request) {
	rs := oa.ready.Load()
	if rs == nil {
		http.Error(w, "login temporarily unavailable", http.StatusServiceUnavailable)
		return
	}

	state, err := randomOIDCValue()
	if err != nil {
		log.Printf("[auth] OIDC login initialization failed: %v", err)
		http.Error(w, "login unavailable", http.StatusInternalServerError)
		return
	}
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
func (oa *OIDCAuth) HandleCallback(w http.ResponseWriter, r *http.Request) {
	rs := oa.ready.Load()
	if rs == nil {
		http.Error(w, "login temporarily unavailable", http.StatusServiceUnavailable)
		return
	}

	stateCookie, stateErr := r.Cookie(oidcStateCookieName)
	nonceCookie, nonceErr := r.Cookie(oidcNonceCookieName)
	verifierCookie, verifierErr := r.Cookie(oidcVerifierCookieName)
	if stateErr != nil || nonceErr != nil || verifierErr != nil {
		oa.clearTransactionCookies(w)
		http.Error(w, "invalid login transaction", http.StatusBadRequest)
		return
	}

	requestState := r.URL.Query().Get("state")
	if requestState == "" || subtle.ConstantTimeCompare([]byte(requestState), []byte(stateCookie.Value)) != 1 {
		oa.clearTransactionCookies(w)
		http.Error(w, "invalid login transaction", http.StatusBadRequest)
		return
	}

	// Consume the browser transaction before any network or database work.
	// Authorization codes remain single-use at the provider as required by OIDC.
	oa.clearTransactionCookies(w)

	if r.URL.Query().Get("error") != "" {
		log.Printf("[auth] OIDC provider rejected authorization")
		http.Error(w, "login rejected", http.StatusBadRequest)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "invalid login response", http.StatusBadRequest)
		return
	}

	token, err := rs.oauthConfig.Exchange(r.Context(), code, oauth2.VerifierOption(verifierCookie.Value))
	if err != nil {
		// A *oauth2.RetrieveError formats the provider's FULL raw HTTP response
		// body into its Error() string — third-party text of unbounded size that
		// must not land in logs verbatim. Log the status + RFC 6749 error code
		// only, and truncate that too: ErrorCode is parsed straight out of the
		// provider's response body, so it is provider-controlled and unbounded
		// just like the Error() string in the sibling branch.
		var retrieveErr *oauth2.RetrieveError
		if errors.As(err, &retrieveErr) {
			status := 0
			if retrieveErr.Response != nil {
				status = retrieveErr.Response.StatusCode
			}
			log.Printf("[auth] OIDC code exchange failed: provider returned HTTP %d (oauth error=%q)", status, logredact.Truncate(retrieveErr.ErrorCode, 200))
		} else {
			log.Printf("[auth] OIDC code exchange failed: %s", logredact.Truncate(err.Error(), 200))
		}
		http.Error(w, "login verification failed", http.StatusUnauthorized)
		return
	}
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		log.Printf("[auth] OIDC token response omitted id_token")
		http.Error(w, "login verification failed", http.StatusUnauthorized)
		return
	}
	idToken, err := rs.verifier.Verify(r.Context(), rawIDToken)
	if err != nil {
		log.Printf("[auth] OIDC ID token verification failed: %v", err)
		http.Error(w, "login verification failed", http.StatusUnauthorized)
		return
	}
	if idToken.Nonce == "" || subtle.ConstantTimeCompare([]byte(idToken.Nonce), []byte(nonceCookie.Value)) != 1 {
		log.Printf("[auth] OIDC nonce verification failed")
		http.Error(w, "login verification failed", http.StatusUnauthorized)
		return
	}

	var claims map[string]any
	if err := idToken.Claims(&claims); err != nil {
		log.Printf("[auth] OIDC claims decoding failed: %v", err)
		http.Error(w, "login verification failed", http.StatusUnauthorized)
		return
	}
	rawUserID, ok := claims[oa.cfg.UserIDClaim].(string)
	userID := strings.TrimSpace(rawUserID)
	if !ok || userID == "" {
		log.Printf("[auth] OIDC ID token missing a valid user mapping claim")
		http.Error(w, "login verification failed", http.StatusUnauthorized)
		return
	}

	user, err := oa.store.GetUserByID(r.Context(), userID)
	if err != nil || user == nil {
		log.Printf("[auth] OIDC login referenced an unknown e2a user")
		http.Error(w, "user not found", http.StatusForbidden)
		return
	}
	sessionToken, err := oa.store.CreateUserSession(r.Context(), user.ID)
	if err != nil {
		log.Printf("[auth] OIDC session creation failed: %v", err)
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
	http.Redirect(w, r, oa.baseURL+"/dashboard", http.StatusFound)
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
}
