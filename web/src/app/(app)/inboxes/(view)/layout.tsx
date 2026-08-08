"use client";

// Shared chrome for every per-agent route under /inboxes/.
// Reads `?email=` from the URL — we use a query param instead of a
// path segment because web/ is statically exported (next.config.ts:9)
// and dynamic segments would require generateStaticParams() with the
// full set of emails enumerated at build time.

import { usePathname, useSearchParams } from "next/navigation";
import { AgentHeader, type AgentTab } from "../../../components/messages/AgentHeader";
import { useAgents } from "../../../components/hooks/useAgents";

function detectTab(pathname: string): AgentTab {
  if (pathname.startsWith("/inboxes/settings")) return "settings";
  if (pathname.startsWith("/inboxes/outreach")) return "outreach";
  if (pathname.startsWith("/inboxes/suppressions")) return "suppressions";
  if (pathname.startsWith("/inboxes/trash")) return "trash";
  // Default to messages — the only other live tab today, and the
  // canonical landing destination from the dashboard's "Open inbox →"
  // CTA. Any unknown sub-path under /inboxes/ (404s aside)
  // also lands here so the AgentHeader has a sensible active state.
  return "messages";
}

export default function AgentLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  const pathname = usePathname();
  const searchParams = useSearchParams();
  const email = searchParams.get("email") ?? "";
  const tab = detectTab(pathname ?? "");

  // The inner content remounts via `key={email}` whenever the URL's
  // email param changes — that's the canonical React way to reset
  // useState across a dependency boundary without setState-in-effect.
  return (
    <div className="flex flex-col" data-app-surface>
      <AgentLayoutContent key={email} email={email} tab={tab}>
        {children}
      </AgentLayoutContent>
    </div>
  );
}

function AgentLayoutContent({
  email,
  tab,
  children,
}: {
  email: string;
  tab: AgentTab;
  children: React.ReactNode;
}) {
  const { agents, error: fetchError, isLoading } = useAgents();
  const agent = email ? agents.find((a) => a.email === email) ?? null : null;

  // Three distinct error states surfaced as one string:
  //   1. Missing ?email= → URL-shape problem
  //   2. The fetch itself errored
  //   3. The fetch returned successfully but the agent isn't in the list
  const error = !email
    ? "Missing ?email= query parameter"
    : fetchError
      ? fetchError.message || "Failed to load agent"
      : !isLoading && !agent
        ? `Agent ${email} not found`
        : "";
  const loading = email && !error && isLoading && !agent;

  if (error) {
    return (
      <div
        className="m-6 p-4 text-[13px]"
        style={{
          background: "var(--danger-bg)",
          border: "1px solid var(--danger-bg)",
          color: "var(--danger-strong)",
          borderRadius: "var(--r-md)",
        }}
      >
        {error}
      </div>
    );
  }
  if (loading) {
    return (
      <div
        className="px-7 py-8 text-[13px]"
        style={{ color: "var(--fg-muted)" }}
      >
        Loading agent…
      </div>
    );
  }
  if (!agent) return null;
  return (
    <>
      <AgentHeader agent={agent} tab={tab} />
      {children}
    </>
  );
}
