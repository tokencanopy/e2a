// Package limits enforces per-user resource caps at the agent-create,
// domain-register, and message-send paths.
//
// The OSS server is intentionally agnostic about what those caps mean.
// A row in account_limits is the contract; whatever populates it (the
// hosted-service sidecar, an admin tool, a self-host operator with SQL
// access) owns the semantics of plan_code and upgrade_url. The OSS
// server only enforces the integer caps and echoes the opaque label.
//
// When no row exists for a user, the enforcer falls back to the
// operator-configured Defaults (config.yaml `limits:` block). Self-host
// operators who do not want any cap can leave Defaults at MaxInt32.
package limits

import (
	"context"
	"errors"
	"fmt"
)

// Limits is the resolved per-user cap set: either read from
// account_limits or filled from operator Defaults when no row exists.
type Limits struct {
	PlanCode         string `json:"plan_code"`
	MaxAgents        int    `json:"max_agents"`
	MaxDomains       int    `json:"max_domains"`
	MaxMessagesMonth int    `json:"max_messages_month"`
	MaxStorageBytes  int64  `json:"max_storage_bytes"`
	UpgradeURL       string `json:"upgrade_url"`
	// OutboundFooterEnabled is a feature entitlement, not a cap: whether
	// outbound mail from this account carries the operator-configured
	// footer (config `outbound_footer:` block; the feature's master switch
	// lives there, not here). Like PlanCode, the semantics of who gets the
	// footer are owned by whatever provisions the row. Excluded from JSON
	// so the public limits payloads (GET /v1/account, the 402 envelope)
	// are unchanged.
	OutboundFooterEnabled bool `json:"-"`
}

// Defaults is the operator-configured fallback applied when a user has
// no account_limits row. It is also returned verbatim from Get for
// brand-new users so the dashboard has something to render.
type Defaults struct {
	PlanCode         string
	MaxAgents        int
	MaxDomains       int
	MaxMessagesMonth int
	MaxStorageBytes  int64
	// OutboundFooterEnabled is the row-less fallback for the outbound
	// footer entitlement, wired from `outbound_footer.default_enabled`.
	OutboundFooterEnabled bool
}

// Enforcer is the interface that handlers call. Implementations must be
// safe for concurrent use.
type Enforcer interface {
	// Get returns the resolved Limits for the user. Never returns
	// ErrLimitExceeded; that error is reserved for the Check methods.
	Get(ctx context.Context, userID string) (Limits, error)

	// CheckAgentCreate returns nil if the user may create another agent,
	// or *LimitExceededError if they have already hit the cap.
	CheckAgentCreate(ctx context.Context, userID string) error

	// CheckDomainCreate returns nil if the user may register another
	// domain, or *LimitExceededError if they have already hit the cap.
	CheckDomainCreate(ctx context.Context, userID string) error

	// CheckMessageSend returns nil if the user may send/receive another
	// message this calendar month, or *LimitExceededError if they have
	// already hit the cap. Counts inbound+outbound in the current UTC
	// month against MaxMessagesMonth.
	CheckMessageSend(ctx context.Context, userID string) error

	// Invalidate evicts the user's cached Limits so the next Get/Check
	// re-reads from the database. Called by the limits-invalidate HTTP
	// endpoint when an external writer (e.g. billing sidecar) has just
	// updated account_limits.
	Invalidate(userID string)
}

// LimitExceededError is returned by Check* methods when the user has
// reached a cap. Handlers convert it to HTTP 402 with the Limits payload
// so the dashboard can show the current cap and any upgrade affordance.
type LimitExceededError struct {
	// Resource is the AccountView usage/limits field stem the client can
	// key the error to: "agents" | "domains" | "messages_month" |
	// "storage_bytes". It matches usage.<resource> and limits.max_<resource>.
	Resource string
	Limit    int // the cap that was hit
	Current  int // the user's current count (counts vary by resource)
	Limits   Limits // full resolved limits for upgrade-URL rendering
}

func (e *LimitExceededError) Error() string {
	return fmt.Sprintf("limits: %s cap reached (%d/%d)", e.Resource, e.Current, e.Limit)
}

// IsLimitExceeded reports whether err is a *LimitExceededError, and
// returns it if so. Callers convert the typed error to HTTP 402.
func IsLimitExceeded(err error) (*LimitExceededError, bool) {
	var le *LimitExceededError
	if errors.As(err, &le) {
		return le, true
	}
	return nil, false
}
