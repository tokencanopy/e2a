package identity

import (
	"context"
	"fmt"
	"os"

	"github.com/tokencanopy/e2a/internal/inboundpolicy"
)

// ContentScanEnabled reports whether the piguard content scan is turned on for
// this deployment. The detector ships as a single heuristic lexicon that is
// near-zero-false-positive but paraphrase-evadable, so it is gated OFF by
// default for GA and only runs where an operator has explicitly opted in via
// E2A_CONTENT_SCAN_ENABLED=true. When off: the two screening paths skip the
// scan entirely (the recipient/sender gate, HITL review holds, suppression and
// allowlists are unaffected), and the protection API clamps scan_sensitivity to
// "off" so a caller never sets a knob that silently does nothing. Read at call
// time so tests (and a live config flip) take effect without a restart.
func ContentScanEnabled() bool {
	return os.Getenv("E2A_CONTENT_SCAN_ENABLED") == "true"
}

// Scan sensitivity levels — the public protection API's content-scan knob
// (design 2026-06-22-agent-protection-config.md §4.2). A level maps to the
// engine's (review, block) threshold pair; `off` disables the scan.
const (
	SensitivityOff    = "off"
	SensitivityLow    = "low"
	SensitivityMedium = "medium"
	SensitivityHigh   = "high"
)

// sensitivityBand is the (scan toggle, review threshold, block threshold) a
// sensitivity level resolves to. The handler writes these derived columns so
// the piguard engine (which reads the float thresholds) is unchanged; the
// sensitivity column is the API read-back source of truth.
type sensitivityBand struct {
	scan   string
	review float64
	block  float64
}

// sensitivityBands is the canonical level → threshold mapping (§4.3). `medium`
// equals the historical default pair (0.5 / 0.9) so pre-existing agents read
// back as medium-tuned. Numbers are internal and tunable post-beta (O5).
var sensitivityBands = map[string]sensitivityBand{
	SensitivityOff:    {scan: ScanOff, review: 0.5, block: 0.9},
	SensitivityLow:    {scan: ScanOn, review: 0.70, block: 0.95},
	SensitivityMedium: {scan: ScanOn, review: 0.50, block: 0.90},
	SensitivityHigh:   {scan: ScanOn, review: 0.30, block: 0.80},
}

func validSensitivity(s string) bool {
	_, ok := sensitivityBands[s]
	return ok
}

// ProtectionConfig is the full per-agent protection posture as the public
// /protection resource models it: a gate (trust policy + allowlist + action)
// and a content-scan sensitivity per direction, plus the shared hold-queue
// mechanism. Concrete values (the PUT body is a full replace), so
// UpdateAgentProtection validates and writes the effective posture atomically.
type ProtectionConfig struct {
	InboundGatePolicy       string
	InboundAllowlist        []string
	InboundGateAction       string
	InboundScanSensitivity  string
	OutboundGatePolicy      string
	OutboundAllowlist       []string
	OutboundGateAction      string
	OutboundScanSensitivity string
	HITLTTLSeconds          int
	HITLExpirationAction    string
	SuppressNotifications   bool
}

// inboundGatePolicyValid restricts the inbound trust gate to the supported
// ladder (open|allowlist|domain). A DMARC-alignment "verified_only" posture was
// dropped pre-GA (migration 047) and may return later as a composable flag.
func inboundGatePolicyValid(p string) bool {
	return p == inboundpolicy.Open || p == inboundpolicy.Allowlist || p == inboundpolicy.Domain
}

// ValidateProtectionConfig checks the effective posture before any write, so a
// caller gets a clean error rather than a raw CHECK-constraint violation. Levels
// are valid-by-construction (they map to a valid threshold ladder), so no
// threshold-ladder check is needed here.
func ValidateProtectionConfig(c ProtectionConfig) error {
	if !inboundGatePolicyValid(c.InboundGatePolicy) {
		return fmt.Errorf("inbound gate policy must be open, allowlist, or domain")
	}
	if !validOutboundPolicy(c.OutboundGatePolicy) {
		return fmt.Errorf("outbound gate policy must be open, allowlist, or domain")
	}
	if !validScanAction(c.InboundGateAction) {
		return fmt.Errorf("inbound gate action must be flag, review, or block")
	}
	if !validScanAction(c.OutboundGateAction) {
		return fmt.Errorf("outbound gate action must be flag, review, or block")
	}
	if !validSensitivity(c.InboundScanSensitivity) {
		return fmt.Errorf("inbound scan sensitivity must be off, low, medium, or high")
	}
	if !validSensitivity(c.OutboundScanSensitivity) {
		return fmt.Errorf("outbound scan sensitivity must be off, low, medium, or high")
	}
	if len(c.InboundAllowlist) > maxInboundAllowlist {
		return fmt.Errorf("inbound allowlist has %d entries, max %d", len(c.InboundAllowlist), maxInboundAllowlist)
	}
	if len(c.OutboundAllowlist) > maxInboundAllowlist {
		return fmt.Errorf("outbound allowlist has %d entries, max %d", len(c.OutboundAllowlist), maxInboundAllowlist)
	}
	if c.HITLTTLSeconds < 0 {
		return fmt.Errorf("holds ttl_seconds must be >= 0")
	}
	if c.HITLExpirationAction != HITLExpirationApprove && c.HITLExpirationAction != HITLExpirationReject {
		return fmt.Errorf("holds on_expiry must be approve or reject")
	}
	return nil
}

// UpdateAgentProtection writes the full protection posture for an agent owned by
// userID in a single statement, validating first. It writes the sensitivity
// columns (the API source of truth) AND the derived scan toggle + float
// thresholds the piguard engine reads, so the two never diverge and the engine
// needs no change. Returns an error if the agent isn't found or not owned.
//
// It returns the agent AS WRITTEN, re-read INSIDE the write's transaction.
// Reading afterwards from the pool was a torn read: the PUT is a full replace,
// so a concurrent PUT landing between the commit and the re-read handed the
// caller the OTHER writer's posture as the result of their own request — the
// worst shape of this bug, because the response looks authoritative. A
// concurrent trash likewise turned a committed write into a 500 "failed to
// reload agent". Same recipe as UpdateEngagementIfUnchanged; the read includes
// trashed rows for the reason given on UpdateAgentName.
func (s *Store) UpdateAgentProtection(ctx context.Context, agentID, userID string, c ProtectionConfig) (*AgentIdentity, error) {
	if err := ValidateProtectionConfig(c); err != nil {
		return nil, err
	}
	inB := sensitivityBands[c.InboundScanSensitivity]
	outB := sensitivityBands[c.OutboundScanSensitivity]

	// Normalize nil allowlists to empty slices so the column is [] not NULL.
	inAllow := c.InboundAllowlist
	if inAllow == nil {
		inAllow = []string{}
	}
	outAllow := c.OutboundAllowlist
	if outAllow == nil {
		outAllow = []string{}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx,
		`UPDATE agent_identities SET
		    inbound_policy = $3, inbound_allowlist = $4, inbound_policy_action = $5,
		    inbound_scan = $6, inbound_scan_review_threshold = $7, inbound_scan_block_threshold = $8,
		    inbound_scan_sensitivity = $9,
		    outbound_policy = $10, outbound_allowlist = $11, outbound_policy_action = $12,
		    outbound_scan = $13, outbound_scan_review_threshold = $14, outbound_scan_block_threshold = $15,
		    outbound_scan_sensitivity = $16,
		    hitl_ttl_seconds = $17, hitl_expiration_action = $18,
		    suppress_notifications = $19
		  WHERE id = $1 AND user_id = $2`,
		agentID, userID,
		c.InboundGatePolicy, inAllow, c.InboundGateAction,
		inB.scan, inB.review, inB.block,
		c.InboundScanSensitivity,
		c.OutboundGatePolicy, outAllow, c.OutboundGateAction,
		outB.scan, outB.review, outB.block,
		c.OutboundScanSensitivity,
		c.HITLTTLSeconds, c.HITLExpirationAction,
		c.SuppressNotifications,
	)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, fmt.Errorf("agent not found or not owned by user")
	}
	a, err := loadAgentByID(ctx, tx, agentID, true)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return a, nil
}
