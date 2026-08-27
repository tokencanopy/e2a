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
			if keys, ferr := v.fetchJWKS(jwksURI); ferr == nil {
				v.cache.mu.Lock()
				v.cache.keys = keys
				v.cache.lastSuccess = v.now()
				v.cache.mu.Unlock()
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
func (v *Verifier) keyForKid(ctx context.Context, kid string) (jose.JSONWebKey, error) {
	uriPtr := v.jwksURI.Load()
	if uriPtr == nil {
		return jose.JSONWebKey{}, fmt.Errorf("%w: issuer not yet discovered", ErrUnavailable)
	}
	now := v.now()
	v.cache.mu.Lock()
	key, known := v.cache.keys[kid]
	withinGrace := !v.cache.lastSuccess.IsZero() &&
		now.Sub(v.cache.lastSuccess) <= KeysFreshFor+KeysStaleGrace
	v.cache.mu.Unlock()
	if known && withinGrace {
		return key, nil
	}
	// Unknown kid (any cache age) or a cache past the stale grace: one
	// singleflight, rate-limited refresh decides the request's fate. An
	// unknown key is never verified from stale state.
	if err := v.refresh(ctx, *uriPtr); err != nil {
		return jose.JSONWebKey{}, err
	}
	v.cache.mu.Lock()
	key, known = v.cache.keys[kid]
	v.cache.mu.Unlock()
	if !known {
		v.emitRefresh("key_absent")
		return jose.JSONWebKey{}, errUnknownKid
	}
	return key, nil
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

	v.cache.mu.Lock()
	v.cache.lastAttemptErr = ferr
	if ferr != nil {
		v.cache.cooldownUntil = v.now().Add(RefreshCooldown)
		if errors.Is(ferr, errJWKSMalformed) {
			v.emitRefresh("parse_error")
		} else {
			v.emitRefresh("transport_error")
		}
	} else {
		v.cache.keys = keys
		v.cache.lastSuccess = v.now()
		v.emitRefresh("success")
	}
	v.cache.refreshing = nil
	close(ch)
	v.cache.mu.Unlock()
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
