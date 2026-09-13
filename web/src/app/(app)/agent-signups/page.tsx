"use client";

import { useEffect, useState } from "react";
import { PageShell } from "../../components/loft/PageShell";
import {
  approveAgentSignup,
  listPendingAgentSignups,
  rejectAgentSignup,
  type AgentSignupView,
} from "../../components/onboarding/api";

export default function AgentSignupsPage() {
  const [signups, setSignups] = useState<AgentSignupView[]>([]);
  const [review, setReview] = useState<Record<string, boolean>>({});
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState<string | null>(null);
  const [error, setError] = useState("");

  useEffect(() => {
    let active = true;
    listPendingAgentSignups()
      .then((items) => {
        if (active) setSignups(items);
      })
      .catch((err: unknown) => {
        if (active) setError(err instanceof Error ? err.message : "Could not load agent requests.");
      })
      .finally(() => {
        if (active) setLoading(false);
      });
    return () => {
      active = false;
    };
  }, []);

  const decide = async (signup: AgentSignupView, decision: "approve" | "reject") => {
    setBusy(signup.id);
    setError("");
    try {
      if (decision === "approve") {
        await approveAgentSignup(signup.id, review[signup.id] ?? false);
      } else {
        await rejectAgentSignup(signup.id);
      }
      setSignups((items) => items.filter((item) => item.id !== signup.id));
    } catch (err) {
      setError(err instanceof Error ? err.message : `Could not ${decision} this request.`);
    } finally {
      setBusy(null);
    }
  };

  return (
    <PageShell
      eyebrow="Workspace"
      title={<>Agent requests</>}
      subtitle="Approve agents that named your email during self-signup, or reject them to revoke the provisional key and deactivate the inbox."
      maxWidth={860}
    >
      {error && (
        <div
          role="alert"
          className="mb-5 p-3 text-[13px]"
          style={{
            color: "var(--danger-strong)",
            background: "var(--danger-bg)",
            borderRadius: "var(--r-md)",
          }}
        >
          {error}
        </div>
      )}

      {loading ? (
        <p className="text-[13px]" style={{ color: "var(--fg-muted)" }}>
          Loading agent requests…
        </p>
      ) : signups.length === 0 ? (
        <div
          className="p-8 text-center"
          style={{ background: "var(--bg-panel)", border: "1px solid var(--border)", borderRadius: "var(--r-lg)" }}
        >
          <p className="text-[14px] font-medium" style={{ color: "var(--fg)" }}>
            No agent requests waiting
          </p>
          <p className="mt-1 text-[13px]" style={{ color: "var(--fg-muted)" }}>
            New requests that name your signed-in email will appear here.
          </p>
        </div>
      ) : (
        <div className="space-y-3">
          {signups.map((signup) => {
            const disabled = busy === signup.id;
            return (
              <article
                key={signup.id}
                className="p-5"
                style={{ background: "var(--bg-panel)", border: "1px solid var(--border)", borderRadius: "var(--r-lg)" }}
              >
                <div className="flex flex-col sm:flex-row sm:items-start sm:justify-between gap-4">
                  <div className="min-w-0">
                    <h2 className="text-[15px] font-semibold" style={{ color: "var(--fg)" }}>
                      {signup.display_name}
                    </h2>
                    <p className="font-mono text-[12px] mt-1 break-all" style={{ color: "var(--fg-muted)" }}>
                      {signup.inbox}
                    </p>
                    <p className="text-[12px] mt-2" style={{ color: "var(--fg-subtle)" }}>
                      Requested {new Date(signup.created_at).toLocaleString()}
                      {signup.harness ? ` · ${signup.harness}` : ""}
                    </p>
                  </div>
                  <div className="flex gap-2 sm:shrink-0">
                    <button
                      type="button"
                      disabled={disabled}
                      onClick={() => void decide(signup, "reject")}
                      className="px-3.5 py-2 text-[13px] font-medium disabled:opacity-50"
                      style={{ color: "var(--danger-strong)", border: "1px solid var(--border)", borderRadius: "var(--r-md)" }}
                    >
                      Reject
                    </button>
                    <button
                      type="button"
                      disabled={disabled}
                      onClick={() => void decide(signup, "approve")}
                      className="px-3.5 py-2 text-[13px] font-medium disabled:opacity-50"
                      style={{ color: "var(--accent-fg)", background: "var(--accent-fill)", borderRadius: "var(--r-md)" }}
                    >
                      Approve
                    </button>
                  </div>
                </div>

                <label className="flex items-start gap-2.5 mt-4 text-[13px] cursor-pointer" style={{ color: "var(--fg-muted)" }}>
                  <input
                    type="checkbox"
                    checked={review[signup.id] ?? false}
                    onChange={(event) =>
                      setReview((current) => ({ ...current, [signup.id]: event.target.checked }))
                    }
                    className="mt-0.5"
                  />
                  <span>
                    <span className="font-medium" style={{ color: "var(--fg)" }}>
                      Review outbound
                    </span>{" "}
                    — messages to anyone other than you enter the existing Pending review queue.
                  </span>
                </label>
              </article>
            );
          })}
        </div>
      )}
    </PageShell>
  );
}
