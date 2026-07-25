"use client";

import { useState, useEffect, useMemo } from "react";
import useSWR from "swr";
import Link from "next/link";
import { useAuth } from "../../components/AuthProvider";
import { AgentPromptCard, AGENT_PROMPTS } from "../../components/AgentPromptCard";
import { listDomains } from "../../components/onboarding/api";
import { canReceive } from "../../components/onboarding/state";
import { useAgents } from "../../components/hooks/useAgents";
import { domainsKey } from "../../../lib/swrKeys";
import type { DashboardAgent } from "../../components/types";
import type { DomainInfo } from "../../components/onboarding/types";
import { PageShell } from "../../components/loft/PageShell";
import { AgentsEmptyState } from "./_components/AgentsEmptyState";
import { AgentCard } from "./_components/AgentCard";

// Filter chips + sort dropdown. Counts are derived client-side from the
// agents list — the backend doesn't need to compute filter aggregates.
// "Sort: last activity" uses created_at descending as a proxy; the
// label is honest about what's available.
type Filter = "all" | "unverified";
type SortKey = "recent" | "name";

function FilterBar({
  agents,
  filter,
  setFilter,
  sort,
  setSort,
}: {
  agents: DashboardAgent[];
  filter: Filter;
  setFilter: (f: Filter) => void;
  sort: SortKey;
  setSort: (s: SortKey) => void;
}) {
  const counts = {
    all: agents.length,
    unverified: agents.filter((a) => !a.domain_verified).length,
  };
  const chips: { key: Filter; label: string; count: number }[] = [
    { key: "all", label: "All", count: counts.all },
    { key: "unverified", label: "Unverified", count: counts.unverified },
  ];

  return (
    <div className="flex items-center gap-2 mb-3.5 flex-wrap">
      {chips.map((c) => {
        const active = filter === c.key;
        return (
          <button
            key={c.key}
            onClick={() => setFilter(c.key)}
            className="text-[12px] font-medium px-3 py-1 transition"
            style={{
              borderRadius: 999,
              background: active ? "var(--fg)" : "var(--bg-panel)",
              color: active ? "var(--bg)" : "var(--fg-muted)",
              border: active
                ? "1px solid var(--fg)"
                : "1px solid var(--border)",
            }}
          >
            {c.label} {c.count}
          </button>
        );
      })}
      <span className="flex-1" />
      <label
        className="font-mono text-[11px] flex items-center gap-1.5"
        style={{ color: "var(--fg-subtle)", letterSpacing: "0.02em" }}
      >
        Sort:
        <select
          value={sort}
          onChange={(e) => setSort(e.target.value as SortKey)}
          className="font-mono text-[11px] bg-transparent border-none cursor-pointer"
          style={{ color: "var(--fg-muted)" }}
        >
          <option value="recent">last activity ▾</option>
          <option value="name">name ▾</option>
        </select>
      </label>
    </div>
  );
}

export default function DashboardPage() {
  const { user } = useAuth();
  // Both feeds are read through SWR so an agent edit on the Settings
  // page (which invalidates `agentsKey`) flows back into this view
  // without a manual refetch.
  const { agents, error: agentsError, isLoading: agentsLoading } = useAgents();
  const { data: domains = [] } = useSWR(domainsKey, () =>
    listDomains().catch(() => [] as DomainInfo[]),
  );
  const error = agentsError ? agentsError.message || "Failed to load inboxes" : "";
  const loading = agentsLoading;
  const [filter, setFilter] = useState<Filter>("all");
  const [sort, setSort] = useState<SortKey>("recent");

  // Delete moved to /inboxes/settings → Danger zone. The
  // dashboard no longer needs a per-card delete handler; the settings
  // page calls deleteAgent + invalidateAgents() and routes back here,
  // which causes useAgents() to refetch and the new list to render.

  // Derived: filtered + sorted agent list.
  const visibleAgents = useMemo(() => {
    let out = agents;
    if (filter === "unverified") {
      out = out.filter((a) => !a.domain_verified);
    }
    if (sort === "recent") {
      // created_at descending as a stand-in for last activity.
      out = [...out].sort(
        (a, b) =>
          new Date(b.created_at).getTime() - new Date(a.created_at).getTime(),
      );
    } else {
      out = [...out].sort((a, b) =>
        (a.name || a.email).localeCompare(b.name || b.email),
      );
    }
    return out;
  }, [agents, filter, sort]);

  // Meta line: "N inboxes · M verified domains · indexed <relative> ago"
  const [indexedAt, setIndexedAt] = useState<number>(Date.now());
  useEffect(() => {
    setIndexedAt(Date.now());
  }, [agents, domains]);
  const [tick, setTick] = useState(0);
  useEffect(() => {
    const id = setInterval(() => setTick((t) => t + 1), 5000);
    return () => clearInterval(id);
  }, []);
  const indexedAgo = useMemo(() => {
    const sec = Math.max(1, Math.floor((Date.now() - indexedAt) / 1000));
    if (sec < 60) return `${sec}s`;
    const min = Math.floor(sec / 60);
    if (min < 60) return `${min}m`;
    return `${Math.floor(min / 60)}h`;
  }, [indexedAt, tick]); // eslint-disable-line react-hooks/exhaustive-deps

  // Counts domains that can RECEIVE mail — the inbound axis, which is what
  // this stat has always meant. Read through the helper so it stays correct
  // once a domain can be outbound-capable without being inbound-verified.
  const verifiedDomains = domains.filter((d: DomainInfo) => canReceive(d)).length;
  const inboxLabel = agents.length === 1 ? "inbox" : "inboxes";
  const domainLabel = verifiedDomains === 1 ? "verified domain" : "verified domains";

  return (
    <PageShell
      eyebrow="Workspace"
      title={<>Inboxes</>}
      subtitle={
        <>
          {agents.length} {inboxLabel} · {verifiedDomains} {domainLabel} ·
          indexed{" "}
          <span style={{ fontFamily: "var(--f-mono)" }}>
            {indexedAgo} ago
          </span>{" "}
          · signed in as{" "}
          <span style={{ color: "var(--fg)", fontWeight: 500 }}>
            {user?.email}
          </span>
        </>
      }
      actions={
        agents.length > 0 ? (
          <Link
            href="/get-started"
            className="inline-flex items-center gap-1.5 text-[13px] font-medium px-4 py-2"
            style={{
              background: "var(--accent-fill)",
              color: "var(--accent-fg)",
              borderRadius: "var(--r-md)",
            }}
          >
            <span className="font-mono">+</span> Create inbox
          </Link>
        ) : null
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

      <div className="mb-10">
        <AgentPromptCard {...AGENT_PROMPTS.inboxes} />
      </div>

      {loading ? (
        <div
          className="text-[13px] py-12 text-center"
          style={{ color: "var(--fg-muted)" }}
        >
          Loading...
        </div>
      ) : agents.length === 0 ? (
        <AgentsEmptyState />
      ) : (
        <>
          <FilterBar
            agents={agents}
            filter={filter}
            setFilter={setFilter}
            sort={sort}
            setSort={setSort}
          />
          <div className="space-y-4">
            {visibleAgents.length === 0 ? (
              <p
                className="text-[13px] py-8 text-center"
                style={{ color: "var(--fg-muted)" }}
              >
                No inboxes match this filter.
              </p>
            ) : (
              visibleAgents.map((agent) => (
                <AgentCard key={agent.email} agent={agent} />
              ))
            )}
          </div>
        </>
      )}
    </PageShell>
  );
}
