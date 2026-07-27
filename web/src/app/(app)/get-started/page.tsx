"use client";

import { useEffect, useState } from "react";
import { useRouter, useSearchParams } from "next/navigation";
import { listDomains } from "../../components/onboarding/api";
import { track } from "../../components/onboarding/analytics";
import { PageShell } from "../../components/loft/PageShell";
import { SetupMethodChoice } from "./_components/SetupMethodChoice";
import { AddressChoice } from "./_components/AddressChoice";
import { SharedAgentForm } from "./_components/SharedAgentForm";
import { CustomDomainChecklist } from "./_components/CustomDomainChecklist";
import { AgentSetupCards } from "./_components/AgentSetupCards";
import { SuccessPanel } from "./_components/SuccessPanel";
import type { AddressType, SetupMethod } from "../../components/onboarding/types";
import type { DomainInfo } from "../../components/onboarding/types";
import type { AgentData } from "../../components/types";

// "choose" = top-level method (agent vs web); "address" = the web sub-chooser
// (shared vs custom). The agent path jumps straight from choose → agent_mcp.
type Step =
  | "choose"
  | "address"
  | "shared_form"
  | "custom_checklist"
  | "agent_mcp"
  | "success";

function isStep(value: string | null): value is Step {
  return (
    value === "choose" ||
    value === "address" ||
    value === "shared_form" ||
    value === "custom_checklist" ||
    value === "agent_mcp" ||
    value === "success"
  );
}

const PAGE_HEADER = {
  eyebrow: "Onboarding · est. 3 minutes",
  // Plain Inter heading to match the rest of the (app) pages —
  // editorial italic stays on marketing/landing surfaces only.
  title: "Set up your first inbox.",
  subtitle:
    "Claim an email address for your agent, then point e2a at where your code runs. You can change all of this later from the dashboard.",
};

export default function GetStartedPage() {
  const router = useRouter();
  const searchParams = useSearchParams();

  // The active step lives in the URL as ?step=… so the browser back
  // button moves between onboarding steps instead of leaving the page
  // entirely. Legacy entry points (?mode=shared from the domains page,
  // ?domain=… from the resume flow) are still honored — the bootstrap
  // effect below translates them to the equivalent ?step value via
  // router.replace (no extra history entry).
  const stepParam = searchParams.get("step");
  const routeStep: Step = isStep(stepParam) ? stepParam : "choose";
  const [step, setStep] = useState<Step>(routeStep);
  const initialMode = searchParams.get("mode") === "shared" ? "shared" : null;
  const initialDomain = searchParams.get("domain");

  const [method, setMethod] = useState<SetupMethod | null>(null);
  const [addressType, setAddressType] = useState<AddressType | null>(null);
  const [agent, setAgent] = useState<AgentData | null>(null);
  const [domainData, setDomainData] = useState<DomainInfo | null>(null);
  const [error, setError] = useState("");
  const [bootstrapping, setBootstrapping] = useState(true);

  // Route state remains canonical for deep links and browser navigation, but
  // forward interactions update locally first. Same-page router transitions
  // can lag briefly; deriving the rendered step only from useSearchParams made
  // a selected option appear inert until the URL commit completed.
  useEffect(() => {
    setStep(routeStep);
  }, [routeStep]);

  useEffect(() => {
    let cancelled = false;

    async function bootstrap() {
      setError("");
      setAgent(null);

      if (initialDomain) {
        setAddressType("custom");
        try {
          const domains = await listDomains();
          if (cancelled) return;

          const matchedDomain = domains.find((d) => d.domain === initialDomain);
          if (!matchedDomain) {
            setDomainData(null);
            setAddressType(null);
            setStep("choose");
            router.replace("/get-started");
            setError(`Domain ${initialDomain} not found in your account`);
          } else {
            setDomainData(matchedDomain);
            setStep("custom_checklist");
            router.replace("/get-started?step=custom_checklist");
          }
        } catch (err) {
          if (cancelled) return;
          setDomainData(null);
          setAddressType(null);
          setStep("choose");
          router.replace("/get-started");
          setError(
            err instanceof Error
              ? err.message
              : "Failed to load onboarding state",
          );
        } finally {
          if (!cancelled) setBootstrapping(false);
        }
        return;
      }

      if (initialMode === "shared") {
        setAddressType("shared");
        // Replace the legacy ?mode=shared with the canonical ?step= so
        // back from shared_form lands on the choose step rather than
        // bouncing back to ?mode=shared again.
        setStep("shared_form");
        router.replace("/get-started?step=shared_form");
        setBootstrapping(false);
        return;
      }

      setBootstrapping(false);
    }

    bootstrap();
    return () => {
      cancelled = true;
    };
    // initialDomain / initialMode are URL-derived constants for this
    // mount. router.replace is stable on Next 13+ so omitting it from
    // deps doesn't risk stale closures.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [initialDomain, initialMode]);

  // If the URL is ?step=success but local agent state is missing
  // (refresh, direct URL, back-then-forward), the rendered view falls
  // back to the chooser. Strip the stale ?step= so the URL matches —
  // otherwise a subsequent back-button press jumps to ?step=success
  // again and the same fallback fires in a loop.
  useEffect(() => {
    if (step === "success" && !agent && !bootstrapping) {
      setStep("choose");
      router.replace("/get-started");
    }
  }, [step, agent, bootstrapping, router]);

  // Top-level fork: agent (MCP) jumps straight to the headless cards; web opens
  // the shared/custom address chooser.
  const handleMethodChoice = (m: SetupMethod) => {
    setMethod(m);
    setError("");
    track("setup_method_selected", { method: m });
    const nextStep = m === "agent" ? "agent_mcp" : "address";
    setStep(nextStep);
    router.push(`/get-started?step=${nextStep}`);
  };

  const handleAddressChoice = (type: AddressType) => {
    setAddressType(type);
    setError("");
    track("address_type_selected", { type });
    setStep(type === "shared" ? "shared_form" : "custom_checklist");
    router.push(
      `/get-started?step=${type === "shared" ? "shared_form" : "custom_checklist"}`,
    );
  };

  const handleBackToChoose = () => {
    // Prefer router.back() so we navigate the browser history (matches
    // what the user expects from the browser's own Back button); fall
    // back to a push to the choose step if there's nothing to go back
    // to in the same-origin history.
    if (window.history.length > 1) {
      router.back();
    } else {
      router.push("/get-started");
    }
  };

  if (bootstrapping) {
    return (
      <PageShell>
        <p
          className="py-10 text-center text-[13px]"
          style={{ color: "var(--fg-muted)" }}
        >
          Loading onboarding...
        </p>
      </PageShell>
    );
  }

  return (
    <PageShell
      eyebrow={PAGE_HEADER.eyebrow}
      title={PAGE_HEADER.title}
      subtitle={PAGE_HEADER.subtitle}
      maxWidth={880}
    >
      {step === "choose" && (
        <>
          <SetupMethodChoice selected={method} onSelect={handleMethodChoice} />
          {error && (
            <div
              className="mt-6 p-3 text-[13px]"
              style={{
                background: "var(--danger-bg)",
                border: "1px solid var(--danger-bg)",
                color: "var(--danger-strong)",
                borderRadius: "var(--r-md)",
              }}
            >
              {error}
            </div>
          )}
        </>
      )}

      {step === "address" && (
        <>
          <button
            type="button"
            onClick={handleBackToChoose}
            className="text-[12px] mb-4 inline-flex items-center gap-1 transition"
            style={{ color: "var(--fg-muted)" }}
          >
            ← Back
          </button>
          <AddressChoice selected={addressType} onSelect={handleAddressChoice} />
        </>
      )}

      {step === "shared_form" && (
        <SharedAgentForm
          onBack={handleBackToChoose}
          onCreated={(agentData) => {
            setAgent(agentData);
            setStep("success");
            router.push("/get-started?step=success");
          }}
        />
      )}

      {step === "custom_checklist" && (
        <CustomDomainChecklist
          initialDomain={domainData}
          onBack={handleBackToChoose}
          onComplete={(agentData) => {
            setAgent(agentData);
            setStep("success");
            router.push("/get-started?step=success");
          }}
        />
      )}

      {step === "agent_mcp" && <AgentSetupCards onBack={handleBackToChoose} />}

      {/* Success is the only step that needs an agent in local state.
          If a user lands on ?step=success without state (refresh, share,
          back-then-forward), drop them back at the choose screen rather
          than rendering an empty success panel. */}
      {step === "success" && agent && <SuccessPanel agent={agent} />}
      {step === "success" && !agent && (
        <SetupMethodChoice selected={null} onSelect={handleMethodChoice} />
      )}
    </PageShell>
  );
}
