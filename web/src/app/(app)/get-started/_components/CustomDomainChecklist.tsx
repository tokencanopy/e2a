"use client";

import { useState, useEffect, useCallback } from "react";
import { listDomains } from "../../../components/onboarding/api";
import { DomainSelector } from "./DomainSelector";
import { DNSSetupCard } from "./DNSSetupCard";
import { VerifyDomainCard } from "./VerifyDomainCard";
import { CustomAgentForm } from "./CustomAgentForm";
import { canReceive } from "../../../components/onboarding/state";
import type { DomainInfo } from "../../../components/onboarding/types";
import type { AgentData } from "../../../components/types";

/**
 * Checklist phases derived entirely from resource state.
 * No wizard-position state — re-entering always derives the correct phase.
 *
 * - select_domain: no domain chosen yet
 * - dns_and_verify: domain registered but not verified (DNS is informational,
 *   shown inline alongside verify — not a separate gate)
 * - create_agent: domain verified, ready for agent creation
 */
type ChecklistPhase = "select_domain" | "dns_and_verify" | "create_agent";

function derivePhase(domain: DomainInfo | null): ChecklistPhase {
  if (!domain) return "select_domain";
  // Inbound axis, not the legacy boolean: this gate is about whether the
  // domain can RECEIVE mail, which is what the DNS/verify phase resolves.
  if (!canReceive(domain)) return "dns_and_verify";
  return "create_agent";
}

const checklistItems = [
  { key: "domain", label: "Domain selected" },
  { key: "verified", label: "Domain verified" },
  { key: "agent", label: "Inbox created" },
] as const;

function getCompletedSteps(phase: ChecklistPhase): Set<string> {
  const completed = new Set<string>();
  if (phase === "select_domain") return completed;
  completed.add("domain");
  if (phase === "dns_and_verify") return completed;
  completed.add("verified");
  return completed;
}

function getActiveKey(phase: ChecklistPhase): string {
  if (phase === "select_domain") return "domain";
  if (phase === "dns_and_verify") return "verified";
  return "agent";
}

export function CustomDomainChecklist({
  initialDomain,
  onComplete,
  onBack,
}: {
  initialDomain?: DomainInfo | null;
  onComplete: (agent: AgentData) => void;
  /** Returns the user to the address-type chooser. Parent wires this
   * to router.back() so the browser history stays linear. */
  onBack?: () => void;
}) {
  const [domain, setDomain] = useState<DomainInfo | null>(initialDomain ?? null);
  const [existingDomains, setExistingDomains] = useState<DomainInfo[]>([]);
  const [domainsLoaded, setDomainsLoaded] = useState(false);

  // Phase is always derived from resource state — no wizard position
  const phase = derivePhase(domain);

  const fetchDomains = useCallback(async () => {
    try {
      const domains = await listDomains();
      setExistingDomains(domains);
    } catch {
      // Non-fatal — user can still register a new domain
    } finally {
      setDomainsLoaded(true);
    }
  }, []);

  useEffect(() => {
    if (!initialDomain) {
      fetchDomains();
    } else {
      setDomainsLoaded(true);
    }
  }, [initialDomain, fetchDomains]);

  const handleDomainSelected = (d: DomainInfo) => {
    setDomain(d);
  };

  const handleVerified = () => {
    if (domain) {
      setDomain({ ...domain, verified: true });
    }
  };

  const completed = getCompletedSteps(phase);
  const activeKey = getActiveKey(phase);

  return (
    <div>
      {onBack && (
        <button
          type="button"
          onClick={onBack}
          className="inline-flex items-center gap-1.5 mb-4 text-[12px] transition hover:opacity-80"
          style={{ color: "var(--fg-muted)" }}
        >
          <span aria-hidden>←</span>
          Back
        </button>
      )}
      {/* Checklist progress */}
      <div className="mb-8 flex items-center gap-3 text-xs">
        {checklistItems.map((item, i) => (
          <div key={item.key} className="flex items-center gap-2">
            {i > 0 && (
              <div className={`w-6 h-px ${completed.has(item.key) ? "bg-accent" : "bg-border"}`} />
            )}
            <span
              className={`inline-flex items-center justify-center w-5 h-5 rounded-full text-[10px] font-medium ${
                completed.has(item.key)
                  ? "bg-accent/20 text-accent"
                  : activeKey === item.key
                    ? "bg-accent text-white"
                    : "bg-border text-muted"
              }`}
            >
              {completed.has(item.key) ? "\u2713" : i + 1}
            </span>
            <span className={activeKey === item.key ? "text-foreground font-medium" : "text-muted"}>
              {item.label}
            </span>
          </div>
        ))}
      </div>

      {/* Active phase content */}
      {phase === "select_domain" && domainsLoaded && (
        <DomainSelector
          existingDomains={existingDomains}
          onSelected={handleDomainSelected}
        />
      )}

      {phase === "dns_and_verify" && domain && (
        <div className="space-y-8">
          <DNSSetupCard domain={domain} />
          <VerifyDomainCard domain={domain} onVerified={handleVerified} />
        </div>
      )}

      {phase === "create_agent" && domain && (
        <CustomAgentForm domain={domain.domain} onCreated={onComplete} />
      )}
    </div>
  );
}
