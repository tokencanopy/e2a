"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { Button } from "./loft/Button";

// Copy-paste prompts for driving each workspace surface from a coding
// agent (Claude Code, Cursor, …) instead of the dashboard. Each prompt
// names the desired outcome and points the agent at the hosted MCP server.
export const AGENT_PROMPTS = {
  templates: {
    blurb:
      "Templates are a one-time setup your coding agent can do headlessly — paste this into Claude Code, Cursor, or any agent connected to e2a.",
    prompt: "Help me set up e2a email templates using https://api.e2a.dev/mcp",
  },
  inboxes: {
    blurb:
      "Creating and wiring an inbox is a one-time setup your coding agent can do headlessly — paste this into Claude Code, Cursor, or any agent connected to e2a.",
    prompt: "Help me set up an e2a inbox using https://api.e2a.dev/mcp",
    notice: {
      label: "Want every outbound email reviewed first?",
      text: "Ask your agent: “Configure this inbox so every outbound email requires human review.”",
    },
  },
  domains: {
    blurb:
      "Domain setup is a one-time task your coding agent can drive end to end — paste this into Claude Code, Cursor, or any agent connected to e2a.",
    prompt:
      "Help me connect a custom domain to e2a using https://api.e2a.dev/mcp",
    // The agent can create the domain in e2a over MCP, but the DNS records
    // land at the registrar — out of reach unless the agent also has an MCP
    // server for the provider. Surfacing that here, next to the prompt,
    // means the human installs it before the agent stalls halfway through.
    notice: {
      label: "Domain managed by Cloudflare?",
      text: "Add Cloudflare’s MCP server to the same agent and it can create the DNS records for you too.",
      link: {
        href: "https://github.com/cloudflare/mcp",
        text: "https://github.com/cloudflare/mcp",
      },
    },
  },
  contacts: {
    blurb:
      "Your coding agent can import contacts, enroll them with an inbox, and work outreach over MCP while it is running.",
    prompt:
      "Help me set up e2a contacts and outreach using https://api.e2a.dev/mcp",
  },
  // Webhooks are two jobs, not one: register the subscription, and write a
  // handler that verifies the HMAC signature. The prompt names both so the
  // agent doesn't stop at a subscription pointing to an unwritten endpoint.
  // The notice pushes scoping — an unfiltered subscription receives every
  // inbox's mail, which is easy to create and easy to not notice.
  webhooks: {
    blurb:
      "Registering a webhook and wiring a signature-verifying handler is a one-time setup your coding agent can do end to end — paste this into Claude Code, Cursor, or any agent connected to e2a.",
    prompt:
      "Help me set up an e2a webhook using https://api.e2a.dev/mcp — register the subscription, then wire up a handler that verifies the signature with the e2a SDK.",
    notice: {
      label: "Only want events from certain inboxes?",
      text: "Ask your agent: “Scope this webhook to only the inboxes it should receive — an unscoped subscription gets every inbox on the account.”",
    },
  },
} as const;

export type AgentPromptCardProps = {
  blurb: string;
  prompt: string;
  notice?: {
    label: string;
    // Rendered verbatim after the label, so each card phrases its own nudge
    // (an "Ask your agent: …" instruction, a prerequisite to install, …).
    text: string;
    // Optional trailing link for a notice that points somewhere.
    link?: { href: string; text: string };
  };
};

// A copy-paste prompt card: these surfaces are one-time-setup shaped, so
// the card hands the developer's coding agent the whole job instead of
// walking the human through clicks.
export function AgentPromptCard({
  blurb,
  prompt,
  notice,
}: AgentPromptCardProps) {
  const [copied, setCopied] = useState(false);
  const copyTimer = useRef<ReturnType<typeof setTimeout> | null>(null);

  useEffect(() => {
    return () => {
      if (copyTimer.current !== null) clearTimeout(copyTimer.current);
    };
  }, []);

  const onCopy = useCallback(async () => {
    try {
      await navigator.clipboard.writeText(prompt);
      setCopied(true);
      if (copyTimer.current !== null) clearTimeout(copyTimer.current);
      copyTimer.current = setTimeout(() => {
        setCopied(false);
        copyTimer.current = null;
      }, 1200);
    } catch {
      // clipboard unavailable — silently ignore
    }
  }, [prompt]);

  return (
    <section
      aria-label="Set up with a coding agent"
      className="rounded-[var(--r-lg)] border p-5"
      style={{ background: "var(--bg-panel)", borderColor: "var(--border)" }}
    >
      {/* Only the heading shares the row with the button. Keeping the blurb
          inside that flex row forced it to wrap against the button's gutter,
          so its right edge never lined up with the <pre> and notice below —
          which span the full card. Lifting it out gives all three blocks the
          same left AND right edge. */}
      <div className="flex items-start justify-between gap-4">
        <h2
          className="text-[16px] font-semibold min-w-0"
          style={{ color: "var(--fg)", margin: 0 }}
        >
          Set up with a coding agent
        </h2>
        <Button variant="ghost" onClick={onCopy} aria-label="Copy prompt">
          {copied ? "Copied" : "Copy prompt"}
        </Button>
      </div>
      <p
        className="text-[13px] mt-1 mb-0 leading-[1.6]"
        style={{ color: "var(--fg-muted)" }}
      >
        {blurb}
      </p>
      <pre
        className="mt-4 mb-0 whitespace-pre-wrap rounded-[var(--r-md)] border p-3.5 text-[12px] leading-[1.7]"
        style={{
          fontFamily: "var(--f-mono)",
          color: "var(--fg-muted)",
          background: "var(--bg)",
          borderColor: "var(--border)",
        }}
      >
        {prompt}
      </pre>
      {notice ? (
        <aside
          role="note"
          // Each card carries a different nudge, so the accessible name comes
          // from the notice itself rather than a single hardcoded label.
          aria-label={notice.label}
          className="mt-3 rounded-[var(--r-md)] border px-3.5 py-3 text-[12px] leading-[1.6]"
          style={{
            color: "var(--fg-muted)",
            background: "var(--bg)",
            borderColor: "var(--border)",
          }}
        >
          <span className="font-semibold" style={{ color: "var(--fg)" }}>
            {notice.label}
          </span>{" "}
          {notice.text}
          {notice.link ? (
            <>
              {" "}
              <a
                href={notice.link.href}
                target="_blank"
                rel="noopener noreferrer"
                className="underline"
                style={{ color: "var(--fg)" }}
              >
                {notice.link.text}
              </a>
            </>
          ) : null}
        </aside>
      ) : null}
    </section>
  );
}
