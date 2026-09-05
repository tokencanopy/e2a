"use client";

// Per-agent Settings — the review-queue (HITL) editor plus a danger
// zone for deletion. The dashboard card is now lean: identity +
// Open-inbox / Settings CTAs only; the editor lives here.

import { Suspense, useState } from "react";
import { useRouter, useSearchParams } from "next/navigation";
import useSWR from "swr";
import { Eyebrow } from "@e2a/ui";
import { deleteAgent, getProtection } from "../../../../components/onboarding/api";
import { useAgents } from "../../../../components/hooks/useAgents";
import {
  invalidateAgents,
  invalidateAgentUnread,
  invalidateDeletedAgents,
  invalidateProtection,
  protectionKey,
} from "../../../../../lib/swrKeys";
import { ProtectionEditor } from "../../_components/ProtectionEditor";

// Suspense-wrap so useSearchParams stays inside a Suspense boundary
// per Next.js 16+ requirements. Inner content is keyed by email so
// navigating between agents (?email=A → ?email=B) re-mounts the
// editors with fresh state — without the key, useState would persist
// the previous agent's settings while a new fetch was in flight.
export default function AgentSettingsPage() {
  return (
    <Suspense fallback={null}>
      <AgentSettingsRouter />
    </Suspense>
  );
}

function AgentSettingsRouter() {
  const searchParams = useSearchParams();
  const email = searchParams.get("email") ?? "";
  return <AgentSettingsContent key={email} email={email} />;
}

function AgentSettingsContent({ email }: { email: string }) {
  const router = useRouter();
  const { agents, error: fetchError, isLoading } = useAgents();
  const agent = email ? agents.find((a) => a.email === email) ?? null : null;
  const [deleting, setDeleting] = useState(false);
  const [deleteError, setDeleteError] = useState("");

  // Review-queue config lives on the beta protection sub-resource
  // (GET /v1/agents/{email}/protection → holds.{ttl_seconds,on_expiry}),
  // not on the agent identity. Fetched separately, keyed by email.
  const { data: protection } = useSWR(
    agent ? protectionKey(agent.email) : null,
    () => getProtection(agent!.email),
  );

  // Three error states surfaced as one string:
  //   1. Missing ?email= → URL-shape problem
  //   2. The fetch errored
  //   3. The fetch succeeded but the agent isn't in the list
  const error = !email
    ? "Missing ?email= query parameter"
    : fetchError
      ? fetchError.message || "Failed to load inbox"
      : !isLoading && !agent
        ? `Inbox ${email} not found`
        : "";

  // After the Review-queue editor saves, refetch the protection config
  // so the collapsed summary reflects the new TTL / on-expiry.
  const onEditorSaved = () => {
    if (agent) void invalidateProtection(agent.email);
  };

  const onDelete = async () => {
    if (!agent) return;
    if (
      !confirm(
        `Move inbox ${agent.email} to the trash? It stops receiving mail immediately. You can restore it from the Trash page for 30 days.`,
      )
    )
      return;
    setDeleting(true);
    setDeleteError("");
    try {
      await deleteAgent(agent.email);
      void invalidateAgentUnread(agent.email);
      await invalidateAgents();
      // The inbox now lives in the trash — a mounted /trash page is stale.
      void invalidateDeletedAgents();
      // replace, not push: the inbox is gone, so Back must not return to
      // this now-dead settings page.
      router.replace("/inboxes");
    } catch (err) {
      setDeleteError(err instanceof Error ? err.message : "Failed to delete inbox");
      setDeleting(false);
    }
  };

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
  if (isLoading || !agent) {
    return (
      <div className="px-7 py-8 text-[13px]" style={{ color: "var(--fg-muted)" }}>
        Loading settings…
      </div>
    );
  }

  return (
    <div
      data-testid="agent-settings"
      className="mx-auto"
      style={{
        padding: "24px 28px 32px",
        maxWidth: 840,
        width: "100%",
      }}
    >
      {/* Protection — backed by the beta protection sub-resource: the
          inbound/outbound trust gates + content scan that decide what gets
          held, and the review queue that governs held messages. Gated on
          domain verification (the approve/reject pipeline needs a real
          domain) and on the protection config having loaded. */}
      {agent.domain_verified && protection && (
        <ProtectionEditor
          email={agent.email}
          config={protection}
          onSaved={onEditorSaved}
        />
      )}

      {/* Danger zone */}
      <section
        data-testid="danger-zone"
        className="mt-8"
        style={{
          background: "var(--bg-panel)",
          border: "1px solid var(--danger-bg)",
          borderRadius: "var(--r-lg)",
          padding: "20px 22px",
        }}
      >
        <Eyebrow>Danger zone</Eyebrow>
        <h2
          className="mt-2 mb-1.5"
          style={{
            fontFamily: "var(--f-ui)",
            fontSize: 15,
            fontWeight: 600,
            color: "var(--fg)",
          }}
        >
          Delete this inbox
        </h2>
        <p
          className="mb-4"
          style={{ fontSize: 13, color: "var(--fg-muted)", lineHeight: 1.6 }}
        >
          Moves the inbox to the trash: it stops receiving mail immediately,
          its held drafts leave the review queue, and its API credentials
          stop working. You can restore it — messages and settings intact —
          from the Trash page for 30 days; after that it is purged
          permanently together with its messages. The email address stays
          reserved while the inbox is in the trash.
        </p>
        <button
          type="button"
          onClick={onDelete}
          disabled={deleting}
          style={{
            fontFamily: "var(--f-ui)",
            fontSize: 13,
            fontWeight: 500,
            padding: "8px 14px",
            background: "var(--bg-panel)",
            border: "1px solid var(--danger-strong)",
            borderRadius: "var(--r-md)",
            color: "var(--danger-strong)",
            cursor: deleting ? "default" : "pointer",
          }}
        >
          {deleting ? "Deleting…" : "Delete inbox"}
        </button>
        {deleteError && (
          <p
            className="mt-3 text-[12px]"
            style={{ color: "var(--danger-strong)" }}
          >
            {deleteError}
          </p>
        )}
      </section>
    </div>
  );
}
