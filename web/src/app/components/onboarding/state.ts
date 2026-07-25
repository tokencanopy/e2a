// Pure state helpers for the onboarding flow.
// No React, no fetch — just data transforms and derivations.

import type {
  AddressType,
  ChecklistStep,
  DomainCapabilityStatus,
  DomainInfo,
  DomainProgress,
  CustomFlowStep,
} from "./types";
import type { DashboardAgent } from "../types";
import { AGENTS_DOMAIN } from "../../../lib/site";

// ── Capability axes ──────────────────────────────────────

// A domain has two independent capabilities: it can receive mail (inbound) and
// it can send as its own address (outbound). The backend reports both under
// `capabilities`; these readers prefer that field and fall back to the legacy
// `verified` / `sending_status` pair, which a server predating `capabilities`
// still returns. Read every axis through these — never off `verified` directly,
// which names only the inbound axis while looking domain-wide.

/** Inbound (can-receive) capability: ownership TXT + inbound MX. */
export function inboundCapability(domain: DomainInfo): DomainCapabilityStatus {
  return domain.capabilities?.inbound ?? (domain.verified ? "verified" : "pending");
}

/** Outbound (send-as-own-address) capability: the SES sending identity rollup. */
export function outboundCapability(domain: DomainInfo): DomainCapabilityStatus {
  return domain.capabilities?.outbound ?? domain.sending_status ?? "none";
}

/** Whether the domain can receive mail today — the gate for creating an inbox. */
export function canReceive(domain: DomainInfo): boolean {
  return inboundCapability(domain) === "verified";
}

// ── Checklist derivation ─────────────────────────────────

/** Derive the checklist step for a domain given its agents. */
export function deriveChecklistStep(
  domain: DomainInfo,
  agents: DashboardAgent[],
): ChecklistStep {
  const domainAgents = agents.filter((a) => a.domain === domain.domain);

  if (!canReceive(domain)) return "domain_added";
  if (domainAgents.length === 0) return "domain_verified";
  return "agent_created";
}

/** Build progress objects for all domains. */
export function buildDomainProgress(
  domains: DomainInfo[],
  agents: DashboardAgent[],
): DomainProgress[] {
  return domains.map((d) => ({
    domain: d,
    step: deriveChecklistStep(d, agents),
    agentCount: agents.filter((a) => a.domain === d.domain).length,
  }));
}

// ── Resume logic ─────────────────────────────────────────

/** Given a domain's checklist progress, determine the onboarding step to resume at.
 *  Returns null for domains that already have agents — those should send users
 *  to the Domains or Agents page, not back into onboarding (design line 528). */
export function getResumeTarget(progress: DomainProgress): CustomFlowStep | null {
  switch (progress.step) {
    case "domain_added":
      return "dns";
    case "domain_verified":
      return "create_agent";
    case "agent_created":
      return null;
  }
}

/** Determine the address type from a domain string. */
export function getAddressType(domain: string): AddressType {
  return AGENTS_DOMAIN !== "" && domain === AGENTS_DOMAIN ? "shared" : "custom";
}

// ── Validation ───────────────────────────────────────────

// Must match backend: internal/agent/api.go slugPattern + validateSlug
const SLUG_RE = /^[a-z0-9][a-z0-9-]{0,38}[a-z0-9]$/;

// Must match backend: internal/agent/api.go reservedSlugs
const RESERVED_SLUGS = new Set([
  "admin", "postmaster", "abuse", "noreply", "no-reply",
  "mailer-daemon", "info", "help", "demo", "test",
  "www", "mail", "agent", "api", "system", "root",
]);

export function isValidSlug(slug: string): boolean {
  return slug.length >= 2 && slug.length <= 40 && SLUG_RE.test(slug) && !RESERVED_SLUGS.has(slug);
}

export function isValidDomain(domain: string): boolean {
  if (!domain || domain.length > 253) return false;
  const parts = domain.split(".");
  return parts.length >= 2 && parts.every((p) => /^[a-z0-9]([a-z0-9-]*[a-z0-9])?$/.test(p));
}

export function isValidLocalPart(localPart: string): boolean {
  return localPart.length >= 1 && localPart.length <= 64 && /^[a-z0-9][a-z0-9._-]*$/.test(localPart);
}
