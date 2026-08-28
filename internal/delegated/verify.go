package delegated

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-jose/go-jose/v4"
)

// Verifier is the one long-lived delegated-token verifier for the single
// configured issuer. Construction never performs network I/O: discovery
// runs on a background retry loop, so issuer unavailability degrades
// delegated authentication (503) without affecting startup or any other
// credential path.
type Verifier struct {
	cfg     Config
	metrics RefreshMetrics
	// now is injectable for tests; time.Now otherwise.
	now        func() time.Time
	httpClient *http.Client
	jwksURI    atomic.Pointer[string]
	cache      keyCache
	// discoveryDone, when non-nil, is closed after the discovery loop
	// exits — a test hook only.
	discoveryDone chan struct{}
}

// Option customizes a Verifier; test seams only.
type Option func(*Verifier)

// WithClock injects the time source.
func WithClock(now func() time.Time) Option { return func(v *Verifier) { v.now = now } }

// WithHTTPClient injects the HTTP client used for discovery and JWKS.
func WithHTTPClient(c *http.Client) Option { return func(v *Verifier) { v.httpClient = c } }

// WithDiscoveryDone arranges for ch to be closed when the background
// discovery loop exits.
func WithDiscoveryDone(ch chan struct{}) Option {
	return func(v *Verifier) { v.discoveryDone = ch }
}

// NewVerifier validates the static policy and starts background issuer
// discovery bounded by ctx. A static-policy error here is a
// misconfiguration the caller should treat as startup-fatal; network
// state never surfaces here.
func NewVerifier(ctx context.Context, cfg Config, metrics RefreshMetrics, opts ...Option) (*Verifier, error) {
	if err := validateStaticConfig(cfg); err != nil {
		return nil, err
	}
	v := &Verifier{
		cfg:        cfg,
		metrics:    metrics,
		now:        time.Now,
		httpClient: &http.Client{Timeout: FetchTimeout},
	}
	for _, opt := range opts {
		opt(v)
	}
	// go-oidc reads its HTTP client from the context; the discovery client
	// caps the one response go-oidc reads for us (maxDiscoveryBytes).
	go v.discoverWithRetry(oidcClientContext(ctx, discoveryClient(v.httpClient)))
	return v, nil
}

// validateStaticConfig is the defensive re-check of what Verify cannot
// run without. The config package owns the operator-facing validation
// (aggregated errors, production transport rules); this guards direct
// constructions in tests and future callers.
func validateStaticConfig(cfg Config) error {
	switch {
	case cfg.IssuerURL == "":
		return errors.New("delegated: issuer URL required")
	case cfg.Audience == "":
		return errors.New("delegated: audience required")
	case cfg.AuthorizedParty == "":
		return errors.New("delegated: authorized party required")
	case cfg.RequiredScope == "":
		return errors.New("delegated: required scope required")
	case len(cfg.AllowedAlgorithms) == 0:
		return errors.New("delegated: allowed algorithms required")
	case cfg.MaxTokenLifetime <= 0:
		return errors.New("delegated: max token lifetime must be positive")
	case cfg.ClockSkew < 0:
		return errors.New("delegated: clock skew must be nonnegative")
	}
	for _, alg := range cfg.AllowedAlgorithms {
		if alg != "RS256" && alg != "ES256" {
			return fmt.Errorf("delegated: unsupported algorithm %q", alg)
		}
	}
	return nil
}

// Verify authenticates one delegated-owned compact token end to end:
// exact size limits, protected-header pins, cached-key signature
// verification, and the full claim policy. On success it returns the
// verified (issuer, subject) pair and nothing else. Errors are
// ErrInvalidToken (401 class) or ErrUnavailable (503 class) — callers
// must not surface which check failed.
func (v *Verifier) Verify(ctx context.Context, bearer string) (*Claims, error) {
	hdr, ok := parseProtectedHeader(bearer)
	if !ok || hdr.Typ != TokenType {
		return nil, fmt.Errorf("%w: not a delegated compact token", ErrInvalidToken)
	}
	algAllowed := false
	for _, alg := range v.cfg.AllowedAlgorithms {
		if hdr.Alg == alg {
			algAllowed = true
			break
		}
	}
	if !algAllowed {
		return nil, fmt.Errorf("%w: algorithm not allowed", ErrInvalidToken)
	}
	if !boundedASCII(hdr.Kid, MaxKidBytes) {
		return nil, fmt.Errorf("%w: missing or oversized kid", ErrInvalidToken)
	}
	// Enforce the decoded payload/signature caps before any key or
	// crypto work.
	_, payloadSeg, signatureSeg, ok := splitCompact(bearer)
	if !ok {
		return nil, fmt.Errorf("%w: malformed compact token", ErrInvalidToken)
	}
	if _, ok := decodeSegment(payloadSeg, MaxPayloadBytes); !ok {
		return nil, fmt.Errorf("%w: payload out of bounds", ErrInvalidToken)
	}
	if _, ok := decodeSegment(signatureSeg, MaxSignatureBytes); !ok {
		return nil, fmt.Errorf("%w: signature out of bounds", ErrInvalidToken)
	}

	key, err := v.keyForKid(ctx, hdr.Kid)
	if err != nil {
		if errors.Is(err, errUnknownKid) {
			return nil, fmt.Errorf("%w: unknown signing key", ErrInvalidToken)
		}
		return nil, err // ErrUnavailable class, already wrapped
	}
	if !keyMatchesAlg(key, hdr.Alg) {
		return nil, fmt.Errorf("%w: key type does not match algorithm", ErrInvalidToken)
	}
	jws, err := jose.ParseSigned(bearer, allowedJoseAlgs(v.cfg.AllowedAlgorithms))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	payload, err := jws.Verify(key)
	if err != nil {
		return nil, fmt.Errorf("%w: signature verification failed", ErrInvalidToken)
	}
	sub, err := v.checkClaims(payload)
	if err != nil {
		return nil, err
	}
	return &Claims{Issuer: v.cfg.IssuerURL, Subject: sub}, nil
}

func allowedJoseAlgs(algs []string) []jose.SignatureAlgorithm {
	out := make([]jose.SignatureAlgorithm, 0, len(algs))
	for _, a := range algs {
		out = append(out, jose.SignatureAlgorithm(a))
	}
	return out
}

// keyMatchesAlg pins the JWK's key type to the token's declared
// algorithm so an RS256 header can never verify against an EC key or
// vice versa.
func keyMatchesAlg(key jose.JSONWebKey, alg string) bool {
	switch alg {
	case "RS256":
		_, ok := key.Key.(*rsa.PublicKey)
		return ok
	case "ES256":
		ec, ok := key.Key.(*ecdsa.PublicKey)
		return ok && ec.Curve == elliptic.P256()
	default:
		return false
	}
}

// checkClaims walks the verified payload's top-level claims strictly —
// bounded member count, bounded ASCII names, no duplicates — then
// enforces the full policy. It returns the token's subject.
func (v *Verifier) checkClaims(payload []byte) (string, error) {
	claims, err := parseTopLevelClaims(payload)
	if err != nil {
		return "", err
	}

	// Forbidden claims reject on presence, even as null.
	for _, name := range v.cfg.ForbiddenClaims {
		if _, present := claims[name]; present {
			return "", fmt.Errorf("%w: forbidden claim present", ErrInvalidToken)
		}
	}

	iss, err := stringClaim(claims, "iss", MaxIssuerCodePoints, MaxIssuerBytes)
	if err != nil {
		return "", err
	}
	// Byte-for-byte issuer comparison; no alias or trailing-slash
	// normalization.
	if iss != v.cfg.IssuerURL {
		return "", fmt.Errorf("%w: issuer mismatch", ErrInvalidToken)
	}
	aud, err := stringClaim(claims, "aud", MaxAudienceCodePoints, MaxAudienceBytes)
	if err != nil {
		// A JSON array aud is rejected here even when it contains the
		// configured audience — the claim must be a single string.
		return "", err
	}
	if aud != v.cfg.Audience {
		return "", fmt.Errorf("%w: audience mismatch", ErrInvalidToken)
	}
	azp, err := stringClaim(claims, "azp", MaxAzpCodePoints, MaxAzpBytes)
	if err != nil {
		return "", err
	}
	if azp != v.cfg.AuthorizedParty {
		return "", fmt.Errorf("%w: authorized party mismatch", ErrInvalidToken)
	}
	scope, err := stringClaim(claims, "scope", MaxScopeBytes, MaxScopeBytes)
	if err != nil {
		return "", err
	}
	// Exact singleton scope: the whole claim equals the one configured
	// token — a set containing it does not pass.
	if !boundedASCII(scope, MaxScopeBytes) || scope != v.cfg.RequiredScope {
		return "", fmt.Errorf("%w: scope mismatch", ErrInvalidToken)
	}

	iat, err := integerClaim(claims, "iat")
	if err != nil {
		return "", err
	}
	exp, err := integerClaim(claims, "exp")
	if err != nil {
		return "", err
	}
	now := v.now().Unix()
	skew := int64(v.cfg.ClockSkew / time.Second)
	if iat > now+skew {
		return "", fmt.Errorf("%w: issued in the future", ErrInvalidToken)
	}
	if exp+skew <= now {
		return "", fmt.Errorf("%w: expired", ErrInvalidToken)
	}
	if exp <= iat {
		return "", fmt.Errorf("%w: exp not after iat", ErrInvalidToken)
	}
	// Unsigned subtraction is exact for any int64 pair with exp > iat, so
	// an extreme iat cannot overflow the lifetime check.
	if uint64(exp)-uint64(iat) > uint64(v.cfg.MaxTokenLifetime/time.Second) {
		return "", fmt.Errorf("%w: lifetime over limit", ErrInvalidToken)
	}

	sub, err := stringClaim(claims, "sub", MaxSubjectCodePoints, MaxSubjectBytes)
	if err != nil {
		return "", err
	}
	if _, err := stringClaim(claims, "jti", MaxSubjectCodePoints, MaxSubjectBytes); err != nil {
		return "", err
	}
	for _, rc := range v.cfg.RequiredClaims {
		val, err := stringClaim(claims, rc.Name, MaxSubjectCodePoints, MaxSubjectBytes)
		if err != nil {
			return "", err
		}
		if len(rc.AllowedValues) > 0 {
			allowed := false
			for _, av := range rc.AllowedValues {
				if val == av {
					allowed = true
					break
				}
			}
			if !allowed {
				return "", fmt.Errorf("%w: claim value not allowed", ErrInvalidToken)
			}
		}
	}
	return sub, nil
}

// parseTopLevelClaims strictly decodes the claims object: exactly one
// top-level JSON object, at most MaxTopLevelClaims members, each name
// bounded ASCII (no C0 controls, no non-ASCII) and unique.
func parseTopLevelClaims(payload []byte) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("%w: claims not valid JSON", ErrInvalidToken)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, fmt.Errorf("%w: claims not a JSON object", ErrInvalidToken)
	}
	claims := make(map[string]json.RawMessage)
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("%w: claims not valid JSON", ErrInvalidToken)
		}
		name, ok := keyTok.(string)
		if !ok || !boundedASCII(name, MaxClaimNameBytes) {
			return nil, fmt.Errorf("%w: claim name out of bounds", ErrInvalidToken)
		}
		if _, dup := claims[name]; dup {
			return nil, fmt.Errorf("%w: duplicate claim", ErrInvalidToken)
		}
		if len(claims) == MaxTopLevelClaims {
			return nil, fmt.Errorf("%w: too many claims", ErrInvalidToken)
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, fmt.Errorf("%w: claims not valid JSON", ErrInvalidToken)
		}
		claims[name] = raw
	}
	if _, err := dec.Token(); err != nil { // consume the closing brace
		return nil, fmt.Errorf("%w: claims not valid JSON", ErrInvalidToken)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("%w: trailing data after claims", ErrInvalidToken)
	}
	return claims, nil
}

// stringClaim extracts a required claim that must be a JSON string —
// never an object, array, number, bool, or null — nonempty and within
// both the code-point and byte limits.
func stringClaim(claims map[string]json.RawMessage, name string, maxCodePoints, maxBytes int) (string, error) {
	raw, present := claims[name]
	if !present {
		return "", fmt.Errorf("%w: missing claim %s", ErrInvalidToken, name)
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '"' {
		return "", fmt.Errorf("%w: claim %s is not a string", ErrInvalidToken, name)
	}
	var s string
	if err := json.Unmarshal(trimmed, &s); err != nil {
		return "", fmt.Errorf("%w: claim %s is not a string", ErrInvalidToken, name)
	}
	if !boundedUTF8(s, maxCodePoints, maxBytes) {
		return "", fmt.Errorf("%w: claim %s out of bounds", ErrInvalidToken, name)
	}
	return s, nil
}

// integerClaim extracts a required NumericDate claim as a finite integer
// number of seconds within the signed 64-bit range. Fractions, exponent
// notation, and oversized literals are rejected before conversion.
func integerClaim(claims map[string]json.RawMessage, name string) (int64, error) {
	raw, present := claims[name]
	if !present {
		return 0, fmt.Errorf("%w: missing claim %s", ErrInvalidToken, name)
	}
	var n json.Number
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] == '"' || trimmed[0] == '{' || trimmed[0] == '[' {
		return 0, fmt.Errorf("%w: claim %s is not a number", ErrInvalidToken, name)
	}
	if err := json.Unmarshal(trimmed, &n); err != nil {
		return 0, fmt.Errorf("%w: claim %s is not a number", ErrInvalidToken, name)
	}
	s := n.String()
	if strings.ContainsAny(s, ".eE") {
		return 0, fmt.Errorf("%w: claim %s is not an integer", ErrInvalidToken, name)
	}
	val, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: claim %s out of range", ErrInvalidToken, name)
	}
	return val, nil
}
