package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	jose "github.com/go-jose/go-jose/v3"
	"github.com/gorilla/mux"
	"github.com/jackc/pgx/v5"
	"github.com/ory/fosite"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/limits"
	"github.com/tokencanopy/e2a/internal/oauth"
	"golang.org/x/text/language"
)

// handleOAuthToken is the /oauth2/token endpoint. Thin wrapper
// over fosite's NewAccessRequest → NewAccessResponse → WriteAccess-
// Response chain. Everything interesting (grant_type dispatch, PKCE
// verification, refresh rotation with reuse defense, RFC 6749 §5.1
// no-store headers, error shape) lives in fosite; our job here is to
// adapt HTTP ↔ fosite and to inject the session type fosite hydrates
// into.
//
// 404s when the OAuth provider isn't wired (operator opted out via
// not calling SetOAuthProvider). Matches the discovery / DCR / etc.
// 404-when-not-configured pattern from the hand-rolled branch.
func (a *API) handleOAuthToken(w http.ResponseWriter, r *http.Request) {
	// Agent-identity grant (Slice 5b-2): dispatch grant_type=jwt-bearer here
	// before fosite, which doesn't implement it. ParseForm is idempotent —
	// fosite re-reads the same cached r.PostForm — so peeking at grant_type
	// first is safe. This path works even when fosite OAuth isn't wired.
	if err := r.ParseForm(); err == nil && r.PostForm.Get("grant_type") == jwtBearerGrantType {
		a.handleJWTBearerGrant(w, r)
		return
	}
	if a.oauthProvider == nil {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	// Resolve the request id up front so error log lines (and the
	// X-Request-Id header when no middleware ran) can carry it.
	reqID := oauthRequestID(w, r)

	// Hand fosite a fresh session pointer. NewAccessRequest will
	// populate it from the stored auth-code / refresh-token row;
	// the populated session ends up on the response too (e.g. for
	// JWT access tokens — not used here but harmless).
	session := &oauth.Session{}

	accessReq, err := a.oauthProvider.NewAccessRequest(ctx, r, session)
	if err != nil {
		logTokenError(accessReq, "new_access_request", reqID, err)
		// fosite writes the canonical RFC 6749 §5.2 JSON error body
		// here: {"error":"invalid_grant",...} with correct status
		// code and Cache-Control: no-store.
		a.oauthProvider.WriteAccessError(ctx, w, accessReq, err)
		return
	}

	accessResp, err := a.oauthProvider.NewAccessResponse(ctx, accessReq)
	if err != nil {
		logTokenError(accessReq, "new_access_response", reqID, err)
		a.oauthProvider.WriteAccessError(ctx, w, accessReq, err)
		return
	}

	a.oauthProvider.WriteAccessResponse(ctx, w, accessReq, accessResp)
}

// ───────────────────────── DCR (RFC 7591) ─────────────────────────

// OAuthRegisterRequest is the RFC 7591 §2 client metadata POSTed to
// /oauth2/register. Unknown fields are tolerated (forward-compat
// with RFC 7591 extensions) but ignored.
type OAuthRegisterRequest struct {
	ClientName              string   `json:"client_name"`
	RedirectURIs            []string `json:"redirect_uris"`
	GrantTypes              []string `json:"grant_types,omitempty"`
	ResponseTypes           []string `json:"response_types,omitempty"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method,omitempty"`
	Scope                   string   `json:"scope,omitempty"`
}

// OAuthRegisterResponse is the RFC 7591 §3.2.1 success envelope.
// Echoes the metadata the server stored (after defaults applied) plus
// the assigned client_id and issuance timestamp. No client_secret —
// public clients only.
type OAuthRegisterResponse struct {
	ClientID                string   `json:"client_id"`
	ClientIDIssuedAt        int64    `json:"client_id_issued_at"`
	ClientName              string   `json:"client_name"`
	RedirectURIs            []string `json:"redirect_uris"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	Scope                   string   `json:"scope"`
}

// OAuthError is the RFC 7591 §3.2.2 / RFC 6749 §5.2 JSON error body.
// Used for DCR-side errors and direct (non-redirected) authorize errors.
// request_id is an extension member (both RFCs permit additional fields
// in the error response) so a client can quote the id from the body when
// reporting a failure; it always matches the X-Request-Id response header.
type OAuthError struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description,omitempty"`
	RequestID        string `json:"request_id,omitempty"`
}

// validRequestID bounds the ids this package will reflect into [oauth] log
// lines and error bodies. Client-supplied X-Request-Id values outside this
// alphabet/length (field-spoofing text, multi-KB padding) are replaced with
// a minted id rather than logged verbatim. Same rule as the MCP transport's
// INBOUND_REQUEST_ID in mcp/src/correlation.ts.
var validRequestID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// oauthRequestID returns the request id used to correlate error bodies and
// [oauth] log lines with the X-Request-Id response header. The httpapi root
// middleware sets X-Request-Id on the response writer before dispatching
// (it wraps the legacy mux too), so downstream handlers can read back the
// exact id the client will see. When no middleware ran (bare-mux test
// servers), honor a client-supplied header, else mint one and set it on the
// response — header, body, and logs stay consistent either way. Any id that
// fails validRequestID (the middleware echoes client input unchecked) is
// replaced with a minted one, overwriting the response header so header,
// body, and logs still agree. Returns "" only if crypto/rand fails; callers
// degrade gracefully (omit the id).
func oauthRequestID(w http.ResponseWriter, r *http.Request) string {
	if id := w.Header().Get("X-Request-Id"); validRequestID.MatchString(id) {
		return id
	}
	if id := r.Header.Get("X-Request-Id"); validRequestID.MatchString(id) {
		w.Header().Set("X-Request-Id", id)
		return id
	}
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	id := "req_" + hex.EncodeToString(b)
	w.Header().Set("X-Request-Id", id)
	return id
}

func writeOAuthError(w http.ResponseWriter, r *http.Request, status int, code, desc string) {
	// Resolve the id before WriteHeader: oauthRequestID may have to set
	// X-Request-Id on the response (no-middleware case), which only works
	// while headers are still mutable.
	reqID := oauthRequestID(w, r)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(OAuthError{
		Error:            code,
		ErrorDescription: desc,
		RequestID:        reqID,
	})
}

// dcrSourceIP returns the per-IP key for DCR rate-limiting. DCR is
// the only anonymous persistent-write endpoint; we can't trust
// X-Forwarded-For because an attacker can rotate it to bypass the
// limit. CF-Connecting-IP is set by Cloudflare and stripped from
// inbound requests at the edge, so it's spoofable only by someone
// who can reach the origin directly — operators should firewall
// the origin to CF anyway.
//
// Falls back to RemoteAddr if CF-Connecting-IP is empty (dev /
// non-CF deployments). Doesn't fall back to XFF.
func dcrSourceIP(r *http.Request) string {
	if ip := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); ip != "" {
		return ip
	}
	// net.SplitHostPort handles bracketed IPv6 ("[::1]:443"), bare IPv4
	// with port, and reports an error when no port is present (rare in
	// stdlib http but possible behind some proxies / in tests). On
	// error fall back to the raw RemoteAddr so we still bucket on
	// something stable instead of collapsing all such requests onto
	// the empty string.
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// validateRedirectURI enforces e2a's redirect_uri allow-list, mirroring
// the rules fosite applies at /authorize time so DCR rejects invalid
// URIs at registration rather than at first-use.
//
// Allowed:
//   - https://… (web apps; must have host)
//   - http://localhost[:port]/…, http://127.0.0.1[:port]/…,
//     http://[::1][:port]/… (loopback for native apps; "localhost" is
//     accepted because every mainstream native MCP client — Claude
//     Code included — uses it for its callback. fosite's RFC 8252 §7.3
//     port-rewrite path only matches the IP-literal form because it
//     uses net.ParseIP(hostname).IsLoopback(); the practical
//     consequence is that clients registering with "localhost" must
//     re-use the same port at /authorize time. Clients that need
//     ephemeral-port-per-session should register with 127.0.0.1
//     instead. DCR-driven clients (the common case) re-register every
//     session so this isn't a real limitation in practice.)
//   - reverse-domain custom schemes per RFC 8252 §7.1 (com.example.app:/cb)
//
// Rejected:
//   - http:// to anything non-loopback (codes would leak in transit)
//   - URIs with fragments (RFC 6749 §3.1.2)
//   - URIs with userinfo (https://anyone@evil.com/cb — looks legit
//     to a human but the authority is attacker-controlled)
//   - URIs missing scheme/authority for http(s)
//   - dangerous schemes (javascript:, data:, file:, vbscript:, blob:,
//     about:) — embedded webviews and some MCP host clients honor a
//     non-HTTP(S) Location header, which would deliver the auth code
//     into attacker-controlled JS/HTML via http.Redirect
//   - single-label custom schemes (myapp:) — RFC 7595 §3.8 reserves
//     these for future IANA registration, and they bypass the OS-level
//     scheme registry collision protections RFC 8252 §7.1 relies on
//
// isLoopbackHost reports whether a redirect_uri hostname is a loopback
// target: the literal "localhost" or any IP that net considers loopback
// (127.0.0.0/8, ::1). Used both by validateRedirectURI (which loopback http
// hosts are allowed) and by the account-scope gate (account may only be
// granted to a native app that receives its callback on loopback).
//
// Deliberately NARROWER than fosite's IsLocalhost: it does NOT accept the
// ".localhost" suffix (e.g. app.localhost). The cost is only that account
// scope is unavailable to a tool using such a redirect (fail-closed, a
// conservative UX edge); it is never a security gap. Keep it strict — a
// suffix rule is where loopback look-alike bypasses tend to creep in.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// isLoopbackRedirect reports whether a full redirect_uri is an http loopback
// URL (http://localhost[:port]/…, http://127.0.0.1[:port]/…, http://[::1]…).
// https URLs return false even though they're valid redirect targets — the
// point of this gate is "the callback lands on the user's own machine", which
// only http-loopback guarantees. This is the structural control that lets the
// consent screen offer account scope to local tools (Claude Code, Cursor) while
// a hosted/remote client — which can't receive a localhost callback — can't.
func isLoopbackRedirect(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" {
		return false
	}
	return isLoopbackHost(u.Hostname())
}

func isDangerousOAuthRedirectScheme(scheme string) bool {
	switch strings.ToLower(scheme) {
	case "javascript", "data", "file", "vbscript", "blob", "about":
		return true
	default:
		return false
	}
}

// accountEligibleRedirect reports whether a redirect_uri may be granted
// account (workspace-admin) scope.
//
// SECURITY DECISION (2026-07-10): account was historically loopback-ONLY —
// a native tool whose callback lands on the user's own machine — so a
// hosted/remote client could never be socially-engineered into a
// workspace-admin grant. That gate is intentionally RELAXED here to also
// permit any https redirect, so hosted connectors (Claude Chat/Cowork,
// redirect https://claude.ai/api/mcp/auth_callback) can be granted account.
//
// The consequence, accepted deliberately: ANY https MCP connector a user
// consents to can obtain full e2a workspace admin (create/delete inboxes,
// manage domains & webhooks, mint API keys, resolve reviews). fosite
// exact-matches redirect_uris so an auth code still can't be diverted to an
// attacker, but the admin token is then held by whatever hosted client the
// user approved. Consent remains mandatory and grants nothing on its own.
func accountEligibleRedirect(raw string) bool {
	// Self-contained fail-closed: never treat a redirect as account-eligible
	// unless it also passes full redirect_uri validation (rejects userinfo,
	// empty host, fragments, dangerous schemes). Every caller today
	// pre-validates, but this keeps the gate correct even if some future
	// writer to oauth_clients skips validateRedirectURI.
	if validateRedirectURI(raw) != nil {
		return false
	}
	if isLoopbackRedirect(raw) {
		return true
	}
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https"
}

func validateRedirectURI(raw string) error {
	if raw == "" {
		return errors.New("redirect_uri cannot be empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("redirect_uri is not a valid URI")
	}
	if u.Fragment != "" {
		return errors.New("redirect_uri must not contain a fragment")
	}
	if u.User != nil {
		return errors.New("redirect_uri must not contain userinfo")
	}
	switch u.Scheme {
	case "":
		return errors.New("redirect_uri must include a scheme")
	case "https":
		if u.Host == "" {
			return errors.New("https redirect_uri must include a host")
		}
		return nil
	case "http":
		if isLoopbackHost(u.Hostname()) {
			return nil
		}
		return errors.New("http redirect_uri must use a loopback host (localhost, 127.0.0.1, or ::1)")
	default:
		// Block well-known dangerous schemes outright. Even though
		// fosite's exact-match at /authorize time would normally
		// prevent these from being reached, http.Redirect writes the
		// Location header verbatim regardless of scheme — see
		// writeAuthorizeRedirect for the matching defense-in-depth check.
		if isDangerousOAuthRedirectScheme(u.Scheme) {
			return errors.New("redirect_uri scheme not permitted")
		}
		// RFC 8252 §7.1: private-use URI schemes MUST be in reverse-
		// domain notation; RFC 7595 §3.8 reserves single-label schemes
		// for IANA. Require a dot to enforce that.
		if !strings.Contains(u.Scheme, ".") {
			return errors.New("custom scheme must use reverse-domain notation (RFC 8252 §7.1)")
		}
		// Require an actual destination — neither bare scheme nor a
		// stray opaque-only ("myapp:") qualifies.
		if u.Host == "" && u.Path == "" && u.Opaque == "" {
			return errors.New("redirect_uri must include a path or authority")
		}
		return nil
	}
}

// handleOAuthRegister is the RFC 7591 Dynamic Client Registration
// endpoint. Anonymous ("open" registration per §2); rate-limited per
// real IP via dcrSourceIP so an attacker can't fill oauth_clients.
//
// Public clients only (token_endpoint_auth_method must be "none" or
// omitted). The schema's CHECK constraint enforces this at the DB
// level as a second line of defense.
//
// 404 when OAuth isn't configured. 429 when over the per-IP cap.
// 400 invalid_client_metadata / invalid_redirect_uri for bad input.
// 201 on success with the full registered metadata.
func (a *API) handleOAuthRegister(w http.ResponseWriter, r *http.Request) {
	// Gate on BOTH provider and storage. SetOAuthProvider wires the
	// flow surface (authorize/token/revoke); SetOAuthStorage wires the
	// DB-backed adapter we INSERT into here. Registering a client when
	// the provider isn't wired would persist rows that the rest of the
	// surface can't redeem — better to 404 than create dead state.
	if a.oauthProvider == nil || a.oauthStorage == nil {
		http.NotFound(w, r)
		return
	}
	if ok, retryAfter := a.dcrLimit.AllowWithRetryAfter(dcrSourceIP(r)); !ok {
		secs := int(retryAfter.Round(time.Second).Seconds())
		if secs < 1 {
			secs = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		writeOAuthError(w, r, http.StatusTooManyRequests, "rate_limited",
			"too many registrations from this IP; try again later")
		return
	}

	var req OAuthRegisterRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&req); err != nil {
		writeOAuthError(w, r, http.StatusBadRequest, "invalid_client_metadata", "request body must be JSON")
		return
	}

	// client_name: required, length-bound.
	if strings.TrimSpace(req.ClientName) == "" {
		writeOAuthError(w, r, http.StatusBadRequest, "invalid_client_metadata", "client_name is required")
		return
	}
	if len(req.ClientName) > 200 {
		writeOAuthError(w, r, http.StatusBadRequest, "invalid_client_metadata",
			"client_name must be 200 characters or fewer")
		return
	}

	// redirect_uris: required, ≤10, each one a valid loopback/https/
	// custom URI, deduped to prevent a hostile DCR caller from
	// padding the soft cap with identical URLs.
	if len(req.RedirectURIs) == 0 {
		writeOAuthError(w, r, http.StatusBadRequest, "invalid_redirect_uri",
			"at least one redirect_uri is required")
		return
	}
	if len(req.RedirectURIs) > 10 {
		writeOAuthError(w, r, http.StatusBadRequest, "invalid_redirect_uri",
			"too many redirect_uris (max 10)")
		return
	}
	seen := make(map[string]struct{}, len(req.RedirectURIs))
	deduped := req.RedirectURIs[:0]
	for _, raw := range req.RedirectURIs {
		if err := validateRedirectURI(raw); err != nil {
			writeOAuthError(w, r, http.StatusBadRequest, "invalid_redirect_uri", err.Error())
			return
		}
		if _, ok := seen[raw]; ok {
			continue
		}
		seen[raw] = struct{}{}
		deduped = append(deduped, raw)
	}
	req.RedirectURIs = deduped

	// Defaults — fill in unspecified fields per RFC 7591 §2.
	if len(req.GrantTypes) == 0 {
		req.GrantTypes = []string{"authorization_code", "refresh_token"}
	}
	if len(req.ResponseTypes) == 0 {
		req.ResponseTypes = []string{"code"}
	}
	if req.TokenEndpointAuthMethod == "" {
		req.TokenEndpointAuthMethod = "none"
	}

	// Capability enforcement — reject anything outside what we
	// support. Failing here gives a clear error at registration time
	// instead of a confusing one at /token.
	for _, gt := range req.GrantTypes {
		if gt != "authorization_code" && gt != "refresh_token" {
			writeOAuthError(w, r, http.StatusBadRequest, "invalid_client_metadata",
				"unsupported grant_type: "+gt)
			return
		}
	}
	for _, rt := range req.ResponseTypes {
		if rt != "code" {
			writeOAuthError(w, r, http.StatusBadRequest, "invalid_client_metadata",
				"unsupported response_type: "+rt)
			return
		}
	}
	if req.TokenEndpointAuthMethod != "none" {
		writeOAuthError(w, r, http.StatusBadRequest, "invalid_client_metadata",
			`only token_endpoint_auth_method="none" (public clients with PKCE) is supported`)
		return
	}
	// Scope ceiling (Slice 5b / design §5 + scope-picker). A public DCR
	// client's REGISTERED scopes are the maximum a user can later approve on
	// the consent screen — not what the client gets autonomously (consent is
	// mandatory and grants nothing on its own). "account" (workspace admin) is
	// eligible only when the client's redirect_uris are ALL account-eligible
	// (loopback or https — see accountEligibleRedirect). fosite's
	// ExactScopeStrategy enforces granted ⊆ registered, so account must be on
	// the client row for the consent screen to grant it.
	redirectAllowsAccount := true
	for _, ru := range req.RedirectURIs {
		if !accountEligibleRedirect(ru) {
			redirectAllowsAccount = false
			break
		}
	}
	// Narrow, don't reject (RFC 7591 §3.2.1). A spec-compliant MCP client
	// requests every scope we advertise in `scopes_supported` (agent AND
	// account) whenever the 401 challenge carries no `scope` param — so
	// rejecting a superset would 400 every such client (this was the
	// "couldn't register with e2a's sign-in service" failure). Instead the
	// registered ceiling is the requested scopes ∩ what the redirect allows,
	// with unknown/ineligible scopes dropped and agent as the floor. A client
	// that OMITS scope gets the full eligible ceiling (Claude's case); a
	// client that explicitly requests only agent is honored at agent — we
	// never widen a caller's own least-privilege request. The result is
	// echoed back via the response's `scope`.
	wantAccount := redirectAllowsAccount
	if requested := strings.Fields(req.Scope); len(requested) > 0 {
		wantAccount = false
		for _, sc := range requested {
			if sc == identity.ScopeAccount && redirectAllowsAccount {
				wantAccount = true
			}
		}
	}
	scopeCeiling := []string{identity.ScopeAgent}
	if wantAccount {
		scopeCeiling = append(scopeCeiling, identity.ScopeAccount)
	}

	// Generate the client_id and persist. The DB CHECK constraints
	// validate (public, auth_method) and (public, secret_hash)
	// alignment as the second line of defense — if a future code
	// path tried to insert a confidential client without a secret,
	// the INSERT would fail at the DB level rather than persist
	// garbage.
	clientID := generateClientID()
	if _, err := a.oauthStorage.Pool().Exec(r.Context(), `
		INSERT INTO oauth_clients
		    (client_id, client_name, redirect_uris, grant_types,
		     response_types, scopes, audiences, token_endpoint_auth_method,
		     public, created_via)
		VALUES ($1, $2, $3, $4, $5, $6, ARRAY[]::TEXT[], $7, TRUE, 'dcr')
	`, clientID, req.ClientName, req.RedirectURIs, req.GrantTypes,
		req.ResponseTypes, scopeCeiling, req.TokenEndpointAuthMethod); err != nil {
		log.Printf("[oauth] DCR insert failed: request_id=%s err=%v", oauthRequestID(w, r), err)
		writeOAuthError(w, r, http.StatusInternalServerError, "server_error", "failed to register client")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(OAuthRegisterResponse{
		ClientID:                clientID,
		ClientIDIssuedAt:        timeNow().Unix(),
		ClientName:              req.ClientName,
		RedirectURIs:            req.RedirectURIs,
		GrantTypes:              req.GrantTypes,
		ResponseTypes:           req.ResponseTypes,
		TokenEndpointAuthMethod: req.TokenEndpointAuthMethod,
		// Echo the granted ceiling, not the raw request: per RFC 7591 the
		// response reflects the registered metadata, so a client that asked
		// for a superset (or an ineligible/unknown scope) learns what it
		// actually got.
		Scope: strings.Join(scopeCeiling, " "),
	})
}

// OAuthClientPublicMetadata is the subset of an oauth_clients row we
// surface to anonymous readers. The consent page (web/) fetches this
// so it can render the friendly client_name beside the requested
// scope. No secrets are present in this struct; secret_hash and
// internal bookkeeping fields are deliberately omitted.
type OAuthClientPublicMetadata struct {
	ClientID         string   `json:"client_id"`
	ClientName       string   `json:"client_name"`
	RedirectURIs     []string `json:"redirect_uris"`
	Scopes           []string `json:"scopes"`
	ClientIDIssuedAt int64    `json:"client_id_issued_at"`
	// AccountEligible reports whether the consent screen may offer account
	// (workspace-admin) scope for this client: true iff "account" is on the
	// client's registered scope ceiling (which DCR writes only when the
	// redirect is account-eligible AND the client requested it). Drives the
	// consent UI (the Account option is disabled otherwise); fosite re-checks
	// granted ⊆ registered at grant time, so this is UX, not the security
	// boundary.
	AccountEligible bool `json:"account_eligible"`
}

// handleOAuthGetClient is the public read endpoint the consent UI
// uses to look up client metadata by client_id. Anonymous: RFC 7591
// §4 says client metadata is generally not secret, and the consent
// screen needs the friendly name to give the user a meaningful
// "Allow X to access Y" prompt. 404 when oauth is not wired or the
// client_id is unknown; 500 on transient DB errors so the UI can
// distinguish "client doesn't exist" from "backend is sick".
// Rate-limited per source IP because the endpoint is anonymous and
// every request runs an indexed PK lookup — without the gate, an
// attacker could sustain high-QPS DB pressure against a path that
// CDNs can't absorb (Go's http.NotFound emits no Cache-Control, so
// 404 responses aren't edge-cacheable).
func (a *API) handleOAuthGetClient(w http.ResponseWriter, r *http.Request) {
	if a.oauthStorage == nil {
		http.NotFound(w, r)
		return
	}
	if ok, retryAfter := a.dcrLimit.AllowWithRetryAfter(dcrSourceIP(r)); !ok {
		secs := int(retryAfter.Round(time.Second).Seconds())
		if secs < 1 {
			secs = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		writeOAuthError(w, r, http.StatusTooManyRequests, "rate_limited",
			"too many client-metadata requests from this IP; try again later")
		return
	}
	clientID := mux.Vars(r)["client_id"]
	if clientID == "" {
		http.NotFound(w, r)
		return
	}
	var (
		name      string
		redirects []string
		scopes    []string
		createdAt time.Time
	)
	err := a.oauthStorage.Pool().QueryRow(r.Context(), `
		SELECT client_name, redirect_uris, scopes, created_at
		  FROM oauth_clients
		 WHERE client_id = $1
	`, clientID).Scan(&name, &redirects, &scopes, &createdAt)
	if err != nil {
		// Distinguish "not found" (which 404 is the right answer for)
		// from a transient DB failure (which the UI should retry
		// instead of telling the user "this client isn't registered").
		// pgx.ErrNoRows is the canonical sentinel for an empty row.
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return
		}
		log.Printf("[oauth] client metadata lookup failed: request_id=%s client=%q err=%v",
			oauthRequestID(w, r), clientID, err)
		writeOAuthError(w, r, http.StatusInternalServerError, "server_error",
			"client metadata lookup failed; try again")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=60")
	// account_eligible drives whether the consent screen offers (and defaults
	// to) account. It reflects the client's REGISTERED ceiling, not the raw
	// redirect: DCR only writes account into the ceiling when the redirect
	// allows it AND the client requested it, so reading the scopes column
	// keeps the UI consistent with what fosite will actually grant (no
	// account radio the user can pick only to hit invalid_scope at /authorize).
	accountEligible := false
	for _, s := range scopes {
		if s == identity.ScopeAccount {
			accountEligible = true
			break
		}
	}
	_ = json.NewEncoder(w).Encode(OAuthClientPublicMetadata{
		ClientID:         clientID,
		ClientName:       name,
		RedirectURIs:     redirects,
		Scopes:           scopes,
		ClientIDIssuedAt: createdAt.Unix(),
		AccountEligible:  accountEligible,
	})
}

// generateClientID returns a fresh mcp_-prefixed client_id. 12 hex chars
// (6 bytes / 48 bits) is enough entropy to make accidental collision
// negligible; client_ids are identifiers, not secrets.
func generateClientID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("oauth: crypto/rand failed: %v", err))
	}
	return oauth.ClientIDPrefix + hex.EncodeToString(b)
}

// timeNow is a seam for tests. Returns wall-clock by default.
var timeNow = func() time.Time { return time.Now() }

// OAuthMetadata is the RFC 8414 authorization-server metadata document
// served at /.well-known/oauth-authorization-server. Field names use
// snake_case per §2 of the RFC. Only the fields e2a actually advertises
// are present — omitting an OPTIONAL field (introspection_endpoint,
// jwks_uri, …) signals to clients that the feature isn't supported.
type OAuthMetadata struct {
	Issuer                                 string   `json:"issuer"`
	AuthorizationEndpoint                  string   `json:"authorization_endpoint"`
	TokenEndpoint                          string   `json:"token_endpoint"`
	RegistrationEndpoint                   string   `json:"registration_endpoint"`
	RevocationEndpoint                     string   `json:"revocation_endpoint"`
	ResponseTypesSupported                 []string `json:"response_types_supported"`
	ResponseModesSupported                 []string `json:"response_modes_supported"`
	GrantTypesSupported                    []string `json:"grant_types_supported"`
	CodeChallengeMethodsSupported          []string `json:"code_challenge_methods_supported"`
	TokenEndpointAuthMethodsSupported      []string `json:"token_endpoint_auth_methods_supported"`
	RevocationEndpointAuthMethodsSupported []string `json:"revocation_endpoint_auth_methods_supported"`
	ScopesSupported                        []string `json:"scopes_supported"`
	// JWKSURI points at the public key set agents verify e2a-minted JWTs
	// against (Slice 5b). Always present once a signer surface exists; the
	// endpoint serves {"keys":[]} when no signing key is configured.
	JWKSURI string `json:"jwks_uri,omitempty"`
	// AgentAuth is the auth.md agent-identity discovery block (Slice 5b-2).
	// Present only when the agent-identity surface is enabled (a signing key is
	// configured) — advertising endpoints that 501 would mislead clients.
	AgentAuth *agentAuthMetadata `json:"agent_auth,omitempty"`
	// RFC 9207 §3 — advertises that authorize responses carry `iss`
	// (we emit it manually in writeAuthorizeRedirect since fosite
	// v0.49 doesn't ship native RFC 9207 support).
	AuthorizationResponseIssParameterSupported bool `json:"authorization_response_iss_parameter_supported,omitempty"`
}

// oauthIssuer returns the canonical authorization-server issuer used by
// discovery, authorization responses, and agent-auth tokens.
func (a *API) oauthIssuer() string {
	return strings.TrimRight(a.apiURL, "/")
}

// agentAuthMetadata is the auth.md `agent_auth` discovery block (Slice 5b-2).
// Advertised only when the agent-identity surface is live.
type agentAuthMetadata struct {
	IdentityEndpoint       string   `json:"identity_endpoint"`
	IdentityTypesSupported []string `json:"identity_types_supported"`
	// AssertionTypesSupported lists provider-assertion types (e.g. ID-JAG)
	// accepted at /agent/identity. Omitted until 5b-4 actually intakes them —
	// advertising a type that currently 400s would mislead feature-detecting
	// clients.
	AssertionTypesSupported []string `json:"assertion_types_supported,omitempty"`
}

// handleOAuthDiscovery serves the RFC 8414 metadata document.
//
// Unauthenticated by design — RFC 8414 §3 requires the document be
// publicly retrievable. Cache for 1h: values are deployment-static.
//
// 404 when publicURL is empty: the metadata MUST contain absolute URLs
// (RFC 8414 §2) and we'd rather hide the endpoint than emit values
// derived from request headers (X-Forwarded-Host spoofing → issuer
// confusion). 404 when OAuth isn't configured (provider not wired) so
// the announcement matches actual behavior.
func (a *API) handleOAuthDiscovery(w http.ResponseWriter, r *http.Request) {
	// Gated on the fosite provider, which in production is co-wired with the
	// agent-auth signer: cmd/e2a wires BOTH only when http.public_url is set
	// (and the signer additionally requires agentAuthReady ⊇ publicURL). So a
	// "signer enabled but provider nil" deployment can't occur in prod — the
	// only place that config exists is test harnesses that SetSigner without
	// SetOAuthProvider, which don't hit discovery. Thus this guard never hides
	// a live agent-auth surface in practice.
	if a.oauthProvider == nil || a.publicURL == "" {
		http.NotFound(w, r)
		return
	}
	// apiBase is the issuer + programmatic endpoints (token/register/revoke/
	// jwks/identity) — they live with the API host (apiURL). webBase is the
	// browser-facing authorization_endpoint — it stays on the web app
	// (publicURL) where the login/consent UI lives. When api_url is unset
	// the two are identical (single-host), preserving historical behaviour.
	apiBase := a.oauthIssuer()
	if apiBase == "" {
		http.NotFound(w, r)
		return
	}
	webBase := strings.TrimRight(a.publicURL, "/")
	meta := OAuthMetadata{
		Issuer:                 apiBase,
		AuthorizationEndpoint:  webBase + "/oauth2/authorize",
		TokenEndpoint:          apiBase + "/oauth2/token",
		RegistrationEndpoint:   apiBase + "/oauth2/register",
		RevocationEndpoint:     apiBase + "/oauth2/revoke",
		ResponseTypesSupported: []string{"code"},
		// response_mode=query is the only mode we emit at the redirect
		// URI; explicit so strict MCP clients don't try "fragment".
		ResponseModesSupported:                 []string{"query"},
		GrantTypesSupported:                    []string{"authorization_code", "refresh_token"},
		CodeChallengeMethodsSupported:          []string{"S256"},
		TokenEndpointAuthMethodsSupported:      []string{"none"},
		RevocationEndpointAuthMethodsSupported: []string{"none"},
		// Scope vocabulary is the §6a tier model (Slice 5b): agent (runtime/
		// inbox, the DCR-public default) and account (admin). The lone legacy
		// "mcp" scope is retired.
		ScopesSupported: []string{"agent", "account"},
		JWKSURI:         apiBase + "/.well-known/jwks.json",
		AuthorizationResponseIssParameterSupported: true,
	}
	// auth.md agent-identity surface (Slice 5b-2) — advertised only when a
	// signing key is configured, so we never announce endpoints that 501.
	if a.agentAuthReady() {
		meta.GrantTypesSupported = append(meta.GrantTypesSupported, jwtBearerGrantType)
		meta.AgentAuth = &agentAuthMetadata{
			IdentityEndpoint:       apiBase + "/agent/identity",
			IdentityTypesSupported: []string{"identity_assertion"},
			// AssertionTypesSupported left empty until 5b-4 intakes ID-JAG.
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_ = json.NewEncoder(w).Encode(meta)
}

// handleJWKS serves /.well-known/jwks.json — the public RS256 key(s) agents use
// to verify e2a-minted JWTs (Slice 5b). Unauthenticated by design (a JWKS is
// public). Always available: when no signing key is configured it serves a
// valid empty set ({"keys":[]}) rather than 404, so a verifier gets a definite
// "no keys" answer instead of an ambiguous routing failure. Cached 1h like the
// discovery doc; rotation publishes a new kid which clients pick up within TTL.
func (a *API) handleJWKS(w http.ResponseWriter, r *http.Request) {
	var jwks jose.JSONWebKeySet
	if a.signer != nil {
		jwks = a.signer.PublicJWKS()
	} else {
		jwks = jose.JSONWebKeySet{Keys: []jose.JSONWebKey{}}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_ = json.NewEncoder(w).Encode(jwks)
}

// handleOAuthRevoke is the /oauth2/revoke endpoint (RFC 7009).
// Thin shim over fosite's NewRevocationRequest → WriteRevocationResponse:
//
//   - parses + validates the token + token_type_hint per RFC 7009 §2.1
//   - dispatches by hint (or both if absent) to the right storage layer
//   - on access-token revoke: marks the row revoked
//   - on refresh-token revoke: cascades to the whole request_id family
//     (every access token issued from the same grant)
//   - on unknown token: 200 silently (RFC 7009 §2.2 — don't reveal
//     whether tokens exist)
//
// 404s when SetOAuthProvider wasn't called. Otherwise fosite writes
// the RFC 7009 §2.2-shaped response itself (200 OK with no body on
// success, or §5.2 JSON error on parse/auth failure).
func (a *API) handleOAuthRevoke(w http.ResponseWriter, r *http.Request) {
	if a.oauthProvider == nil {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	err := a.oauthProvider.NewRevocationRequest(ctx, r)
	// WriteRevocationResponse handles both the success (200, no body)
	// and the failure (JSON error envelope) paths. Per RFC 7009 §2.2
	// it treats "token not found" the same as success — fosite
	// already does that internally.
	a.oauthProvider.WriteRevocationResponse(ctx, w, err)
}

// logTokenError emits a structured line for a failed /token exchange.
// Captures enough to spot patterns (repeated invalid_grant from one
// client, brute-force bad-PKCE attempts) without leaking anything
// sensitive. fosite's RFC6749Error.Error() returns only the code string
// ("invalid_grant"), which can't distinguish expired-code from
// PKCE-mismatch from replay — so we also log the hint (and debug, when
// present). Safe to log: on every /token grant path (auth-code, refresh,
// PKCE) fosite's hints are static category strings, and debug carries the
// wrapped storage error text — never the code, verifier, or tokens.
// fosite may hand us a nil requester or a partial one when the request
// failed during parsing; we don't panic on either.
func logTokenError(req fosite.AccessRequester, stage, reqID string, err error) {
	clientID := ""
	grantType := ""
	if req != nil {
		if c := req.GetClient(); c != nil {
			clientID = c.GetID()
		}
		grantType = req.GetRequestForm().Get("grant_type")
	}
	rfcErr := fosite.ErrorToRFC6749Error(err)
	log.Printf("[oauth] /token %s error: request_id=%s client=%q grant=%q code=%s hint=%q debug=%q",
		stage, reqID, clientID, grantType, rfcErr.ErrorField, rfcErr.Reason(), rfcErr.Debug())
}

// ───────────────────────── /authorize ─────────────────────────

// handleOAuthAuthorize is the entry point for the OAuth browser flow.
//
// Steps:
//  1. Hand the request to fosite to validate every parameter (client
//     exists, redirect_uri matches the registered set, response_type
//     == "code", PKCE shape, scope, state). fosite writes the
//     appropriate RFC 6749 §4.1.2.1 error response itself on failure
//     — either a redirect to redirect_uri?error=… (when the URI was
//     verified-safe) or a direct 400 (when it wasn't).
//  2. Check the user's session cookie. No session → 302 to
//     /api/auth/login. Today we don't carry a return_to (port lands
//     in a later slice); operators see a log line on every such
//     redirect so the missing piece is visible.
//  3. With a session, 302 to {publicURL}/oauth/consent?<params>. The
//     consent UI in web/ POSTs back to /oauth2/consent.
func (a *API) handleOAuthAuthorize(w http.ResponseWriter, r *http.Request) {
	if a.oauthProvider == nil {
		http.NotFound(w, r)
		return
	}
	if a.publicURL == "" {
		http.Error(w, "OAuth flow not configured: http.public_url is unset", http.StatusServiceUnavailable)
		return
	}
	ctx := r.Context()

	ar, err := a.oauthProvider.NewAuthorizeRequest(ctx, r)
	if err != nil {
		// fosite writes the right response shape (redirect-with-error
		// when the URI was verified, direct error otherwise).
		a.writeAuthorizeError(ctx, w, ar, err)
		return
	}

	// Session check. The userAuth path produces the same session
	// cookie the rest of the dashboard uses (e2a_session).
	if a.userAuth == nil {
		http.Error(w, "user auth not configured on this deployment", http.StatusServiceUnavailable)
		return
	}
	if user := a.userAuth.AuthenticateRequest(r); user == nil {
		// Bounce through Google login, then resume back here. We only
		// pass the path portion (not host/scheme) so the same-origin
		// invariant of validateReturnToPath holds. After callback the
		// user lands back on /oauth2/authorize with the same params
		// and the now-valid session cookie.
		loginURL, _ := url.Parse(strings.TrimRight(a.publicURL, "/") + "/api/auth/login")
		q := loginURL.Query()
		q.Set("return_to", r.URL.RequestURI())
		loginURL.RawQuery = q.Encode()
		http.Redirect(w, r, loginURL.String(), http.StatusFound)
		return
	}

	// 302 to consent UI with all authorize params re-passed. The
	// consent page hidden-fields these back into its POST so we can
	// re-parse the request via fosite without trusting a server-side
	// session stash (which would otherwise be the natural place but
	// adds operational complexity for little gain).
	consentURL, _ := url.Parse(strings.TrimRight(a.publicURL, "/") + "/oauth/consent")
	consentURL.RawQuery = r.URL.RawQuery
	http.Redirect(w, r, consentURL.String(), http.StatusFound)
}

// ───────────────────────── /consent ─────────────────────────

// handleOAuthConsent processes the consent form POSTed by the web/
// consent UI. Form fields:
//
//   - all the authorize-request params (response_type, client_id,
//     redirect_uri, scope, state, code_challenge,
//     code_challenge_method) — re-passed as hidden inputs by the
//     consent page so we can rebuild the fosite AuthorizeRequester
//   - action: "allow" | "deny"
//   - agent_choice: "create_new" | "existing:<email>"
//   - new_agent_slug: optional, used when agent_choice == create_new
//
// On allow + create_new we open a transaction (via Storage.Pool().Begin)
// that spans BOTH the agent insert (identity package) AND the auth-
// code insert (oauth package — fosite calls our Storage internally).
// The same context carries the tx so both packages join it; commit
// happens after both succeed. A partial failure rolls back, so we
// can't leak an agent the user never authorized.
func (a *API) handleOAuthConsent(w http.ResponseWriter, r *http.Request) {
	if a.oauthProvider == nil {
		http.NotFound(w, r)
		return
	}
	// Resolved up front so every failure log line below can carry it;
	// matches the X-Request-Id response header (middleware-set in prod).
	reqID := oauthRequestID(w, r)
	if a.userAuth == nil {
		log.Printf("[oauth] /consent unavailable: user auth not configured: request_id=%s", reqID)
		http.Error(w, "user auth not configured on this deployment", http.StatusServiceUnavailable)
		return
	}
	if a.oauthStorage == nil {
		log.Printf("[oauth] /consent unavailable: oauth storage not configured: request_id=%s", reqID)
		http.Error(w, "oauth storage not configured on this deployment", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "could not parse form body", http.StatusBadRequest)
		return
	}
	ctx := r.Context()

	// Rebuild the authorize request from the form values. fosite's
	// NewAuthorizeRequest reads from r.URL.Query() — we synthesize a
	// query-string by promoting the POSTed form fields. This way the
	// same validator runs in the same configuration on both /authorize
	// and /consent, so a tampered hidden field gets caught.
	authReq := r.Clone(ctx)
	authReq.URL.RawQuery = r.PostForm.Encode()
	ar, err := a.oauthProvider.NewAuthorizeRequest(ctx, authReq)
	if err != nil {
		a.writeAuthorizeError(ctx, w, ar, err)
		return
	}

	user := a.userAuth.AuthenticateRequest(r)
	if user == nil {
		http.Error(w, "session required: log in before consenting", http.StatusUnauthorized)
		return
	}

	action := r.PostFormValue("action")
	if action == "deny" {
		// RFC 6749 §4.1.2.1: redirect back with error=access_denied.
		// writeAuthorizeError delegates the error shape to fosite and
		// adds the RFC 9207 issuer when fosite emits a redirect.
		a.writeAuthorizeError(ctx, w, ar,
			fosite.ErrAccessDenied.WithHint("user denied consent"))
		return
	}
	if action != "allow" {
		http.Error(w, "action must be 'allow' or 'deny'", http.StatusBadRequest)
		return
	}

	// Scope the user consented to. The consent screen is e2a's scope
	// authority: "agent" binds the grant to one inbox; "account" is full
	// workspace admin. account is granted to loopback OR https redirect_uris
	// (see accountEligibleRedirect for the deliberate policy). The gate is
	// enforced here, server-side and fail-closed, independent of the page's UI.
	scopeChoice := r.PostFormValue("scope_choice")
	if scopeChoice == "" {
		scopeChoice = identity.ScopeAgent
	}
	switch scopeChoice {
	case identity.ScopeAccount:
		if !accountEligibleRedirect(ar.GetRedirectURI().String()) {
			a.writeAuthorizeError(ctx, w, ar,
				fosite.ErrInvalidScope.WithHint(
					"account scope may only be granted to a client with a loopback or https redirect_uri"))
			return
		}
		// account is not inbox-bound: empty AgentEmail, account scope. The
		// principal resolver maps (account, no agent) to an account principal,
		// exactly like an e2a_acct_ key; any agent_choice is ignored.
		if err := a.issueOAuthCode(ctx, w, r, ar, user.ID, "", identity.ScopeAccount); err != nil {
			log.Printf("[oauth] /consent issue (account) failed: request_id=%s err=%v", reqID, err)
			a.writeAuthorizeError(ctx, w, ar, err)
		}
		return
	case identity.ScopeAgent:
		// fall through to the inbox-binding logic below
	default:
		http.Error(w, `scope_choice must be "agent" or "account"`, http.StatusBadRequest)
		return
	}

	// Resolve which agent the (agent-scoped) OAuth grant pins to. Either an
	// existing inbox the user already owns, or a freshly auto-created one on
	// the shared domain.
	agentChoice := r.PostFormValue("agent_choice")
	switch {
	case strings.HasPrefix(agentChoice, "existing:"):
		email := identity.NormalizeEmail(strings.TrimPrefix(agentChoice, "existing:"))
		// ANTI-ENUMERATION INVARIANT: "no such agent" and "exists but owned
		// by another account" MUST be indistinguishable — same status, same
		// body. Reaching this line needs only a logged-in session (checked
		// above) plus any self-registered OAuth client, and agent_choice is
		// an arbitrary caller-supplied address, so a distinguishable error
		// turns this endpoint into an existence oracle for other accounts'
		// inboxes. It would leak strictly more than the SMTP edge does:
		// session.Rcpt returns the SAME 550 for an unknown recipient and for
		// a real agent on an unverified domain (internal/relay/server.go), so
		// unverified-domain agents are invisible over mail and must stay
		// invisible here. The consent page only ever offers the user their
		// own agents, so nothing legitimate needs the distinction.
		//
		// Same invariant as resolveOwnedAgent (internal/httpapi/operations.go)
		// and the WebSocket surface, expressed as a 400 rather than their 404
		// because this is a form POST inside the consent flow, not a REST
		// resource lookup. A store error collapses in here too: the caller
		// learns nothing either way and the operator gets the detail from the
		// log line below.
		agent, err := a.store.GetAgentByEmail(ctx, email)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			// Deliberately omits the address: this is an unresolved,
			// attacker-chosen string that may name a third party, and it
			// must not reach shipped logs (same rule as relay.Rcpt).
			log.Printf("[oauth] /consent agent lookup failed: request_id=%s err=%v", reqID, err)
		}
		if err != nil || agent.UserID != user.ID {
			http.Error(w, "unknown or inaccessible agent", http.StatusBadRequest)
			return
		}
		// No agent creation needed — drop straight into the code-
		// issue path with the resolved email on the session.
		if err := a.issueOAuthCode(ctx, w, r, ar, user.ID, email, identity.ScopeAgent); err != nil {
			log.Printf("[oauth] /consent issue (existing agent) failed: request_id=%s err=%v", reqID, err)
			a.writeAuthorizeError(ctx, w, ar, err)
		}

	case agentChoice == "create_new":
		if a.sharedDomain == "" {
			http.Error(w, "shared-domain auto-create is not configured", http.StatusServiceUnavailable)
			return
		}
		slug := strings.TrimSpace(r.PostFormValue("new_agent_slug"))
		if slug == "" {
			slug = defaultAgentSlug(ar.GetClient().GetID())
		}
		if err := validateSlug(slug); err != nil {
			http.Error(w, "invalid slug: "+err.Error(), http.StatusBadRequest)
			return
		}
		agentEmail := slug + "@" + a.sharedDomain
		if err := a.issueOAuthCodeWithNewAgent(ctx, w, r, ar, user.ID, agentEmail, identity.ScopeAgent); err != nil {
			log.Printf("[oauth] /consent issue (new agent) failed: request_id=%s err=%v", reqID, err)
			a.writeAuthorizeError(ctx, w, ar, err)
		}

	default:
		http.Error(w, "agent_choice must be 'existing:<email>' or 'create_new'", http.StatusBadRequest)
	}
}

// queryModeAuthorizeRequester makes error responses follow the only response
// mode this server advertises. fosite otherwise uses a requested but unsupported
// fragment or form_post mode while reporting an earlier validation error.
// Correctness also depends on the provider using fosite's default response
// mode handler; e2a does not configure a ResponseModeHandlerExtension.
type queryModeAuthorizeRequester struct {
	fosite.AuthorizeRequester
}

func (queryModeAuthorizeRequester) GetResponseMode() fosite.ResponseModeType {
	return fosite.ResponseModeQuery
}

func (r queryModeAuthorizeRequester) GetLang() language.Tag {
	if ctx, ok := r.AuthorizeRequester.(fosite.G11NContext); ok {
		return ctx.GetLang()
	}
	return language.English
}

var _ fosite.G11NContext = queryModeAuthorizeRequester{}

// authorizeErrorResponseWriter decorates redirecting authorization errors
// with the RFC 9207 issuer identifier. fosite v0.49.0 owns the validation,
// status code, and error parameters but has no native RFC 9207 support, so we
// amend only a Location header it has already decided is safe to emit. Direct
// error responses (for example, an unknown client or untrusted redirect_uri)
// have no Location and pass through unchanged.
type authorizeErrorResponseWriter struct {
	http.ResponseWriter
	issuer      string
	wroteHeader bool
}

func (w *authorizeErrorResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *authorizeErrorResponseWriter) WriteHeader(statusCode int) {
	if w.wroteHeader {
		return
	}
	if err := w.appendIssuer(); err != nil {
		w.Header().Del("Location")
		w.wroteHeader = true
		http.Error(w.ResponseWriter, err.Error(), http.StatusBadRequest)
		return
	}
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *authorizeErrorResponseWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}

func (w *authorizeErrorResponseWriter) appendIssuer() error {
	location := w.Header().Get("Location")
	if location == "" {
		return nil
	}
	redirect, err := url.Parse(location)
	if err != nil {
		return errors.New("invalid redirect_uri")
	}
	if isDangerousOAuthRedirectScheme(redirect.Scheme) {
		return errors.New("invalid redirect_uri scheme")
	}
	if w.issuer == "" {
		return nil
	}
	q := redirect.Query()
	q.Set("iss", w.issuer)
	redirect.RawQuery = q.Encode()
	w.Header().Set("Location", redirect.String())
	return nil
}

func (a *API) writeAuthorizeError(ctx context.Context, w http.ResponseWriter, ar fosite.AuthorizeRequester, err error) {
	a.oauthProvider.WriteAuthorizeError(ctx, &authorizeErrorResponseWriter{
		ResponseWriter: w,
		issuer:         a.oauthIssuer(),
	}, queryModeAuthorizeRequester{AuthorizeRequester: ar}, err)
}

// issueOAuthCode is the no-new-agent path. fosite mints the code via
// our Storage on the pool (no cross-package tx needed). After fosite's
// NewAuthorizeResponse we write the redirect ourselves so we can
// append the RFC 9207 iss parameter — fosite v0.49.0 doesn't emit it.
func (a *API) issueOAuthCode(ctx context.Context, w http.ResponseWriter, r *http.Request, ar fosite.AuthorizeRequester, userID, agentEmail, scope string) error {
	sess := &oauth.Session{
		UserID:     userID,
		AgentEmail: agentEmail,
		Subject:    userID,
	}
	ar.SetSession(sess)
	grantConsentedScope(ar, scope)

	resp, err := a.oauthProvider.NewAuthorizeResponse(ctx, ar, sess)
	if err != nil {
		return err
	}
	a.writeAuthorizeRedirect(w, r, ar, resp)
	return nil
}

// issueOAuthCodeWithNewAgent is the auto-create path: agent insert +
// code insert in a single pgx transaction. The tx is opened on the
// shared Storage pool; both writes join it via the oauth.WithTx
// context helper. A partial failure rolls back so we never leak an
// agent without the matching code (or vice versa).
// grantConsentedScope sets the access grant to EXACTLY the scope the user
// consented to on the consent screen — not the scope the client requested.
// The consent screen is e2a's scope authority (it already picks the agent),
// so a user who chooses "account" on a loopback client gets account even
// though the public DCR client only requested "agent".
//
// AppendRequestedScope is LOAD-BEARING, not cosmetic: fosite's
// AuthorizeExplicitGrantHandler re-validates every *requested* scope against
// client.GetScopes() (ExactScopeStrategy) inside NewAuthorizeResponse — which
// runs AFTER this. So appending the consented scope to the requested set is
// what subjects it to that check, and that check is precisely what enforces
// granted ⊆ client.scopes: account is granted only if the client row carries
// it (which DCR writes only for account-eligible — loopback or https —
// clients that requested it). Remove the append and account-scope containment
// silently breaks — keep it.
func grantConsentedScope(ar fosite.AuthorizeRequester, scope string) {
	if !ar.GetRequestedScopes().Has(scope) {
		ar.AppendRequestedScope(scope)
	}
	ar.GrantScope(scope)
}

func (a *API) issueOAuthCodeWithNewAgent(ctx context.Context, w http.ResponseWriter, r *http.Request, ar fosite.AuthorizeRequester, userID, agentEmail, scope string) error {
	// Same per-user agent cap the REST create path enforces (see
	// EnforceAgentCreate in agents_write.go); this auto-create path had no
	// enforcement at all before this check.
	if a.enforcer != nil {
		if err := a.enforcer.CheckAgentCreate(ctx, userID); err != nil {
			if limErr, ok := limits.IsLimitExceeded(err); ok {
				http.Error(w, limErr.Error(), http.StatusPaymentRequired)
				return nil
			}
			return fmt.Errorf("check agent create: %w", err)
		}
	}

	pool := a.oauthStorage.Pool()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	// Safe to defer unconditionally: Rollback is a no-op after Commit
	// in pgx v5.
	defer func() { _ = tx.Rollback(ctx) }()
	txCtx := oauth.WithTx(ctx, tx)

	// Agent insert via the identity package — same tx, same context.
	if _, err := a.store.CreateAgentTx(txCtx, tx, agentEmail, a.sharedDomain, "", "", "", userID); err != nil {
		if isUniqueViolation(err) {
			http.Error(w, "that slug is already taken; pick another", http.StatusConflict)
			return nil
		}
		return fmt.Errorf("create agent: %w", err)
	}

	// Code issue via fosite. Storage.db(txCtx) finds the tx on the
	// context, so the INSERT into oauth_auth_codes runs in the same tx.
	sess := &oauth.Session{
		UserID:     userID,
		AgentEmail: agentEmail,
		Subject:    userID,
	}
	ar.SetSession(sess)
	grantConsentedScope(ar, scope)
	resp, err := a.oauthProvider.NewAuthorizeResponse(txCtx, ar, sess)
	if err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}

	a.writeAuthorizeRedirect(w, r, ar, resp)
	return nil
}

// writeAuthorizeRedirect emits the 303 redirect to the client's
// redirect_uri with fosite's parameters + the RFC 9207 issuer. We
// bypass fosite.WriteAuthorizeResponse so we can append `iss`;
// fosite v0.49.0 doesn't emit it natively.
func (a *API) writeAuthorizeRedirect(w http.ResponseWriter, r *http.Request, ar fosite.AuthorizeRequester, resp fosite.AuthorizeResponder) {
	redirect, err := url.Parse(ar.GetRedirectURI().String())
	if err != nil {
		http.Error(w, "invalid redirect_uri", http.StatusBadRequest)
		return
	}
	// Defense-in-depth: DCR's validateRedirectURI is the primary gate
	// against dangerous schemes, but if a row was inserted by some
	// other path (operator script, future endpoint, migration replay
	// of legacy data), refuse to emit Location: javascript:… here.
	if isDangerousOAuthRedirectScheme(redirect.Scheme) {
		http.Error(w, "invalid redirect_uri scheme", http.StatusBadRequest)
		return
	}
	q := redirect.Query()
	for k, vs := range resp.GetParameters() {
		for _, v := range vs {
			q.Set(k, v)
		}
	}
	// RFC 9207 §2 — tell mix-up-aware clients which AS produced this
	// response. Must byte-match the `issuer` discovery advertises (apiURL).
	if issuer := a.oauthIssuer(); issuer != "" {
		q.Set("iss", issuer)
	}
	redirect.RawQuery = q.Encode()
	http.Redirect(w, r, redirect.String(), http.StatusSeeOther)
}

// defaultAgentSlug derives a slug-safe default from the client_id.
// Used when the user clicks Allow without typing a custom slug. The
// 6-hex suffix gives 24 bits of collision resistance — plenty given
// uniqueness is checked at INSERT time on the shared domain.
func defaultAgentSlug(clientID string) string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	suffix := hex.EncodeToString(b)
	prefix := slugifyClientID(clientID)
	if prefix == "" {
		prefix = "agent"
	}
	out := prefix + "-" + suffix
	if len(out) > 40 {
		out = out[:40]
	}
	return out
}

// slugifyClientID lowercases and strips a client_id to slug-safe chars.
// The "mcp_<hex>" client IDs we mint produce slugs like "mcp-abc123".
func slugifyClientID(clientID string) string {
	var b strings.Builder
	prev := byte(0)
	for i := 0; i < len(clientID); i++ {
		c := clientID[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			b.WriteByte(c)
		case c >= 'A' && c <= 'Z':
			b.WriteByte(c + 32)
		default:
			// Collapse runs of non-alnum into a single hyphen.
			if prev != '-' && b.Len() > 0 {
				b.WriteByte('-')
				prev = '-'
				continue
			}
		}
		if b.Len() > 0 {
			prev = b.String()[b.Len()-1]
		}
	}
	return strings.TrimRight(b.String(), "-")
}

// isUniqueViolation reports whether err is a Postgres unique-violation
// (SQLSTATE 23505). Used by issueOAuthCodeWithNewAgent to surface slug
// collisions as a clean 409 to the user.
func isUniqueViolation(err error) bool {
	var pgErr interface {
		SQLState() string
	}
	if errors.As(err, &pgErr) {
		return pgErr.SQLState() == "23505"
	}
	return false
}
