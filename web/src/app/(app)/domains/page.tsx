"use client";

import { useState } from "react";
import useSWR from "swr";
import { DomainList } from "./_components/DomainList";
import { AddDomainForm } from "./_components/AddDomainForm";
import { listDomains, listAgents } from "../../components/onboarding/api";
import type { DomainInfo } from "../../components/onboarding/types";
import type { DashboardAgent } from "../../components/types";
import { PageShell } from "../../components/loft/PageShell";
import { useAccountReadOnly } from "../../components/hooks/useAccountReadOnly";
import { ACCOUNT_READ_ONLY_CONTROL_TITLE } from "../../../lib/readOnly";
import { AgentPromptCard, AGENT_PROMPTS } from "../../components/AgentPromptCard";
import {
  agentsKey,
  domainsKey,
  invalidateAgents,
  invalidateDomains,
} from "../../../lib/swrKeys";

export default function DomainsPage() {
  const readOnly = useAccountReadOnly();
  const [showAddForm, setShowAddForm] = useState(false);
  // Share the cache with the dashboard's verified-domains stat
  // (useSWR(domainsKey) on /inboxes) so any mutation here flows
  // through to that surface in the same tick. Before this migration
  // the page kept its own useState copy and the dashboard's count
  // stayed stale until tab-focus revalidation eventually caught up.
  const {
    data: domains = [],
    error: domainsError,
    isLoading: domainsLoading,
    mutate: refetchDomains,
  } = useSWR<DomainInfo[]>(domainsKey, () => listDomains());
  const {
    data: agents = [],
    isLoading: agentsLoading,
    mutate: refetchAgents,
  } = useSWR<DashboardAgent[]>(agentsKey, () => listAgents());

  const loading = (domainsLoading || agentsLoading) && domains.length === 0;
  const error = domainsError
    ? domainsError instanceof Error
      ? domainsError.message
      : "Failed to load data"
    : "";

  // After a child component finishes a domain mutation (register /
  // verify / delete / set-primary) it calls onRefresh; we trigger
  // SWR revalidation via the hook-supplied mutate function so this
  // component re-renders against fresh data immediately. We also
  // fire the global invalidateDomains/invalidateAgents helpers so
  // OTHER surfaces subscribed to the same keys (dashboard's
  // verified-domains stat, agent cards) refresh in the same tick.
  // The hook-scoped mutate is what guarantees the local re-render —
  // global mutate alone wouldn't update an SWRConfig-scoped cache
  // (e.g. in tests with a fresh-Map provider).
  const handleDomainRegistered = () => {
    setShowAddForm(false);
    void refetchDomains();
    void refetchAgents();
    void invalidateDomains();
    void invalidateAgents();
  };

  const handleListChanged = () => {
    void refetchDomains();
    void refetchAgents();
    void invalidateDomains();
    void invalidateAgents();
  };

  // Empty state
  if (!loading && domains.length === 0 && !error) {
    return (
      <PageShell
        eyebrow="Workspace"
        title={<>Domains</>}
        subtitle="Add a domain to create branded inbox addresses."
      >
        <div
          className="p-8 text-center space-y-4"
          style={{
            background: "var(--bg-panel)",
            border: "1px solid var(--border)",
            borderRadius: "var(--r-lg)",
          }}
        >
          <p className="text-[14px]" style={{ color: "var(--fg-muted)" }}>
            You don&apos;t have any domains yet.
          </p>
          <div className="flex flex-col items-center gap-3">
            <button
              onClick={() => setShowAddForm(true)}
              disabled={readOnly}
              title={readOnly ? ACCOUNT_READ_ONLY_CONTROL_TITLE : undefined}
              className="px-4 py-2 text-[13px] font-medium transition disabled:opacity-50"
              style={{
                background: "var(--accent-fill)",
                color: "var(--accent-fg)",
                borderRadius: "var(--r-md)",
              }}
            >
              Add domain
            </button>
            <a
              href="/get-started?mode=shared"
              className="text-[13px] underline"
              style={{ color: "var(--accent-strong)" }}
            >
              Use shared e2a domain instead
            </a>
          </div>
        </div>

        <div className="mt-10">
          <AgentPromptCard {...AGENT_PROMPTS.domains} />
        </div>

        {showAddForm && (
          <div className="mt-6">
            <AddDomainForm onRegistered={handleDomainRegistered} />
          </div>
        )}
      </PageShell>
    );
  }

  return (
    <PageShell
      eyebrow="Workspace"
      title={<>Domains</>}
      subtitle="Manage your domains. Verified domains can host inbox addresses."
      actions={
        <button
          onClick={() => setShowAddForm(!showAddForm)}
          disabled={readOnly && !showAddForm}
          title={readOnly ? ACCOUNT_READ_ONLY_CONTROL_TITLE : undefined}
          className="px-4 py-2 text-[13px] font-medium transition disabled:opacity-50"
          style={{
            background: showAddForm ? "var(--bg-panel)" : "var(--accent-fill)",
            color: showAddForm ? "var(--fg)" : "var(--accent-fg)",
            border: showAddForm ? "1px solid var(--border)" : "none",
            borderRadius: "var(--r-md)",
          }}
        >
          {showAddForm ? "Cancel" : "Add domain"}
        </button>
      }
    >
      {error && (
        <div
          className="mb-6 p-3 text-[13px]"
          style={{
            background: "var(--danger-bg)",
            color: "var(--danger-strong)",
            border: "1px solid var(--danger-bg)",
            borderRadius: "var(--r-md)",
          }}
        >
          {error}
        </div>
      )}

      {showAddForm && (
        <div className="mb-6">
          <AddDomainForm onRegistered={handleDomainRegistered} />
        </div>
      )}

      <div className="mb-10">
        <AgentPromptCard {...AGENT_PROMPTS.domains} />
      </div>

      {loading ? (
        <div
          className="text-[13px] py-12 text-center"
          style={{ color: "var(--fg-muted)" }}
        >
          Loading...
        </div>
      ) : (
        <DomainList domains={domains} agents={agents} onRefresh={handleListChanged} />
      )}
    </PageShell>
  );
}
