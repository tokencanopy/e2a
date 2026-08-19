// Package senderidentity manages the per-domain SES sending identity that
// lets outbound mail use the agent's OWN address as the From header
// (decision 4 / Slice 4). Verification is asynchronous: a domain moves
// none → pending → verified|failed, driven by a River-backed provision
// job + a periodic reconciler. The own-address From is used ONLY when the
// domain reaches `verified` (fail-closed); every other state falls back to
// the relay From, so the whole subsystem is behavior-neutral until a
// Provider actually verifies a domain.
//
// The Provider abstraction keeps the AWS SES SDK at the edge: the workers,
// store, and handlers speak this interface, and tests use the in-memory
// fake. The real sesv2 implementation (ses.go) is exercised only against
// live AWS; everything else is unit/integration tested with the fake.
package senderidentity

import (
	"context"
	"errors"
)

// Status is the verification state of a domain's sending identity. It maps
// 1:1 onto the domains.sending_status column.
type Status string

const (
	// StatusNone — no sending identity registered. Default for every
	// domain; self-host / SES-not-configured deployments stay here, which
	// keeps outbound on the relay From (fail-closed).
	StatusNone Status = "none"
	// StatusPending — identity registered with SES (BYODKIM), awaiting
	// asynchronous verification. The reconciler polls until it resolves.
	StatusPending Status = "pending"
	// StatusVerified — SES confirmed the identity; own-address From is now
	// used for this domain's agents.
	StatusVerified Status = "verified"
	// StatusFailed — verification failed, or `pending` exceeded its TTL.
	// Carries an actionable reason; outbound stays on the relay From.
	StatusFailed Status = "failed"
)

// Valid reports whether s is one of the four known states.
func (s Status) Valid() bool {
	switch s {
	case StatusNone, StatusPending, StatusVerified, StatusFailed:
		return true
	}
	return false
}

// DNSRecord is a single record the customer must publish for the sending
// identity. With BYODKIM the customer already published the per-domain DKIM
// record during register/verify; this now carries the custom MAIL FROM
// subdomain's MX + SPF records (Return-Path alignment — see ses.go
// mailFromRecords) and surfaces anything SES reports as still-required.
type DNSRecord struct {
	Type  string `json:"type"`  // "TXT" | "CNAME" | "MX"
	Name  string `json:"name"`  // record host
	Value string `json:"value"` // record value
}

// Result is what a Provider reports for a domain.
//
// Status is the all-or-nothing rollup (mapSESStatus): `verified` only when
// EVERY sending axis is good. DkimStatus and MailFromStatus are the per-axis
// breakdown SES reports independently (DkimAttributes.Status and
// MailFromAttributes.MailFromDomainStatus), so a domain with good DKIM but a
// broken custom MAIL FROM surfaces as DkimStatus=verified + MailFromStatus=failed
// while the rollup Status stays `failed`. They are empty ("") when the provider
// has no per-axis signal (e.g. Provision, which only registers the identity);
// consumers fall back to the rollup in that case.
type Result struct {
	Status         Status      `json:"status"`
	DkimStatus     Status      `json:"dkim_status,omitempty"`
	MailFromStatus Status      `json:"mail_from_status,omitempty"`
	Error          string      `json:"error,omitempty"`
	DNSRecords     []DNSRecord `json:"dns_records,omitempty"`
}

// ErrIdentityNotFound is what Status returns when the provider has no
// identity for the domain (e.g. it was deprovisioned out of band). Callers
// treat it as "drop back to none/failed", never as a hard error.
var ErrIdentityNotFound = errors.New("senderidentity: identity not found")

// ErrIdentityNotOwned means an identity exists for the domain but lacks the
// provider-side ownership marker written by e2a, AND does not qualify for
// adoption (see Provider.Provision). Callers must never update or delete it:
// an SES account can be shared with other applications.
var ErrIdentityNotOwned = errors.New("senderidentity: identity is not managed by e2a")

// AdoptionEvidence is what a caller has on file for a domain, used to judge
// whether an untagged existing provider identity is provably e2a's own (see
// the SES implementation's canAdoptIdentity for the precise criteria).
// Selector and HasPrivateKey must be JOINTLY consistent — a caller's stored
// state can carry a selector with no private key on file (e.g. mid a domain
// reclaim), and reporting HasPrivateKey=true in that case would make Status
// tag an identity e2a cannot actually sign for. Bundling the two into one
// value (batch C finding 5) replaces what used to be two independent
// parameters that existed only to feed a single joint decision — a caller
// could pass them inconsistently (e.g. a non-empty Selector with
// HasPrivateKey hardcoded true) and nothing but code review would catch it,
// since both are the same primitive type.
type AdoptionEvidence struct {
	Selector      string
	HasPrivateKey bool
}

// Provider registers, polls, and removes the upstream (SES) sending
// identity for a domain. Implementations MUST be idempotent for identities they
// own: Provision on an already-managed domain refreshes desired state, and
// Deprovision treats a missing identity as success. An existing identity that
// lacks the provider ownership marker is ADOPTED (tagged and then treated as
// owned) when it is provably e2a's own — an untagged identity configured with
// e2a's own BYODKIM selector for that exact domain (see the SES implementation
// for the precise criteria) — and otherwise returns ErrIdentityNotOwned and
// must not be mutated. Adoption exists because every identity created before
// the ownership tag shipped is untagged, and a naive "no tag means foreign"
// rule would permanently strand every pre-existing customer domain.
type Provider interface {
	// Provision registers a BYODKIM sending identity for domain, supplying
	// the per-domain DKIM selector + PKCS#1 DER private key that e2a
	// already generated (so DKIM d= aligns with the From domain). An
	// existing untagged identity is adopted in place when it provably
	// matches this selector (see Provider doc); otherwise returns
	// ErrIdentityNotOwned. Returns the initial Result — typically
	// StatusPending — or an error to retry.
	Provision(ctx context.Context, domain, dkimSelector string, dkimPrivateKeyDER []byte) (Result, error)

	// Status polls the current verification state from the provider.
	// evidence is what e2a has on file for domain (see AdoptionEvidence) —
	// Status needs it to make the same adoption judgement as Provision
	// (including self-healing an ownership tag that was removed out-of-band
	// on an identity e2a otherwise still provably owns). Callers that have no
	// adoption judgement to make for this poll (no selector, or a selector
	// without key material) should pass a zero AdoptionEvidence; this never
	// affects polling an ALREADY-owned identity, whose status reporting
	// doesn't consult it. Returns ErrIdentityNotFound if no identity exists
	// for domain.
	Status(ctx context.Context, domain string, evidence AdoptionEvidence) (Result, error)

	// Deprovision removes the sending identity. A missing identity MUST be
	// reported as success (idempotent teardown).
	Deprovision(ctx context.Context, domain string) error

	// List returns all domain identities visible to the provider principal.
	// It is retained for the phase-1 compatibility worker only; the v2 reaper
	// uses ListPage so one River job cannot inventory the whole provider account.
	// Neither path treats an unledgered identity as its own.
	List(ctx context.Context) ([]string, error)

	// ListPage returns one provider-bounded page plus the opaque continuation
	// token. The v2 orphan audit uses this so one River job never inventories
	// the whole provider account.
	ListPage(ctx context.Context, nextToken string, limit int) (domains []string, followingToken string, err error)
}
