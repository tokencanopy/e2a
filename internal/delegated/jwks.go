package delegated

import (
	"context"
	"crypto/ecdsa"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v4"
)

// oidcClientContext threads the verifier's HTTP client into go-oidc's
// discovery fetch.
func oidcClientContext(ctx context.Context, c *http.Client) context.Context {
	return oidc.ClientContext(ctx, c)
}

// maxDiscoveryBytes caps the OIDC discovery document go-oidc reads for
// us. The JWKS fetch has its own explicit MaxJWKSBytes cap; this bounds
// the one response go-oidc reads on our behalf so an oversized (or
// hostile) discovery document cannot be buffered unbounded.
const maxDiscoveryBytes = 65536

// errResponseTooLarge is returned by a capped body once it delivers more
// than its byte budget, surfacing as a read error to whatever is reading
// the response (here, go-oidc's discovery parser).
var errResponseTooLarge = errors.New("delegated: response body over cap")

// cappingTransport bounds the response body of every request it carries.
// It is used only for the discovery client; the JWKS fetch caps its own
// body with an explicit io.LimitReader.
type cappingTransport struct {
	base http.RoundTripper
	max  int64
}

func (t *cappingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	resp.Body = &cappedBody{inner: resp.Body, max: t.max}
	return resp, nil
}

// cappedBody fails a read once the cumulative bytes read exceed max, so a
// body of at most max bytes reads to EOF normally while a larger one
// surfaces errResponseTooLarge instead of being silently truncated.
type cappedBody struct {
	inner io.ReadCloser
	max   int64
	read  int64
}

func (b *cappedBody) Read(p []byte) (int, error) {
	n, err := b.inner.Read(p)
	b.read += int64(n)
	if b.read > b.max {
		return n, errResponseTooLarge
	}
	return n, err
}

func (b *cappedBody) Close() error { return b.inner.Close() }

// discoveryClient wraps c with a body-capping transport for the OIDC
// discovery fetch. c is the verifier's own client (its timeout is
// preserved); nil falls back to a default client.
func discoveryClient(c *http.Client) *http.Client {
	var base http.RoundTripper
	timeout := FetchTimeout
	if c != nil {
		base = c.Transport
		if c.Timeout > 0 {
			timeout = c.Timeout
		}
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: &cappingTransport{base: base, max: maxDiscoveryBytes},
	}
}

// RefreshMetrics is the narrow observability seam for key-refresh
// outcomes. telemetry.Metrics satisfies it; nil disables emission.
// Outcome values are a closed set: success, key_absent, transport_error,
// parse_error, rate_limited — never key material, kids, issuer response
// text, or token data.
type RefreshMetrics interface {
	DelegatedJWKSRefresh(outcome string)
}

// Discovery retry pacing (mirrors the OIDC login client's background
// loop: issuer network unavailability must never be startup-fatal).
const (
	discoveryInitialBackoff = time.Second
	discoveryMaxBackoff     = time.Minute
	discoveryFailureLogGap  = time.Minute
)

// errUnknownKid is the post-refresh "the issuer does not publish this
// kid" outcome. Unlike transport failures it is a property of the token,
// so Verify maps it to ErrInvalidToken.
var errUnknownKid = errors.New("delegated: unknown signing key")

// keyCache is the per-issuer key state behind the exact refresh policy:
// atomic replacement, fresh/stale windows, one singleflight refresh, a
// burst-1 token bucket refilling RefreshPerWindow per RefreshWindow, and
// a negative-result cooldown.
type keyCache struct {
	mu sync.Mutex
	// keys is the last good set, replaced atomically on a successful
	// refresh — a failed refresh never clears or mutates it.
	keys        map[string]jose.JSONWebKey
	lastSuccess time.Time
	// refreshing is non-nil while one fetch is in flight; followers wait
	// on it and share the leader's outcome instead of fetching.
	refreshing     chan struct{}
	lastAttemptErr error
	cooldownUntil  time.Time
	tokens         float64
	lastRefill     time.Time
}

func (v *Verifier) emitRefresh(outcome string) {
	if v.metrics != nil {
		v.metrics.DelegatedJWKSRefresh(outcome)
	}
}

// discoverWithRetry runs on its own goroutine for the verifier's
// lifetime. go-oidc's NewProvider enforces that the discovery document's
// issuer exactly equals the configured issuer. On success it publishes
// the jwks_uri and primes the key cache with one direct fetch (outside
// the request-path rate bucket), then returns.
func (v *Verifier) discoverWithRetry(ctx context.Context) {
	if v.discoveryDone != nil {
		defer close(v.discoveryDone)
	}
	backoff := discoveryInitialBackoff
	var lastLogged time.Time
	attempt := 0
	for {
		attempt++
		attemptCtx, cancel := context.WithTimeout(ctx, FetchTimeout)
		provider, err := oidc.NewProvider(attemptCtx, v.cfg.IssuerURL)
		var jwksURI string
		if err == nil {
			var meta struct {
				JWKSURI string `json:"jwks_uri"`
			}
			if cerr := provider.Claims(&meta); cerr != nil || meta.JWKSURI == "" {
				err = fmt.Errorf("discovery document has no usable jwks_uri")
			} else {
				jwksURI = meta.JWKSURI
			}
		}
		cancel()
		if err == nil {
			v.jwksURI.Store(&jwksURI)
			log.Printf("[delegated] issuer discovered: %s", v.cfg.IssuerURL)
			// Prime the cache best-effort; a failure here is just the
			// cold-cache state the request path already handles.
			primed := false
			if keys, ferr := v.fetchJWKS(jwksURI); ferr == nil {
				v.cache.mu.Lock()
				v.cache.keys = keys
				v.cache.lastSuccess = v.now()
				v.cache.mu.Unlock()
				primed = true
			}
			if primed {
				v.emitRefresh("success")
			}
			return
		}
		if ctx.Err() != nil {
			return
		}
		if attempt == 1 || time.Since(lastLogged) >= discoveryFailureLogGap {
			// Log the issuer we configured, never the remote response body.
			log.Printf("[delegated] issuer discovery failed, retrying in background (attempt %d, issuer %s)", attempt, v.cfg.IssuerURL)
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
		if backoff > discoveryMaxBackoff {
			backoff = discoveryMaxBackoff
		}
	}
}

// keyForKid resolves a protected-header kid to a cached public key under
// the exact cache policy. Errors are either ErrUnavailable (cooldown,
// rate limit, transport/parse failure, discovery not ready, cache too
// stale) or errUnknownKid (a successful refresh proves the issuer does
// not publish this kid).
//
// The cache has three regimes for a KNOWN kid, keyed on the age since the
// last successful keyset fetch:
//
//   - fresh (age <= KeysFreshFor): served directly, no refresh — the
//     600s window §10.7's key-retention math turns over on;
//   - stale grace (KeysFreshFor < age <= KeysFreshFor+KeysStaleGrace): a
//     rate-limited singleflight refresh is ATTEMPTED; a known kid is
//     served only if that refresh fails (a removed/rotated-out key stops
//     verifying as soon as a refresh succeeds);
//   - expired (age > KeysFreshFor+KeysStaleGrace): even a known kid is
//     verifier-unavailable until a refresh succeeds.
//
// An UNKNOWN kid is never served from cache at any age: it always forces
// the one refresh, and a successful refresh without the key is 401.
func (v *Verifier) keyForKid(ctx context.Context, kid string) (jose.JSONWebKey, error) {
	uriPtr := v.jwksURI.Load()
	if uriPtr == nil {
		return jose.JSONWebKey{}, fmt.Errorf("%w: issuer not yet discovered", ErrUnavailable)
	}
	now := v.now()
	v.cache.mu.Lock()
	key, known := v.cache.keys[kid]
	haveGood := !v.cache.lastSuccess.IsZero()
	age := now.Sub(v.cache.lastSuccess)
	v.cache.mu.Unlock()

	// Fresh known kid: authoritative, no issuer contact.
	if known && haveGood && age <= KeysFreshFor {
		return key, nil
	}

	// Past the fresh window, or an unknown kid: attempt one singleflight,
	// rate-limited refresh. An unknown kid is never verified from stale
	// state, and a known kid past 600s must re-confirm against the issuer.
	refreshErr := v.refresh(ctx, *uriPtr)
	if refreshErr == nil {
		v.cache.mu.Lock()
		key, known = v.cache.keys[kid]
		v.cache.mu.Unlock()
		if !known {
			v.emitRefresh("key_absent")
			return jose.JSONWebKey{}, errUnknownKid
		}
		return key, nil
	}

	// Refresh failed. A previously-known kid may still serve, but only
	// inside the stale grace window; past it, or for an unknown kid, the
	// refresh failure stands (ErrUnavailable).
	if known && haveGood && age <= KeysFreshFor+KeysStaleGrace {
		return key, nil
	}
	return jose.JSONWebKey{}, refreshErr
}

// refresh performs (or joins) one JWKS refresh. The leader takes the
// cooldown/token-bucket gate, fetches with the fixed deadline, and either
// atomically installs the new set or arms the cooldown, retaining the
// last good set. Followers wait for the in-flight leader and share its
// outcome without fetching or emitting metrics.
func (v *Verifier) refresh(ctx context.Context, uri string) error {
	v.cache.mu.Lock()
	if ch := v.cache.refreshing; ch != nil {
		v.cache.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return fmt.Errorf("%w: %v", ErrUnavailable, ctx.Err())
		}
		v.cache.mu.Lock()
		err := v.cache.lastAttemptErr
		v.cache.mu.Unlock()
		if err != nil {
			return fmt.Errorf("%w: shared refresh failed", ErrUnavailable)
		}
		return nil
	}
	now := v.now()
	if now.Before(v.cache.cooldownUntil) {
		v.cache.mu.Unlock()
		v.emitRefresh("rate_limited")
		return fmt.Errorf("%w: refresh cooldown", ErrUnavailable)
	}
	// Token bucket: burst RefreshBurst, refill RefreshPerWindow per
	// RefreshWindow.
	if v.cache.lastRefill.IsZero() {
		v.cache.tokens = RefreshBurst
	} else {
		v.cache.tokens += now.Sub(v.cache.lastRefill).Seconds() *
			(float64(RefreshPerWindow) / RefreshWindow.Seconds())
		if v.cache.tokens > RefreshBurst {
			v.cache.tokens = RefreshBurst
		}
	}
	v.cache.lastRefill = now
	if v.cache.tokens < 1 {
		v.cache.mu.Unlock()
		v.emitRefresh("rate_limited")
		return fmt.Errorf("%w: refresh rate limited", ErrUnavailable)
	}
	v.cache.tokens--
	ch := make(chan struct{})
	v.cache.refreshing = ch
	v.cache.mu.Unlock()

	keys, ferr := v.fetchJWKS(uri)

	// Decide the outcome under the lock, but emit the metric after
	// unlocking — a Prometheus Inc must never run while cache.mu is held.
	var outcome string
	v.cache.mu.Lock()
	v.cache.lastAttemptErr = ferr
	if ferr != nil {
		v.cache.cooldownUntil = v.now().Add(RefreshCooldown)
		if errors.Is(ferr, errJWKSMalformed) {
			outcome = "parse_error"
		} else {
			outcome = "transport_error"
		}
	} else {
		v.cache.keys = keys
		v.cache.lastSuccess = v.now()
		outcome = "success"
	}
	v.cache.refreshing = nil
	close(ch)
	v.cache.mu.Unlock()
	v.emitRefresh(outcome)
	if ferr != nil {
		return fmt.Errorf("%w: key refresh failed", ErrUnavailable)
	}
	return nil
}

// errJWKSMalformed classifies a response that arrived but is unusable
// (bad JSON, over the caps, invalid kid) apart from transport failures.
var errJWKSMalformed = errors.New("delegated: malformed JWKS response")

// fetchJWKS fetches and strictly validates the issuer's keyset under the
// exact caps: FetchTimeout deadline, MaxJWKSBytes decoded body,
// MaxJWKSKeys accepted keys, per-key ASCII kid <= MaxKidBytes, unique
// kids, RSA/EC public keys only. It deliberately runs on a background
// context so one caller's disconnect cannot abort the shared refresh, and
// it never logs the response body.
func (v *Verifier) fetchJWKS(uri string) (map[string]jose.JSONWebKey, error) {
	ctx, cancel := context.WithTimeout(context.Background(), FetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
	if err != nil {
		return nil, err
	}
	resp, err := v.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jwks fetch: unexpected status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxJWKSBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > MaxJWKSBytes {
		return nil, fmt.Errorf("%w: response over %d bytes", errJWKSMalformed, MaxJWKSBytes)
	}
	var set jose.JSONWebKeySet
	if err := json.Unmarshal(body, &set); err != nil {
		return nil, fmt.Errorf("%w: %v", errJWKSMalformed, err)
	}
	keys := make(map[string]jose.JSONWebKey, len(set.Keys))
	for _, k := range set.Keys {
		switch k.Key.(type) {
		case *rsa.PublicKey, *ecdsa.PublicKey:
		default:
			// Not a verification key type this policy admits (private
			// keys, OKP, symmetric); skip rather than fail the set.
			continue
		}
		if !k.Valid() {
			return nil, fmt.Errorf("%w: invalid key entry", errJWKSMalformed)
		}
		if !boundedASCII(k.KeyID, MaxKidBytes) {
			return nil, fmt.Errorf("%w: key id out of bounds", errJWKSMalformed)
		}
		if _, dup := keys[k.KeyID]; dup {
			return nil, fmt.Errorf("%w: duplicate key id", errJWKSMalformed)
		}
		if len(keys) == MaxJWKSKeys {
			return nil, fmt.Errorf("%w: more than %d keys", errJWKSMalformed, MaxJWKSKeys)
		}
		keys[k.KeyID] = k
	}
	return keys, nil
}
