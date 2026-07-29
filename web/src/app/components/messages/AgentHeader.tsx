"use client";

// Slimmed per-agent header for the dashboard messages view.
// v2 removed the 5-up stats strip (those signals belong on Overview)
// and the Conversations tab (Messages is threaded by default).
//
// Layout: identity row (avatar + name + email chip + Verified/Cloud/HITL
// chips) + mono meta sub-line + tab strip (Overview · Messages ·
// Webhooks · Settings). Only Messages is active in this slice;
// the other tabs render as disabled placeholders with a tooltip.

import { useState } from "react";
import Link from "next/link";
import { Chip, Dot, Eyebrow } from "@e2a/ui";
import { CounterpartyAvatar } from "./CounterpartyAvatar";
import { sendAgentTestEmail } from "../onboarding/api";
import type { DashboardAgent } from "../types";

export type AgentTab = "messages" | "outreach" | "trash" | "settings";

// The agent-detail surface is intentionally scoped to two tabs:
//   • Messages — the threaded inbox with inline conversation detail.
//   • Settings — per-agent editors (mode, webhook URL, HITL config,
//     delete).
// Overview + Webhooks were considered and dropped: Overview duplicated
// the dashboard agent card; Webhooks folded into Settings. When a
// third tab is added, restore the `ready` flag + disabled-tab branch
// (see git history at 63876fc).
const TABS: { key: AgentTab; label: string; slug: string }[] = [
  { key: "messages", label: "Inbox", slug: "messages" },
  { key: "outreach", label: "Outreach", slug: "outreach" },
  { key: "trash", label: "Trash", slug: "trash" },
  { key: "settings", label: "Settings", slug: "settings" },
];

export function AgentHeader({
  agent,
  tab,
}: {
  agent: DashboardAgent;
  tab: AgentTab;
}) {
  const emailQs = encodeURIComponent(agent.email);
  const [testState, setTestState] = useState<"idle" | "sending" | "sent">("idle");
  const [testError, setTestError] = useState("");

  return (
    <div
      style={{
        padding: "20px 28px 0",
        background: "var(--bg-panel)",
        borderBottom: "1px solid var(--border)",
      }}
    >
      {/* Identity row */}
      <div className="flex flex-col md:flex-row md:items-start md:justify-between gap-3 mb-3">
        <div className="min-w-0 flex-1">
          <Eyebrow>Inbox</Eyebrow>
          <div className="flex items-center gap-3 mt-2 mb-1.5 flex-wrap">
            <CounterpartyAvatar email={agent.email} name={agent.name} size={28} />
            <h1
              style={{
                fontFamily: "var(--f-ui)",
                fontSize: 22,
                fontWeight: 700,
                letterSpacing: "-0.012em",
                color: "var(--fg)",
                margin: 0,
              }}
            >
              {agent.name || agent.email.split("@")[0]}
            </h1>
            <code
              style={{
                fontFamily: "var(--f-mono)",
                fontSize: 13,
                fontWeight: 500,
                color: "var(--fg)",
                background: "var(--bg-elev)",
                padding: "3px 9px",
                borderRadius: "var(--r-sm)",
                border: "1px solid var(--border-sub)",
                maxWidth: "100%",
                minWidth: 0,
                wordBreak: "break-all",
              }}
            >
              {agent.email}
            </code>
            {agent.domain_verified ? (
              <Chip tone="success">
                <Dot tone="success" /> Verified
              </Chip>
            ) : (
              <Chip tone="warn">
                <Dot tone="warn" /> Unverified
              </Chip>
            )}
          </div>
          <div
            style={{
              fontFamily: "var(--f-mono)",
              fontSize: 11,
              color: "var(--fg-subtle)",
              letterSpacing: "0.02em",
            }}
          >
            created {new Date(agent.created_at).toLocaleDateString(undefined, {
              month: "short",
              day: "numeric",
            })}
          </div>
        </div>

        {/* Send a test message — exercises outbound SMTP + inbound
            delivery + the webhook/WS round-trip for this inbox. Verified
            inboxes only. */}
        {agent.domain_verified && (
          <div className="shrink-0 md:text-right mt-2 md:mt-7">
            <button
              onClick={async () => {
                setTestError("");
                setTestState("sending");
                try {
                  await sendAgentTestEmail(agent.email);
                  setTestState("sent");
                  setTimeout(() => setTestState("idle"), 3000);
                } catch (err) {
                  setTestError(
                    err instanceof Error ? err.message : "Network error",
                  );
                  setTestState("idle");
                }
              }}
              disabled={testState === "sending"}
              className={`text-[12px] px-3 py-1.5 transition cursor-pointer disabled:cursor-not-allowed${
                testState === "idle"
                  ? " hover:bg-[var(--bg-elev)] hover:border-[var(--border-strong)]"
                  : ""
              }`}
              style={{
                background:
                  testState === "sent"
                    ? "var(--success)"
                    : testState === "sending"
                      ? "var(--bg-elev)"
                      : "var(--bg-panel)",
                color:
                  testState === "sent"
                    ? "#fff"
                    : testState === "sending"
                      ? "var(--fg-muted)"
                      : "var(--fg)",
                border: "1px solid var(--border)",
                borderRadius: "var(--r-md)",
              }}
            >
              {testState === "sent"
                ? "Sent ✓"
                : testState === "sending"
                  ? "Sending…"
                  : "Send a test message"}
            </button>
            {testError && (
              <p
                className="text-[12px] mt-1.5"
                style={{ color: "var(--danger-strong)" }}
              >
                {testError}
              </p>
            )}
          </div>
        )}
      </div>

      {/* Tab strip */}
      <div className="flex items-center gap-1 mt-1">
        {TABS.map((t) => {
          const active = t.key === tab;
          const baseStyle = {
            padding: "10px 14px 12px",
            fontSize: 13,
            fontWeight: active ? 600 : 400,
            color: active ? "var(--fg)" : "var(--fg-muted)",
            borderBottom: active
              ? "2px solid var(--accent)"
              : "2px solid transparent",
            marginBottom: -1,
            textDecoration: "none",
          } as const;
          const href = `/inboxes/${t.slug}?email=${emailQs}`;
          return (
            <Link
              key={t.key}
              href={href}
              aria-current={active ? "page" : undefined}
              style={baseStyle}
            >
              {t.label}
            </Link>
          );
        })}
      </div>
    </div>
  );
}
