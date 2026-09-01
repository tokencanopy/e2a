package senderidentity

import (
	"fmt"
	"strings"
	"time"
)

// IdentityAudit is everything the reaper needs from the provider to judge ONE
// orphan candidate: the classification tags stamped at creation (tags.go) and
// whether the provider currently considers the identity able to SEND. Both
// come from a single GetEmailIdentity call (Provider.InspectIdentity), so a
// candidate costs one round trip and only orphans ever pay it.
//
// Tags is the raw key→value map exactly as the provider reports it: this type
// deliberately does no interpretation of its own, so every judgement lives in
// orphanReclaimable where it can be exhaustively table-tested.
type IdentityAudit struct {
	// Domain is the provider's identity NAME. It is the value matched against
	// the reclaim zones, not a value derived from e2a's database — the whole
	// point is to decide without trusting a join that may not exist.
	Domain string
	Tags   map[string]string
	// VerifiedForSending mirrors SES's VerifiedForSendingStatus: true means
	// this identity can send mail RIGHT NOW.
	VerifiedForSending bool
}

// ReclaimConfig is the operator's orphan-reclaim policy (config
// sender_identity.reap_orphans / reclaim_zones / reclaim_min_age /
// reclaim_max_per_sweep, plus the deployment name). Its zero value reclaims
// NOTHING: disarmed, no zones, no minimum age, no deletion budget. That is the
// only safe default for a subsystem whose failure mode is deleting a paying
// customer's ability to send mail.
type ReclaimConfig struct {
	// Enabled arms actual deletion. When false the reaper still runs the whole
	// decision and logs what it WOULD delete, but makes no provider mutation —
	// the observe-only mode an operator runs for days before arming.
	Enabled bool
	// Deployment is this deployment's configured name ("prod" | "staging"),
	// matched against the identity's e2a-env tag. Empty (an unnamed
	// deployment, the self-host default) reclaims nothing: without a name
	// there is no way to tell this deployment's fixtures from another
	// deployment's in a shared AWS account.
	Deployment string
	// Zones bounds reclaim to identity names at or under a DNS zone e2a's own
	// test fixtures live in. This is the strongest guard — a customer domain
	// is never under the test zone — and an EMPTY list means reclaim nothing,
	// never "any zone".
	Zones []string
	// MinAge is how old an identity must be (by its e2a-created stamp) before
	// it can be reclaimed, independent of its expiry tag. <= 0 reclaims
	// nothing: an unset floor is treated as an unconfigured policy, not as a
	// waiver.
	MinAge time.Duration
	// MaxPerSweep caps deletions per REAP JOB INVOCATION (see
	// reapProviderOrphanPage — the orphan phase is paginated across River
	// jobs, so this is deliberately a per-page budget and not a global one).
	// 0 reclaims nothing.
	MaxPerSweep int
}

// orphanReclaimable decides whether ONE provider identity that the reaper has
// already established is an orphan — present at the provider, with no ledger
// row AND no domain row (the caller's precondition, guard 1, which this
// function cannot see and therefore cannot re-check) — may be deleted.
//
// It is pure and total: every path returns a human-readable reason, and the
// reason is what the reaper logs whether it deletes or refuses. Nothing here
// consults cfg.Enabled — arming is the caller's separate question, which is
// what lets observe-only mode log the same decision it would have acted on.
//
// EVERY guard below must hold. Anything missing, unparseable, or unrecognized
// is a REFUSAL, never a default-allow: a dropped tag is expected (tags.go
// fails open on purpose) and the only thing that makes that asymmetry safe is
// this function failing closed.
func orphanReclaimable(audit IdentityAudit, cfg ReclaimConfig, now time.Time) (bool, string) {
	// Policy-level guards first. These describe the DEPLOYMENT rather than the
	// identity, so an unconfigured policy refuses every candidate with one
	// clear reason instead of N per-identity ones.
	zones, reason := reclaimPolicyUsable(cfg)
	if reason != "" {
		return false, reason
	}

	// Guard 8, checked before any tag reasoning: an identity the provider will
	// currently accept mail through is never deleted, whatever its tags claim.
	// A tag is metadata e2a wrote; this bit is the provider's own answer to
	// "can this thing send today?", and it is the one fact that would make a
	// mistaken deletion immediately customer-visible.
	if audit.VerifiedForSending {
		return false, "identity is verified for sending"
	}

	// Guard 2: the ownership anchor. Same key/value isManagedIdentity reads —
	// an identity e2a did not create (or a foreign one in a shared AWS
	// account) is never a reclaim candidate.
	if audit.Tags[managedIdentityTagKey] != managedIdentityTagValue {
		return false, "identity is not tagged as managed by e2a (" + managedIdentityTagKey + ")"
	}
	// Guard 3: a shared AWS account can hold prod's and staging's identities
	// side by side. Only this deployment's own may be reclaimed by it.
	if env := audit.Tags[envTagKey]; env != cfg.Deployment {
		return false, fmt.Sprintf("identity belongs to deployment %q, not %q", env, cfg.Deployment)
	}
	// Guard 4: purpose classifies the OWNER (tags.go). Only fixtures — e2a's
	// own internal/monitoring accounts — are ever automatically reclaimable;
	// "customer", an unrecognized value, and a missing tag are all refusals.
	if purpose := audit.Tags[purposeTagKey]; purpose != purposeFixture {
		return false, fmt.Sprintf("identity purpose is %q, not a fixture", purpose)
	}

	// Guard 5: the operator-chosen TTL has actually run out.
	expires, reason := parseTagTime(audit.Tags, expiresTagKey)
	if reason != "" {
		return false, reason
	}
	if !expires.Before(now) {
		return false, fmt.Sprintf("identity has not expired yet (%s=%s)", expiresTagKey, expires.Format(time.RFC3339))
	}

	// Guard 6: an absolute age floor, deliberately INDEPENDENT of the expiry
	// above. The expiry is derived from a configured TTL and the provisioning
	// host's clock, so a mis-set TTL or clock skew can stamp an already-past
	// expiry onto an identity created seconds ago. The floor means the worst
	// such a mistake can do is nothing at all until the identity is genuinely
	// old. A future-dated created stamp fails this the same way a fresh one
	// does, since its age is negative.
	created, reason := parseTagTime(audit.Tags, createdTagKey)
	if reason != "" {
		return false, reason
	}
	if age := now.Sub(created); age < cfg.MinAge {
		return false, fmt.Sprintf("identity is younger than the %s minimum age (created %s, age %s)", cfg.MinAge, created.Format(time.RFC3339), age.Round(time.Second))
	}

	// Guard 7, last and strongest: the name must sit at or under a zone the
	// operator has declared to be e2a's own test space. Customer domains are
	// never under it, so even a total failure of every tag guard above cannot
	// reach one.
	zone, ok := matchingZone(audit.Domain, zones)
	if !ok {
		return false, "identity name is outside every configured reclaim zone"
	}
	return true, fmt.Sprintf("expired %s fixture under reclaim zone %s, created %s, not verified for sending",
		cfg.Deployment, zone, created.Format(time.RFC3339))
}

// reclaimPolicyUsable reports whether the POLICY could reclaim anything at
// all, independent of any particular identity, returning the normalized zone
// list when it can and the refusal reason when it cannot. Split out of
// orphanReclaimable so the reaper can answer it BEFORE spending a provider
// round trip inspecting a candidate: the default (self-host, and every
// deployment that omits the config block) can never reclaim, and must keep
// costing exactly what the alert-only audit cost before this existed.
func reclaimPolicyUsable(cfg ReclaimConfig) (zones []string, refusal string) {
	zones = normalizedZones(cfg.Zones)
	if len(zones) == 0 {
		return nil, "no reclaim zone is configured (an empty reclaim_zones list reclaims nothing)"
	}
	if cfg.Deployment == "" {
		return nil, "this deployment is unnamed, so an identity's " + envTagKey + " tag cannot be attributed to it"
	}
	if cfg.MinAge <= 0 {
		return nil, "no minimum age is configured (reclaim_min_age must be positive)"
	}
	return zones, ""
}

// parseTagTime reads one RFC3339 tag, returning a refusal reason (rather than
// an error) for both "absent" and "unparseable" — the two cases the caller
// treats identically. The reason names the tag key so an operator reading the
// reaper's log knows exactly which stamp to go look at.
func parseTagTime(tags map[string]string, key string) (time.Time, string) {
	raw, ok := tags[key]
	if !ok || raw == "" {
		return time.Time{}, "identity carries no " + key + " tag"
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Sprintf("identity's %s tag %q is not RFC3339", key, raw)
	}
	return parsed, ""
}

// normalizedZones lowercases the configured zones and drops the empty ones. An
// empty entry is dropped rather than kept because the suffix rule below would
// otherwise turn it into a wildcard ("." + "" == "."), quietly converting a
// stray blank line in the config into authority over every identity whose name
// contains a dot — i.e. all of them.
func normalizedZones(zones []string) []string {
	out := make([]string, 0, len(zones))
	for _, zone := range zones {
		zone = strings.ToLower(strings.Trim(strings.TrimSpace(zone), "."))
		if zone == "" {
			continue
		}
		out = append(out, zone)
	}
	return out
}

// matchingZone reports which configured zone name falls under, matching on a
// LABEL BOUNDARY only: name == zone, or name ends in "." + zone. A plain
// strings.HasSuffix would make "eviltrymnexa.com" match zone "trymnexa.com",
// handing a domain anyone can register the reclaim authority of e2a's own test
// zone — which is exactly the mistake this guard exists to prevent. DNS names
// are case-insensitive and the provider echoes back whatever case the identity
// was created with, so both sides are lowercased first.
func matchingZone(name string, normalizedZones []string) (string, bool) {
	name = strings.ToLower(strings.Trim(strings.TrimSpace(name), "."))
	if name == "" {
		return "", false
	}
	for _, zone := range normalizedZones {
		if name == zone || strings.HasSuffix(name, "."+zone) {
			return zone, true
		}
	}
	return "", false
}
