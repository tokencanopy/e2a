"use client";

import { useState } from "react";
import type { AgentData } from "../../../components/types";

type TestState = "idle" | "sending" | "delivered";

export function SuccessPanel({
  agent,
}: {
  agent: AgentData;
}) {
  const [testState, setTestState] = useState<TestState>("idle");
  const [sendError, setSendError] = useState("");

  async function sendTestEmail() {
    setSendError("");
    setTestState("sending");
    try {
      const res = await fetch(`/v1/agents/${encodeURIComponent(agent.email)}/test`, {
        method: "POST",
        credentials: "include",
      });
      if (res.ok) {
        setTestState("delivered");
      } else {
        const msg = await res.text();
        console.error("Test email failed:", res.status, msg);
        setSendError(msg || `Request failed (${res.status})`);
        setTestState("idle");
      }
    } catch (err) {
      console.error("Failed to send test email:", err);
      setSendError("Network error — check your connection and try again.");
      setTestState("idle");
    }
  }

  return (
    <div>
      <div
        className="mb-6 p-4 text-[13px]"
        style={{
          background: "var(--success-bg)",
          border: "1px solid var(--success-bg)",
          color: "var(--success)",
          borderRadius: "var(--r-md)",
        }}
      >
        <span className="font-semibold">Inbox created!</span>{" "}
        Your inbox&apos;s email is{" "}
        <code
          className="font-mono text-[12px] px-1.5 py-0.5 break-all"
          style={{
            background: "var(--bg-panel)",
            color: "var(--fg)",
            borderRadius: "var(--r-sm)",
          }}
        >
          {agent.email}
        </code>
      </div>

      <div className="mb-8">
        {testState === "idle" && (
          <button
            type="button"
            onClick={sendTestEmail}
            className="w-full bg-foreground text-background py-3 rounded-lg text-sm font-medium hover:opacity-90 transition"
          >
            Send a test email to {agent.email} →
          </button>
        )}
        {testState === "sending" && (
          <button
            type="button"
            disabled
            className="w-full bg-surface text-muted py-3 rounded-lg text-sm font-medium border border-border cursor-not-allowed"
          >
            Sending…
          </button>
        )}
        {testState === "delivered" && (
          <>
            <button
              type="button"
              disabled
              className="w-full bg-green-600 text-white py-3 rounded-lg text-sm font-medium flex items-center justify-center gap-2 cursor-default"
            >
              <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" strokeLinejoin="round">
                <polyline points="20 6 9 17 4 12" />
              </svg>
              Test email is on its way to {agent.email}
            </button>
            <p className="mt-3 text-sm text-muted text-center">
              Connect your application or agent below to receive it. It will be the first message that arrives.
            </p>
          </>
        )}
        {sendError && (
          <p className="mt-3 text-sm text-red-600">{sendError}</p>
        )}
      </div>

      <h2
        className="mb-2"
        style={{
          fontFamily: "var(--f-ui)",
          fontWeight: 600,
          fontSize: 28,
          letterSpacing: "-0.01em",
          color: "var(--fg)",
        }}
      >
        Connect your application or agent
      </h2>
      <p className="mb-6 text-[14px]" style={{ color: "var(--fg-muted)" }}>
        Use this address from any backend over the HTTP API or an SDK. If you are building an agent, you can also install the e2a skill for inbox tools.
      </p>

      <ApiKeyNote />

      <div className="space-y-4">
        <h3 className="text-[15px] font-semibold" style={{ color: "var(--fg)" }}>
          Agent tools <span className="font-normal" style={{ color: "var(--fg-muted)" }}>(optional)</span>
        </h3>
        <CodeBlock
          title="Install the e2a skill"
          code={`mkdir -p .claude/skills/e2a\ncurl -o .claude/skills/e2a/SKILL.md \\\n  https://raw.githubusercontent.com/tokencanopy/e2a/main/plugins/e2a/skills/e2a/SKILL.md`}
        />
        <p className="text-sm text-muted">
          Then use{" "}
          <code className="bg-surface px-1.5 py-0.5 rounded border border-border text-xs">/e2a</code>{" "}
          in your agent to get started. The skill walks through login, agent registration, and listening for emails automatically.
        </p>
      </div>

      <a
        href="/inboxes"
        className="mt-8 block w-full text-center bg-foreground text-background py-3 rounded-lg text-sm font-medium hover:opacity-90 transition"
      >
        Go to Inboxes
      </a>
    </div>
  );
}

function ApiKeyNote() {
  return (
    <div className="mb-6 p-4 border border-border rounded-lg text-sm text-muted">
      <p>
        <span className="font-medium text-foreground">Application API.</span>{" "}
        Create an{" "}
        <a href="/api-keys" className="text-accent hover:underline">API Keys</a>{" "}
        and use it with the HTTP API, TypeScript SDK, or Python SDK. The CLI can
        also create and save a key automatically via{" "}
        <code className="bg-surface px-1 py-0.5 rounded border border-border">e2a login</code>.
        {" "}Full docs:{" "}
        <a href="https://www.npmjs.com/package/@e2a/cli" target="_blank" rel="noopener noreferrer" className="text-accent hover:underline">CLI</a>,{" "}
        <a href="/api-docs" className="text-accent hover:underline">API</a>,{" "}
        <a href="https://pypi.org/project/e2a/" target="_blank" rel="noopener noreferrer" className="text-accent hover:underline">Python SDK</a>.
      </p>
    </div>
  );
}

function CodeBlock({ title, code }: { title?: string; code: string }) {
  const [copied, setCopied] = useState(false);
  const copy = () => {
    navigator.clipboard.writeText(code);
    setCopied(true);
    setTimeout(() => setCopied(false), 2000);
  };

  return (
    <div>
      {title && (
        <p
          className="text-[12px] font-medium mb-2"
          style={{ color: "var(--fg)" }}
        >
          {title}
        </p>
      )}
      <div className="relative group">
        <pre
          className="font-mono text-[12.5px] p-4 overflow-x-auto leading-[1.6]"
          style={{
            background: "var(--ink)",
            color: "var(--ink-fg)",
            border: "1px solid var(--ink-border)",
            borderRadius: "var(--r-lg)",
          }}
        >
          <code>{code}</code>
        </pre>
        <button
          type="button"
          onClick={copy}
          className="absolute top-2 right-2 px-2 py-1 text-[10px] font-medium font-mono transition opacity-0 group-hover:opacity-100"
          style={{
            background: "var(--ink-elev)",
            color: "var(--ink-fg-muted)",
            border: "1px solid var(--ink-border)",
            borderRadius: "var(--r-sm)",
          }}
        >
          {copied ? "Copied!" : "Copy"}
        </button>
      </div>
    </div>
  );
}
