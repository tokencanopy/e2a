// Package delegated verifies externally issued OAuth 2.0 access tokens
// (RFC 9068 `at+jwt`) minted by a configured OIDC issuer, so an operator's
// control plane can call the e2a API on behalf of its own signed-in humans
// without holding e2a credentials. It is disabled by default and fully
// generic: every deployment-specific value (issuer, audience, authorized
// party, scope, claim policy) arrives via configuration — nothing in this
// package names any particular operator.
//
// The package owns exactly one token kind: a compact JWT whose protected
// JOSE header carries `typ":"at+jwt"`. Classification (Classify) inspects
// only the protected header; ownership is decided before any signature or
// network work, and an owned token never falls through to e2a's other
// credential paths — a malformed or invalid delegated token is a 401, not
// an API-key probe.
//
// Every size limit below is part of the delegated-token contract and is
// enforced byte-for-byte (and, where stated, code-point-for-code-point).
// The limits apply to delegated verification only; existing agent-JWT and
// API-key contracts are unchanged.
package delegated

import (
	"errors"
	"time"
	"unicode/utf8"
)

// Exact credential size limits. Each rejects at limit+1.
const (
	// MaxAuthorizationBytes caps the raw Authorization header, including
	// the "Bearer " prefix, in ASCII bytes.
	MaxAuthorizationBytes = 16384
	// MaxCompactJWTBytes caps the compact token (three non-empty
	// base64url segments) in ASCII bytes.
	MaxCompactJWTBytes = 16384
	// MaxProtectedHeaderBytes caps the base64url-DECODED protected header.
	MaxProtectedHeaderBytes = 1024
	// MaxPayloadBytes caps the base64url-decoded payload.
	MaxPayloadBytes = 8192
	// MaxSignatureBytes caps the base64url-decoded signature.
	MaxSignatureBytes = 1024
	// MaxTypBytes caps the protected `typ` value in ASCII bytes.
	MaxTypBytes = 32
	// MaxAlgBytes caps the protected `alg` value in ASCII bytes.
	MaxAlgBytes = 16
	// MaxKidBytes caps a protected or JWKS `kid` in ASCII bytes.
	MaxKidBytes = 128
	// MaxTopLevelClaims caps the number of top-level members in the JWT
	// claims object.
	MaxTopLevelClaims = 32
	// MaxClaimNameBytes caps a claim name in ASCII bytes. Claim names are
	// ASCII with no C0 control characters.
	MaxClaimNameBytes = 128

	// Dual code-point/byte limits for string claims. Both bounds apply
	// independently: a value fails at either limit+1.
	MaxIssuerCodePoints   = 512
	MaxIssuerBytes        = 2048
	MaxAudienceCodePoints = 256
	MaxAudienceBytes      = 1024
	MaxAzpCodePoints      = 128
	MaxAzpBytes           = 512
	// MaxScopeBytes caps the scope claim in ASCII bytes.
	MaxScopeBytes = 128
	// Subject-class claims: sub, jti, and every configured required
	// context claim share one limit, as does a provisioning external_ref.
	MaxSubjectCodePoints = 128
	MaxSubjectBytes      = 512
)

// TokenType is the protected-header `typ` this package owns, compared as
// the exact string (RFC 9068 registers the "application/at+jwt" media
// type; the issuer contract here mints exactly this short form).
const TokenType = "at+jwt"

// JWKS/discovery policy constants.
const (
	// FetchTimeout bounds one discovery or JWKS HTTP fetch.
	FetchTimeout = 10 * time.Second
	// MaxJWKSBytes caps the decoded JWKS response body.
	MaxJWKSBytes = 65536
	// MaxJWKSKeys caps accepted JWKs per issuer.
	MaxJWKSKeys = 32
	// KeysFreshFor is how long a successfully fetched keyset serves any
	// cached kid without a refresh.
	KeysFreshFor = 600 * time.Second
	// KeysStaleGrace extends KeysFreshFor for kids already present in the
	// last good set. Past KeysFreshFor+KeysStaleGrace even a known key is
	// verifier-unavailable until a refresh succeeds.
	KeysStaleGrace = 300 * time.Second
	// RefreshCooldown is the negative-result cooldown: after a failed
	// refresh, further refresh attempts fail without fetching.
	RefreshCooldown = 10 * time.Second
	// Refresh token bucket: RefreshBurst immediate refresh, refilling at
	// RefreshPerWindow per RefreshWindow.
	RefreshBurst     = 1
	RefreshPerWindow = 6
	RefreshWindow    = 60 * time.Second
)

// ErrInvalidToken is the terminal "this token is bad" class: signature,
// type, algorithm, issuer, audience, azp, scope, time, claim, or size
// failures, and an unknown kid after a successful refresh. Callers must
// map it to 401 with no check-specific detail.
var ErrInvalidToken = errors.New("delegated: invalid token")

// ErrUnavailable is the dynamic availability class: discovery not yet
// complete, JWKS transport/parse failure with no usable cached key, or a
// refresh denied by cooldown/rate limiting. Callers must map it to 503
// (never a WWW-Authenticate challenge) — it says nothing about the token.
var ErrUnavailable = errors.New("delegated: verifier unavailable")

// Claims is what verification exposes to authentication: the exact
// configured issuer the token verified against and the token's opaque
// subject. Identity mapping looks these up as a pair — never the subject
// alone — and nothing else from the token (profile claims, context
// claims) reaches identity resolution.
type Claims struct {
	Issuer  string
	Subject string
}

// RequiredClaim names one required nonempty string claim and, optionally,
// its closed set of allowed values.
type RequiredClaim struct {
	Name          string
	AllowedValues []string
}

// Config carries the deployment's verification policy. All values are
// deployment data — see the config package for validation; NewVerifier
// re-checks only what it cannot function without.
type Config struct {
	// IssuerURL is used for OIDC discovery and exact byte-for-byte `iss`
	// comparison. No alias or trailing-slash normalization.
	IssuerURL string
	// Audience is the exact single-string `aud`. Arrays are rejected even
	// when they contain this value.
	Audience string
	// AuthorizedParty is the exact required `azp`.
	AuthorizedParty string
	// RequiredScope is the exact singleton scope string — the claim must
	// equal it, not merely contain it.
	RequiredScope string
	// AllowedAlgorithms is the closed signature-algorithm allowlist
	// (subset of RS256/ES256 — the only algorithms this verifier
	// implements).
	AllowedAlgorithms []string
	// MaxTokenLifetime bounds `exp - iat`.
	MaxTokenLifetime time.Duration
	// ClockSkew admits an `iat` up to this far in the future and an `exp`
	// up to this far in the past. It never relaxes MaxTokenLifetime.
	ClockSkew time.Duration
	// RequiredClaims are context claims that must be present as bounded
	// nonempty strings.
	RequiredClaims []RequiredClaim
	// ForbiddenClaims must be absent entirely — present-as-null still
	// rejects.
	ForbiddenClaims []string
}

// boundedUTF8 reports whether s is nonempty valid UTF-8 within both the
// code-point and byte limits.
func boundedUTF8(s string, maxCodePoints, maxBytes int) bool {
	return s != "" && len(s) <= maxBytes && utf8.ValidString(s) &&
		utf8.RuneCountInString(s) <= maxCodePoints
}

// boundedASCII reports whether s is nonempty printable-range ASCII (0x20
// or above, below 0x80) within maxBytes.
func boundedASCII(s string, maxBytes int) bool {
	if s == "" || len(s) > maxBytes {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7f {
			return false
		}
	}
	return true
}
