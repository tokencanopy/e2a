"use client";

import { useState } from "react";
import { Field } from "@e2a/ui";
import { createAgent } from "../../../components/onboarding/api";
import { isValidLocalPart } from "../../../components/onboarding/state";
import { track } from "../../../components/onboarding/analytics";
import type { AgentData } from "../../../components/types";
import { invalidateAgents } from "../../../../lib/swrKeys";

export function CustomAgentForm({
  domain,
  onCreated,
}: {
  domain: string;
  onCreated: (agent: AgentData) => void;
}) {
  const [localPart, setLocalPart] = useState("");
  const [name, setName] = useState("");
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");

  const email = localPart ? `${localPart}@${domain}` : "";

  const canSubmit = !loading && localPart.length > 0;

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setError("");

    if (!isValidLocalPart(localPart)) {
      setError("Local part must be lowercase letters, numbers, dots, or hyphens.");
      return;
    }

    setLoading(true);
    track("agent_creation_started", { shared_or_custom: "custom", domain });
    try {
      const result = await createAgent({
        email,
        ...(name ? { name } : {}),
      });
      track("agent_creation_succeeded", { shared_or_custom: "custom", domain });
      // Refresh the SWR `agents` cache so /inboxes shows the new
      // row immediately (otherwise keepPreviousData renders the
      // pre-create list until the next focus revalidation).
      await invalidateAgents();
      onCreated(result as AgentData);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Inbox creation failed");
    } finally {
      setLoading(false);
    }
  };

  return (
    <div>
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
        Your domain{" "}
        <code
          className="font-mono text-[12px] px-1.5 py-0.5"
          style={{
            background: "var(--bg-elev)",
            border: "1px solid var(--border-sub)",
            borderRadius: "var(--r-sm)",
            color: "var(--fg)",
          }}
        >
          {domain}
        </code>{" "}
        is verified. Create an inbox on it.
      </p>

      {error && (
        <div className="mb-6 p-3 bg-red-50 border border-red-200 rounded-lg text-sm text-red-700">
          {error}
        </div>
      )}

      <form onSubmit={handleSubmit} className="space-y-5">
        {/* Email local part */}
        <div>
          <label className="block text-sm font-medium mb-1.5">Inbox address</label>
          <div className="flex items-center gap-0">
            <input
              type="text"
              placeholder="support"
              value={localPart}
              onChange={(e) => setLocalPart(e.target.value.toLowerCase())}
              className="flex-1 px-3 py-2 rounded-l-lg border border-r-0 border-border bg-background text-sm focus:outline-none focus:ring-2 focus:ring-accent/30"
            />
            <span className="px-3 py-2 rounded-r-lg border border-border bg-surface text-sm text-muted">
              @{domain}
            </span>
          </div>
          <p className="mt-1 text-xs text-muted">The local part of the inbox address.</p>
        </div>

        {/* Display name */}
        <Field
          label="Display name"
          placeholder="Support Agent"
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
