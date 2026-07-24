// Package inboundscreen is the shared inbound-protection evaluation core: the
// per-agent ingestion-gate escalation + piguard content scan + most-severe
// merge that decides whether one inbound message is delivered, flagged,
// held for review, or accept-then-quarantined.
//
// It was extracted verbatim from internal/relay (screening.go) so the loopback
// self-send paths (internal/agent's performSelfSend and the HITL approve /
// TTL-approve local deliveries) can run the exact same evaluation as SMTP
// inbound — previously they created the inbound row with a zero-value
// screening verdict, silently bypassing the agent's inbound protection. The
// relay delegates here; behavior on the SMTP path is unchanged.
package inboundscreen

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"time"

	"github.com/tokencanopy/e2a/internal/emailauth"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/inboundpolicy"
	"github.com/tokencanopy/e2a/internal/piguard"
)

// Result is the outcome of content-screening one inbound message: the
// denormalized verdict (incl. any review-hold status) for the message row, the audit
// rows to append, and (when the applied action is block) the data the email.blocked
// event carries.
type Result struct {
	Denorm        identity.InboundScreening
	Events        []identity.ProtectionEvent
	AppliedAction piguard.Action // most-severe of gate + scan
	Hold          bool           // applied action is review|block → suppress delivery
	Detected      bool           // a scan violation fired (used to attribute payload fields)
	Score         float64
	Action        string
	Categories    []string
	Reason        string
}

// Blocked reports whether the applied action is block — the message is
// accept-then-quarantined (review_rejected). Drives the email.blocked event.
func (r Result) Blocked() bool { return r.AppliedAction == piguard.ActionBlock }

// Review reports whether the applied action is review — the message is held as
// pending_review awaiting a human / TTL. Drives the email.review_requested event.
func (r Result) Review() bool { return r.AppliedAction == piguard.ActionReview }

// Evaluate runs the agent's content scan (when inbound_scan='on'), combines it
// with the ingestion-gate decision into one applied action, and decides whether the
// message is HELD (review/block) or delivered (flag/allow).
//
//   - review → held as pending_review (awaiting a human / TTL), delivery suppressed.
//   - block  → accept-then-quarantine as review_rejected (dropped, no human),
//     delivery suppressed.
//   - flag   → delivered + annotated (the gate's email.flagged path is unchanged).
//   - allow  → delivered normally.
func Evaluate(ctx context.Context, engine *piguard.Engine, agent *identity.AgentIdentity, messageID, senderEmail string, body []byte, auth *emailauth.Authentication, gate inboundpolicy.Decision) Result {
	var res Result

	// Gate action: a flagged sender escalates to the agent's inbound_policy_action
	// (default 'flag' → no behavior change; operators opt into review/block).
	gateAction := piguard.ActionAllow
	if gate.Flagged {
		gateAction = piguard.Action(agent.InboundPolicyAction)
		res.Events = append(res.Events, identity.ProtectionEvent{
			ID:          identity.DeterministicProtectionEventID(messageID, identity.ScreeningSourceGate, identity.ReviewReasonSenderGate, ""),
			MessageID:   messageID,
			AgentID:     agent.ID,
			Direction:   "inbound",
			Source:      identity.ScreeningSourceGate,
			Reason:      identity.ReviewReasonSenderGate,
			Action:      agent.InboundPolicyAction,
			SubjectAddr: senderEmail,
		})
	}

	// Scan action.
	scanAction := piguard.ActionAllow
	var scanScore *float64
	if identity.ContentScanEnabled() && agent.InboundScan == identity.ScanOn {
		segs, sig, _ := piguard.Extract(body, 0)
		agg := engine.Evaluate(ctx, piguard.Request{
			Direction: piguard.DirectionInput,
			Segments:  segs,
			Signals:   sig,
			Sender:    senderEmail,
			Auth:      auth,
			SizeBytes: len(body),
		})
		act := agg.Action(agent.InboundScanReviewThreshold, agent.InboundScanBlockThreshold)
		// Record only violations (action ≠ allow). A below-threshold score is allowed
		// and delivered silently; flag (from the force-floor, e.g. Unicode tags) is
		// recorded + delivered; review/block are held.
		if act != piguard.ActionAllow {
			scanAction = act
			score := agg.Score
			scanScore = &score
			res.Detected = true
			res.Score = score
			res.Reason = scanReason(agg)
			for _, c := range agg.Categories {
				res.Categories = append(res.Categories, c.Name)
			}
			catsJSON, _ := json.Marshal(agg.Categories)
			// detectorLabel names every detector that actually contributed a StatusOK
			// verdict (e.g. "gemini,heuristics") instead of a hardcoded single name —
			// with two detectors wired in, "heuristics" alone would misattribute a
			// score/action that Gemini (or Gemini alone) actually drove. rawJSON keeps
			// the full per-detector breakdown (each one's own score/status/categories/
			// rationale) for drill-down without needing to reproduce the request.
			detectorLabel := agg.DetectorLabel()
			rawJSON, _ := json.Marshal(agg.PerDetector)
			res.Events = append(res.Events, identity.ProtectionEvent{
				ID:         identity.DeterministicProtectionEventID(messageID, identity.ScreeningSourceScan, identity.ReviewReasonInboundScan, detectorLabel),
				MessageID:  messageID,
				AgentID:    agent.ID,
				Direction:  "inbound",
				Source:     identity.ScreeningSourceScan,
				Reason:     identity.ReviewReasonInboundScan,
				Action:     string(act),
				Detector:   detectorLabel,
				Score:      &score,
				Categories: json.RawMessage(catsJSON),
				Raw:        json.RawMessage(rawJSON),
			})
		}
	}

	applied := piguard.MoreSevere(gateAction, scanAction)
	res.AppliedAction = applied
	res.Action = string(applied)
	res.Hold = applied == piguard.ActionReview || applied == piguard.ActionBlock

	if applied == piguard.ActionAllow {
		return res // benign: no denorm, no hold (gate audit row may still be present)
	}

	// Attribute the verdict to its driving producer for the denorm + event.
	reviewReason := identity.ReviewReasonSenderGate
	if res.Detected && scanAction == applied {
		reviewReason = identity.ReviewReasonInboundScan
	} else if !gate.Flagged && res.Detected {
		reviewReason = identity.ReviewReasonInboundScan
	}
	if res.Reason == "" {
		res.Reason = gate.Reason
	}

	res.Denorm = identity.InboundScreening{
		ReviewReason: reviewReason,
		ScanScore:    scanScore,
		ScanAction:   string(applied),
	}
	if res.Hold {
		if applied == piguard.ActionBlock {
			// Accept-then-quarantine: persisted but terminal-dropped, no human.
			res.Denorm.Status = identity.MessageStatusReviewRejected
		} else {
			res.Denorm.Status = identity.MessageStatusPendingReview
			ttl := agent.HITLTTLSeconds
			if ttl <= 0 {
				ttl = identity.HITLDefaultTTLSeconds
			}
			exp := time.Now().Add(time.Duration(ttl) * time.Second)
			res.Denorm.ApprovalExpiresAt = &exp
		}
	}
	return res
}

// LoopbackGate evaluates the agent's ingestion gate for the inbound leg of a
// loopback self-send. The message is first-party — composed by the agent
// itself under its API-key identity — which is the strongest possible sender
// authentication, so the gate sees the same facts an SMTP roundtrip of the
// identical message would produce: sender = the agent's own address,
// resolvable (it maps to this very agent), DMARC pass (own domain, DKIM-signed
// by the platform). The gate outcome therefore reduces to allowlist/domain
// membership of the agent's own address — an agent whose gating policy does
// not include itself gets its self-sends escalated per inbound_policy_action,
// exactly like the relay path would.
func LoopbackGate(agent *identity.AgentIdentity) inboundpolicy.Decision {
	return inboundpolicy.EvaluateIngestion(agent.InboundPolicy, agent.InboundAllowlist, agent.EmailAddress(), true, "pass")
}

// EvaluateLoopback is the loopback-flavored entry point: gate + scan over the
// composed MIME with the agent itself as the sender and no SMTP authentication
// evidence (there was no wire hop). messageID must be the pre-allocated
// inbound row id so the audit rows and deterministic event ids anchor to it.
func EvaluateLoopback(ctx context.Context, engine *piguard.Engine, agent *identity.AgentIdentity, messageID string, body []byte) (Result, inboundpolicy.Decision) {
	gate := LoopbackGate(agent)
	return Evaluate(ctx, engine, agent, messageID, agent.EmailAddress(), body, nil, gate), gate
}

func scanReason(agg piguard.Aggregate) string {
	if len(agg.Categories) == 0 {
		return "content scan flagged the message"
	}
	return "content scan: " + agg.Categories[0].Name
}

// GeminiDetectorTimeout is the per-detector timeout used when the Gemini detector
// is wired in, wider than the Engine's default (5s) so the retry/backoff schedule
// in piguard/gemini.go (up to geminiDefaultMaxRetries retries) has room to run
// instead of being cut off by the engine before it can fire.
const GeminiDetectorTimeout = 10 * time.Second

// GeminiDetectorEnabled reports whether BuildEngine should even attempt to
// construct the Gemini detector. Defaults to true — the existing behavior, where
// Gemini is enabled purely by GEMINI_API_KEY/GOOGLE_API_KEY being present — unless
// E2A_GEMINI_DETECTOR_ENABLED is explicitly set to "false". This is an operator
// kill-switch/A-B toggle independent of the credential: it lets you disable Gemini
// (isolating whether it or heuristics is driving a given block/review outcome, or
// rolling back without touching secrets) without having to remove the API key.
func GeminiDetectorEnabled() bool {
	return os.Getenv("E2A_GEMINI_DETECTOR_ENABLED") != "false"
}

// BuildEngine constructs the piguard screening engine for inbound mail. The
// heuristics detector is always included. The Gemini detector is added when
// GeminiDetectorEnabled() and GEMINI_API_KEY or GOOGLE_API_KEY is set in the
// environment; its prompt only classifies inbound content, so this engine
// (inbound-only, unlike buildAgentScreenEngine in internal/agent/api.go) is where
// it belongs. Used by the SMTP relay and by the loopback inbound-leg screeners.
func BuildEngine() *piguard.Engine {
	detectors := []piguard.Detector{piguard.NewHeuristicsDetector()}
	cfg := piguard.EngineConfig{}
	if GeminiDetectorEnabled() {
		if d, err := piguard.NewGeminiDetector(piguard.GeminiConfig{}); err == nil {
			detectors = append(detectors, d)
			cfg.Timeout = GeminiDetectorTimeout
			log.Printf("[piguard] Gemini detector enabled (model: %s)", d.Model())
		}
	} else {
		log.Printf("[piguard] Gemini detector disabled via E2A_GEMINI_DETECTOR_ENABLED=false")
	}
	return piguard.NewEngine(cfg, detectors...)
}
