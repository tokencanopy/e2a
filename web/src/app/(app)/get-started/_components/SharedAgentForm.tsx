"use client";

import { useState } from "react";
import { Field } from "@e2a/ui";
import { createAgent } from "../../../components/onboarding/api";
import { isValidSlug } from "../../../components/onboarding/state";
import { track } from "../../../components/onboarding/analytics";
import type { AgentData } from "../../../components/types";
import { AGENTS_DOMAIN, AGENTS_DOMAIN_DISPLAY } from "../../../../lib/site";
import { invalidateAgents } from "../../../../lib/swrKeys";

export function SharedAgentForm({
  onCreated,
  onBack,
}: {
  onCreated: (agent: AgentData) => void;
  /** Returns the user to the address-type chooser. Wired by the parent
   * to router.back() so the browser history stays consistent. */
  onBack?: () => void;
}) {
  const [slug, setSlug] = useState("");
  const [name, setName] = useState("");
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");

  const canSubmit = !loading && slug.length > 0;

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setError("");

    if (!isValidSlug(slug)) {
      setError("Slug must be 2\u201340 lowercase characters (letters, numbers, hyphens). No leading/trailing hyphens.");
      return;
    }

    // /v1 registers agents by full email; a shared-domain agent is just an
    // email on the deployment's shared domain (the legacy `slug` field is
    // gone). AGENTS_DOMAIN empty means the deployment has no shared domain,
    // so this form can't produce a valid address.
    if (!AGENTS_DOMAIN) {
      setError("This deployment has no shared agent domain configured.");
      return;
    }

    setLoading(true);
    track("agent_creation_started", { shared_or_custom: "shared" });
    try {
      const result = await createAgent({
        email: `${slug}@${AGENTS_DOMAIN}`,
        ...(name ? { name } : {}),
      });
      track("agent_creation_succeeded", { shared_or_custom: "shared" });
      // Refresh the SWR `agents` cache so /inboxes shows the new
      // row immediately (otherwise keepPreviousData renders the
      // pre-create list until the next focus revalidation).
      await invalidateAgents();
      onCreated(result as AgentData);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Registration failed");
    } finally {
      setLoading(false);
    }
  };

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
      <h2
        className="mb-2"
        style={{
          fontFamily: "var(--f-ui)",
          fontWeight: 600,
          fontSize: 30,
          letterSpacing: "-0.01em",
          color: "var(--fg)",
        }}
      >
        Create your inbox
      </h2>
      <p className="mb-7 text-[14px]" style={{ color: "var(--fg-muted)" }}>
        Choose a slug for your inbox on the shared e2a domain.
      </p>

      {/* How it works */}
      <div className="mb-8 p-4 bg-blue-50/50 border border-accent/20 rounded-lg text-sm text-muted space-y-3">
        <p className="font-medium text-foreground mb-1">How it works</p>
        <p>
          Your inbox gets{" "}
          <code className="text-xs bg-white/60 px-1 py-0.5 rounded break-all">
            {slug || "your-slug"}@{AGENTS_DOMAIN_DISPLAY}
          </code>
          . Emails arrive in real time via CLI, SDK, or WebSocket, or to an
          account webhook you configure under Webhook secrets.
        </p>
      </div>

      {error && (
        <div className="mb-6 p-3 bg-red-50 border border-red-200 rounded-lg text-sm text-red-700">
          {error}
        </div>
      )}

      <form onSubmit={handleSubmit} className="space-y-5">
        {/* Slug */}
        <Field
          label="Slug"
          placeholder="my-agent"
          value={slug}
          onChange={(v) => setSlug(v.toLowerCase())}
          hint={`Your inbox\u2019s email will be ${slug || "slug"}@${AGENTS_DOMAIN_DISPLAY}`}
        />

        {/* Display name */}
        <Field
          label="Display name"
          placeholder="My Agent"
          value={name}
          onChange={setName}
          hint="Shown in the From header of outbound emails (optional)"
        />

        <button
          type="submit"
          disabled={!canSubmit}
          className="w-full bg-foreground text-background py-3 rounded-lg text-sm font-medium hover:opacity-90 transition disabled:opacity-50 disabled:cursor-not-allowed"
        >
          {loading ? "Creating..." : "Create inbox"}
        </button>
      </form>
    </div>
  );
}
