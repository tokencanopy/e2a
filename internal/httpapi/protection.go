package httpapi

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/tokencanopy/e2a/internal/identity"
)

// Agent protection config sub-resource (design 2026-06-22-agent-protection-config.md).
//
// BETA. The screening posture (inbound/outbound gate + content scan + hold
// mechanism) lives here, not on AgentView, so the whole resource sits behind
// account scope: an agent-scoped credential — the entity being screened — can
// neither read its own detection tuning nor change its posture (closes audit
// #13/#21). The scan is exposed as a semantic sensitivity level; the engine's
// raw thresholds are derived server-side (see internal/identity/protection.go).

const protectionBetaDoc = "Beta: the agent protection config is unstable — its shape may change before it is declared stable."

// ProtectionGateView is one direction's trust gate: who may send, and what a
// non-match does. policy is a monotonic trust ladder (open < domain < allowlist).
// Leaves are optional (omitempty) with defaults, so a caller may send an empty
// gate/scan object and get the safe-permissive default. The three TOP-LEVEL
// keys (inbound/outbound/holds) stay required (design D3) — a missing section
// is a 422, not a silent reset.
//
// INBOUND gate identity note (#299): the inbound allowlist/domain gate matches on
// the message's From address, which only carries a specific per-agent identity for
// senders on a sending-verified domain. Mail from agents NOT yet sending-verified
// is relayed under the shared "via e2a" address (internal/outbound/sender.go) — it
// authenticates but is the same address for every such agent. The relay treats that
// shared sender as unresolvable (see senderResolvable), so it can never satisfy an
// allowlist/domain gate and is flagged (fail closed); under "open" it still passes.
// Net: per-agent inbound allowlisting is reliable for sending-verified senders;
// unverified intra-system senders are uniformly gated, not silently admitted.
//
// Inbound allowlist/domain gates fail closed unless DMARC passes, then match the
// aligned RFC 5322 From address. Open policy remains intentionally ungated.
type ProtectionGateView struct {
	// Policy and Action are OPEN sets on this response view (evolving config
	// vocabularies — a new gate posture or disposition must not break
	// spec-generated clients); the mirroring ProtectionGateRequest keeps the
	// closed enums, which is where validation belongs.
	Policy    string   `json:"policy,omitempty" default:"open" doc:"Trust gate: open (all), domain (inbound: senders on the listed domains; outbound: recipients on the agent's own domain — for a subdomain agent bound to its verified PARENT domain, that own domain is the parent, so under this policy it can send to @parent recipients but not to its own @subdomain), allowlist (listed addresses). Open set: new values may be added over time, so treat these as strings and tolerate unknown values. Known values: open, allowlist, domain."`
	Allowlist []string `json:"allowlist,omitempty" nullable:"false" maxItems:"1000" doc:"Addresses (allowlist) or, inbound only, domains (domain) the gate trusts; ignored for open and for the outbound domain policy (which matches the agent's own domain, not this list). Inbound allowlist/domain gates first require DMARC pass, then match the aligned RFC 5322 From address."`
	Action    string   `json:"action,omitempty" default:"flag" doc:"What a gate non-match does: flag (deliver + annotate), review (hold), block. Open set: new values may be added over time, so treat these as strings and tolerate unknown values. Known values: flag, review, block."`
}

// ProtectionScanView is one direction's content scan, as a semantic sensitivity
// level. off disables it; low|medium|high tune how aggressively flagged content
// is held/blocked.
type ProtectionScanView struct {
	// Sensitivity is an OPEN set on this response view (evolving vocabulary —
	// a new level must not break clients); ProtectionScanRequest keeps the
	// closed enum for validation.
	Sensitivity string `json:"sensitivity,omitempty" default:"off" doc:"Content-scan sensitivity: off disables; low|medium|high increase aggressiveness. Open set: new values may be added over time, so treat these as strings and tolerate unknown values. Known values: off, low, medium, high."`
}

// ProtectionDirectionView pairs the gate and scan for one direction.
type ProtectionDirectionView struct {
	Gate ProtectionGateView `json:"gate"`
	Scan ProtectionScanView `json:"scan"`
}

// ProtectionHoldsView is the shared review-queue mechanism for held items.
type ProtectionHoldsView struct {
	TTLSeconds int `json:"ttl_seconds,omitempty" minimum:"0" default:"604800" doc:"How long a held item waits before its on_expiry action fires."`
	// OnExpiry is an OPEN set on this response view: today's values happen to
	// be two, but expiry disposition is an evolving config vocabulary (e.g. a
	// future extend/escalate), not a binary invariant of the model like
	// message direction. ProtectionHoldsRequest keeps the closed enum.
	OnExpiry              string `json:"on_expiry,omitempty" default:"reject" doc:"What happens to a held item when its TTL expires. Open set: new values may be added over time, so treat these as strings and tolerate unknown values. Known values: approve, reject."`
	SuppressNotifications bool   `json:"suppress_notifications,omitempty" default:"false" doc:"Suppress the approval-notification email for held messages on this agent."`
}

// ProtectionConfigView is the GET (and PUT echo) RESPONSE. The PUT body is the
// separate ProtectionConfigRequest below — see its comment for why the two are
// distinct types despite the identical shape.
type ProtectionConfigView struct {
	Inbound  ProtectionDirectionView `json:"inbound"`
	Outbound ProtectionDirectionView `json:"outbound"`
	Holds    ProtectionHoldsView     `json:"holds"`
}

func protectionViewFromIdentity(ag *identity.AgentIdentity) ProtectionConfigView {
	return ProtectionConfigView{
		Inbound: ProtectionDirectionView{
			Gate: ProtectionGateView{Policy: ag.InboundPolicy, Allowlist: orEmpty(ag.InboundAllowlist), Action: ag.InboundPolicyAction},
			Scan: ProtectionScanView{Sensitivity: ag.InboundScanSensitivity},
		},
		Outbound: ProtectionDirectionView{
			Gate: ProtectionGateView{Policy: ag.OutboundPolicy, Allowlist: orEmpty(ag.OutboundAllowlist), Action: ag.OutboundPolicyAction},
			Scan: ProtectionScanView{Sensitivity: ag.OutboundScanSensitivity},
		},
		Holds: ProtectionHoldsView{TTLSeconds: ag.HITLTTLSeconds, OnExpiry: ag.HITLExpirationAction, SuppressNotifications: ag.SuppressNotifications},
	}
}

// ProtectionGateRequest / ProtectionScanRequest / ProtectionDirectionRequest /
// ProtectionHoldsRequest / ProtectionConfigRequest mirror the *View shapes
// field-for-field as the PUT body. They are dedicated INPUT types (not the
// Views) because the spec's forward-compat stance is asymmetric: request
// schemas stay `additionalProperties: false` (strict validation — an unknown
// key is a 422, not a silent no-op on a security posture), while response
// schemas are open (`additionalProperties: true`) so clients tolerate additive
// fields. One shared schema cannot carry both; the stability pass in
// stability.go panics if a schema is reachable from both sides. Keep the
// validation tags here in lockstep with the View tags above — except the
// enum tags, which live ONLY on these request types: the Views document the
// same vocabularies as open sets (response-side vocabularies that can evolve
// are open; see docs/api.md "Versioning & stability"), while an unknown value
// a client SENDS is still validated and rejected here.
type ProtectionGateRequest struct {
	Policy    string   `json:"policy,omitempty" enum:"open,allowlist,domain" default:"open" doc:"Trust gate: open (all), domain (inbound: senders on the listed domains; outbound: recipients on the agent's own domain — for a subdomain agent bound to its verified PARENT domain, that own domain is the parent, so under this policy it can send to @parent recipients but not to its own @subdomain), allowlist (listed addresses)."`
	Allowlist []string `json:"allowlist,omitempty" nullable:"false" maxItems:"1000" doc:"Addresses (allowlist) or, inbound only, domains (domain) the gate trusts; ignored for open and for the outbound domain policy (which matches the agent's own domain, not this list). Inbound allowlist/domain gates first require DMARC pass, then match the aligned RFC 5322 From address."`
	Action    string   `json:"action,omitempty" enum:"flag,review,block" default:"flag" doc:"What a gate non-match does: flag (deliver + annotate), review (hold), block."`
}

// ProtectionScanRequest mirrors ProtectionScanView for the PUT body.
type ProtectionScanRequest struct {
	Sensitivity string `json:"sensitivity,omitempty" enum:"off,low,medium,high" default:"off" doc:"Content-scan sensitivity: off disables; low|medium|high increase aggressiveness."`
}

// ProtectionDirectionRequest mirrors ProtectionDirectionView for the PUT body.
type ProtectionDirectionRequest struct {
	Gate ProtectionGateRequest `json:"gate"`
	Scan ProtectionScanRequest `json:"scan"`
}

// ProtectionHoldsRequest mirrors ProtectionHoldsView for the PUT body.
type ProtectionHoldsRequest struct {
	TTLSeconds            int    `json:"ttl_seconds,omitempty" minimum:"0" default:"604800" doc:"How long a held item waits before its on_expiry action fires."`
	OnExpiry              string `json:"on_expiry,omitempty" enum:"approve,reject" default:"reject" doc:"What happens to a held item when its TTL expires."`
	SuppressNotifications bool   `json:"suppress_notifications,omitempty" default:"false" doc:"Suppress the approval-notification email for held messages on this agent."`
}

// ProtectionConfigRequest is the PUT body (full replace). The three top-level
// keys are required (no silent section reset); leaves are optional and fill
// from defaults.
type ProtectionConfigRequest struct {
	Inbound  ProtectionDirectionRequest `json:"inbound"`
	Outbound ProtectionDirectionRequest `json:"outbound"`
	Holds    ProtectionHoldsRequest     `json:"holds"`
}

func protectionConfigFromRequest(v ProtectionConfigRequest) identity.ProtectionConfig {
	return identity.ProtectionConfig{
		InboundGatePolicy:       v.Inbound.Gate.Policy,
		InboundAllowlist:        v.Inbound.Gate.Allowlist,
		InboundGateAction:       v.Inbound.Gate.Action,
		InboundScanSensitivity:  v.Inbound.Scan.Sensitivity,
		OutboundGatePolicy:      v.Outbound.Gate.Policy,
		OutboundAllowlist:       v.Outbound.Gate.Allowlist,
		OutboundGateAction:      v.Outbound.Gate.Action,
		OutboundScanSensitivity: v.Outbound.Scan.Sensitivity,
		HITLTTLSeconds:          v.Holds.TTLSeconds,
		HITLExpirationAction:    v.Holds.OnExpiry,
		SuppressNotifications:   v.Holds.SuppressNotifications,
	}
}

type getProtectionInput struct {
	Address string `path:"email" doc:"The agent's full email address."`
}

type protectionOutput struct {
	Body ProtectionConfigView
}

type putProtectionInput struct {
	Address string `path:"email" doc:"The agent's full email address."`
	Body    ProtectionConfigRequest
}

func (s *Server) registerAgentProtection() {
	registerOp(s.API, huma.Operation{
		OperationID: "getAgentProtection",
		Method:      http.MethodGet,
		Path:        "/v1/agents/{email}/protection",
		Summary:     "Get an agent's protection config (beta)",
		Description: "Read the agent's protection posture — inbound/outbound trust gate, content-scan sensitivity, and hold-queue mechanism. Account scope only: an agent-scoped credential cannot read its own protection config. " + protectionBetaDoc,
		Tags:        []string{"agents"},
		Security:    []map[string][]string{{"bearer": {}}},
		Extensions:  beta(),
	}, s.handleGetProtection)

	registerOp(s.API, huma.Operation{
		OperationID: "putAgentProtection",
		Method:      http.MethodPut,
		Path:        "/v1/agents/{email}/protection",
		Summary:     "Replace an agent's protection config (beta)",
		Description: "Replace the agent's protection posture wholesale. The three top-level keys (inbound, outbound, holds) are required; leaves default. Account scope only. " + protectionBetaDoc,
		Tags:        []string{"agents"},
		Security:    []map[string][]string{{"bearer": {}}},
		Extensions:  beta(),
	}, s.handlePutProtection)
}

func (s *Server) handleGetProtection(ctx context.Context, in *getProtectionInput) (*protectionOutput, error) {
	// Protection config is account administration — an agent-scoped credential
	// (the screened entity) must not read its own detection tuning (#13).
	if _, err := s.requireAccountScope(ctx); err != nil {
		return nil, err
	}
	ag, err := s.resolveOwnedAgent(ctx, in.Address)
	if err != nil {
		return nil, err
	}
	return &protectionOutput{Body: protectionViewFromIdentity(ag)}, nil
}

func (s *Server) handlePutProtection(ctx context.Context, in *putProtectionInput) (*protectionOutput, error) {
	if _, err := s.requireAccountScope(ctx); err != nil {
		return nil, err
	}
	ag, err := s.resolveOwnedAgent(ctx, in.Address)
	if err != nil {
		return nil, err
	}
	if s.deps.UpdateAgentProtection == nil {
		return nil, NewError(http.StatusInternalServerError, "internal_error", "update unavailable")
	}
	cfg := protectionConfigFromRequest(in.Body)
	// Content scan is gated off by default for GA (see identity.ContentScanEnabled).
	// Clamp the scan knob to "off" rather than store a sensitivity that would
	// silently never run — so get_protection reads back the honest, effective
	// posture. Re-enabling the flag restores the caller's intended values on the
	// next write.
	if !identity.ContentScanEnabled() {
		cfg.InboundScanSensitivity = identity.SensitivityOff
		cfg.OutboundScanSensitivity = identity.SensitivityOff
	}
	if err := s.deps.UpdateAgentProtection(ctx, ag.ID, ag.UserID, cfg); err != nil {
		return nil, NewError(http.StatusBadRequest, "invalid_request", err.Error())
	}
	updated, err := s.deps.GetAgent(ctx, ag.ID)
	if err != nil || updated == nil {
		return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to reload agent")
	}
	return &protectionOutput{Body: protectionViewFromIdentity(updated)}, nil
}
