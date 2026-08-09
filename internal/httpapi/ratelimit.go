package httpapi

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/usage"
)

// RateSnapshot records an attempt for key and reports whether it is allowed
// along with the IETF RateLimit header values (quota, remaining-after-this,
// seconds-until-reset) and the Retry-After delay when blocked. It mirrors
// ratelimit.Limiter.AllowSnapshot so httpapi shares the exact buckets the
// legacy surface uses without importing the limiter directly.
type RateSnapshot func(key string) (ok bool, retryAfter time.Duration, limit, remaining, resetSeconds int)

// pollLimitedOps are the authenticated read operations governed by the
// per-user poll limiter. This mirrors EXACTLY the set the legacy gorilla/mux
// surface poll-limited (verified against origin/main: handleGetMessages,
// handleGetMessage, handleListConversations, handleGetConversation,
// handleListWebhooks, handleGetWebhook, handleListWebhookDeliveries — the
// label PATCH stays legacy-only). The legacy surface deliberately did NOT
// poll-limit agents/domains/events/limits/export reads, so neither do we:
// notably the events API is built for reconciliation polling and must not
// compete for the shared per-user message-read budget. getInfo is public (no
// principal to key on).
var pollLimitedOps = map[string]bool{
	"listMessages": true, "getMessage": true,
	"listConversations": true, "getConversation": true,
	"listWebhooks": true, "getWebhook": true, "listWebhookDeliveries": true,
}

// destructiveOps are mutations whose server-side cost is unbounded by the
// request: the row named in the path is cheap, but the cascade behind it scales
// with everything the caller has ever accumulated under it. Nothing else in the
// API lets one call do an unbounded amount of write work.
//
// These are governed by a per-account CONCURRENCY cap rather than a rate
// limiter. The hazard is not request frequency — it is several expensive
// cascades running at once, each holding a pooled connection while it works.
// Six concurrent permanent agent deletes were enough to saturate the pool,
// starve the readiness probe, and take an entire slot out of the load balancer.
// A rate limiter sized to allow legitimate bulk cleanup would not have stopped
// that; a concurrency cap does, while still letting a caller delete as much as
// they like sequentially.
var destructiveOps = map[string]bool{
	"deleteAgent":  true,
	"deleteDomain": true,
}

// maxConcurrentDestructive is the per-account in-flight ceiling for
// destructiveOps. Two keeps sequential cleanup (the e2e harness, a customer
// draining an account) at full speed while bounding the blast radius.
const maxConcurrentDestructive = 2

// acquireDestructive reserves an in-flight slot for userID, reporting false
// when the account is already at its ceiling.
func (s *Server) acquireDestructive(userID string) bool {
	v, _ := s.destructiveInFlight.LoadOrStore(userID, new(int64))
	n := v.(*int64)
	if atomic.AddInt64(n, 1) > maxConcurrentDestructive {
		atomic.AddInt64(n, -1)
		return false
	}
	return true
}

func (s *Server) releaseDestructive(userID string) {
	if v, ok := s.destructiveInFlight.Load(userID); ok {
		atomic.AddInt64(v.(*int64), -1)
	}
}

// rateLimit is the Huma middleware that enforces the per-user poll limiter on
// reads and the per-IP registration limiter on agent create, and stamps the
// IETF RateLimit-Limit/Remaining/Reset headers (plus Retry-After on a 429) on
// the response. The per-agent SEND limiter is enforced inside the outbound
// handlers instead: its key is the *resolved owned* agent (after the
// resolveOwnedAgent ownership check), which this middleware doesn't perform —
// so the send limit is applied in deliver()/the outbound handlers, not here.
func (s *Server) rateLimit(ctx huma.Context, next func(huma.Context)) {
	op := ctx.Operation()
	if op == nil {
		next(ctx)
		return
	}

	var snap RateSnapshot
	var key string
	switch {
	case pollLimitedOps[op.OperationID] && s.deps.PollLimit != nil:
		r := RequestFromContext(ctx.Context())
		if r == nil || s.deps.Authenticator == nil {
			next(ctx)
			return
		}
		p, err := s.resolvePrincipal(r)
		if err != nil {
			// Unauthenticated: let the handler emit the canonical 401 rather
			// than masking a missing credential as a rate-limit decision.
			next(ctx)
			return
		}
		// Reuse the principal so the handler does not authenticate a second
		// time on the hot read path.
		ctx = huma.WithContext(ctx, withPrincipal(ctx.Context(), p))
		// Trusted internal traffic (system probes, internal dogfooding /
		// conformance) bypasses the limiter — same policy axis as metering.
		if !usage.RateLimited(usage.AccountClass(p.User.AccountClass)) {
			next(ctx)
			return
		}
		snap, key = s.deps.PollLimit, p.User.ID
	case op.OperationID == "createAgent" && s.deps.RegLimit != nil:
		r := RequestFromContext(ctx.Context())
		if r == nil {
			next(ctx)
			return
		}
		// Exempt trusted internal classes (system/internal) from the per-IP
		// registration limiter, reusing the resolved principal downstream.
		// Resolving here costs a keyed api_keys touch even on an over-cap
		// attempt (origin/main rejected those IP-only at zero DB cost); accepted
		// because the exemption must be bucket-independent (behind a proxy the
		// per-IP bucket is shared), the caller already holds a valid credential,
		// the write lands only on the caller's OWN key row, and registration is
		// low-QPS. Branch on the resolve error rather than pre-checking a
		// specific authenticator field — resolvePrincipal prefers
		// PrincipalAuthenticator and errors when neither is wired, so an
		// unauthenticated/unresolvable create simply falls through to the
		// limiter and the handler emits the canonical 401.
		if p, err := s.resolvePrincipal(r); err == nil {
			ctx = huma.WithContext(ctx, withPrincipal(ctx.Context(), p))
			if !usage.RateLimited(usage.AccountClass(p.User.AccountClass)) {
				next(ctx)
				return
			}
		}
		snap, key = s.deps.RegLimit, clientIP(r)
	case destructiveOps[op.OperationID]:
		r := RequestFromContext(ctx.Context())
		if r == nil {
			next(ctx)
			return
		}
		p, err := s.resolvePrincipal(r)
		if err != nil {
			// Unauthenticated: let the handler emit the canonical 401 rather
			// than masking a missing credential as a concurrency decision.
			next(ctx)
			return
		}
		ctx = huma.WithContext(ctx, withPrincipal(ctx.Context(), p))
		// Deliberately NOT exempt for ClassSystem/ClassInternal, unlike every
		// other limiter here. Those exemptions exist because the rate limiters
		// bound user abuse, and trusted first-party traffic is not abuse. This
		// cap is not about abuse — it protects the instance from itself, and
		// the outage that motivated it was caused by an internal-class client.
		// Exempting the classes most likely to run bulk operations would leave
		// the actual failure mode wide open.
		if !s.acquireDestructive(p.User.ID) {
			ctx.SetHeader("Retry-After", "1")
			writeEnvelope(ctx, NewError(http.StatusTooManyRequests, "rate_limited",
				"too many concurrent delete operations for this account").
				WithDetails(map[string]any{
					"retry_after_seconds": 1,
					"max_concurrent":      maxConcurrentDestructive,
				}))
			return
		}
		defer s.releaseDestructive(p.User.ID)
		next(ctx)
		return
	default:
		next(ctx)
		return
	}

	ok, retryAfter, limit, remaining, reset := snap(key)
	ctx.SetHeader("RateLimit-Limit", strconv.Itoa(limit))
	ctx.SetHeader("RateLimit-Remaining", strconv.Itoa(remaining))
	ctx.SetHeader("RateLimit-Reset", strconv.Itoa(reset))
	if ok {
		next(ctx)
		return
	}
	secs := int(retryAfter.Round(time.Second).Seconds())
	if secs < 1 {
		secs = 1
	}
	ctx.SetHeader("Retry-After", strconv.Itoa(secs))
	writeEnvelope(ctx, NewError(http.StatusTooManyRequests, "rate_limited",
		"rate limit exceeded").WithDetails(map[string]any{"retry_after_seconds": secs}))
}

// writeEnvelope serializes an error envelope directly to the response from a
// middleware, where there is no handler return value for Huma to render. It
// stamps the request id so the body matches the stampRequestID transformer on
// the handler path, and sets headers before the status line.
func writeEnvelope(ctx huma.Context, env *ErrorEnvelope) {
	env.Err.RequestID = RequestIDFromContext(ctx.Context())
	ctx.SetHeader("Content-Type", "application/json")
	ctx.SetStatus(env.status)
	_ = json.NewEncoder(ctx.BodyWriter()).Encode(env)
}

// principalCtxKey carries a principal resolved by the rate-limit middleware so
// the downstream handler's requireUser/requirePrincipal can skip a second auth.
type principalCtxKey struct{}

func withPrincipal(ctx context.Context, p *identity.Principal) context.Context {
	return context.WithValue(ctx, principalCtxKey{}, p)
}

func principalFromContext(ctx context.Context) *identity.Principal {
	if p, ok := ctx.Value(principalCtxKey{}).(*identity.Principal); ok {
		return p
	}
	return nil
}

func userFromContext(ctx context.Context) *identity.User {
	if p := principalFromContext(ctx); p != nil {
		return p.User
	}
	return nil
}

// clientIP extracts the caller IP for per-IP limiting. It trusts only
// CF-Connecting-IP (set by Cloudflare and stripped from inbound requests
// at the edge), NOT X-Forwarded-For: an attacker can prepend an arbitrary
// XFF entry and Cloudflare appends the real client IP rather than
// replacing it, so the leftmost XFF value — which this used to key on —
// is fully client-controlled and lets one attacker mint unlimited per-IP
// budget on every limiter (agent registration, attachment-token
// throttle, feedback). CF-Connecting-IP is spoofable only by someone who
// can reach the origin directly, so the origin must be firewalled to
// Cloudflare's ranges. Falls back to RemoteAddr (dev / non-CF) but never
// to XFF. Mirrors agent.clientIP and oauth dcrSourceIP so every per-IP
// limiter keys identically.
func clientIP(r *http.Request) string {
	if ip := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); ip != "" {
		return ip
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
