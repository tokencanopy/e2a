"use client";

import { useState } from "react";
import { FEEDBACK_EMAIL } from "../../../lib/site";
import { PageShell } from "../../components/loft/PageShell";
import { useAuth } from "../../components/AuthProvider";

// Restyled to match the rest of the app — PageShell wrapping, accent
// selected category, canopy submit CTA, the Loft input + button rhythm
// shared with /api-keys and /settings.
//
// Test selectors (text/placeholders) are stable so page.test.tsx keeps
// passing across visual tweaks.

export default function FeedbackPage() {
  const { user } = useAuth();
  const [category, setCategory] = useState<"bug" | "feature" | "general">("general");
  const [message, setMessage] = useState("");
  const [status, setStatus] = useState<"idle" | "sending" | "sent" | "error" | "rate-limited">("idle");

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!message.trim()) return;

    setStatus("sending");
    try {
      const res = await fetch("/api/feedback", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        credentials: "include",
        body: JSON.stringify({ email: user?.email || undefined, category, message }),
      });
      if (res.ok) {
        setStatus("sent");
        setMessage("");
      } else if (res.status === 429) {
        setStatus("rate-limited");
      } else {
        setStatus("error");
      }
    } catch {
      setStatus("error");
    }
  };

  if (status === "sent") {
    return (
      <PageShell
        eyebrow="Workspace"
        title={<>Send us feedback</>}
        maxWidth={640}
      >
        <div
          className="p-8 text-center"
          style={{
            background: "var(--bg-panel)",
            border: "1px solid var(--border)",
            borderRadius: "var(--r-lg)",
          }}
        >
          <div
            className="mx-auto mb-5 flex items-center justify-center text-[20px]"
            style={{
              width: 48,
              height: 48,
              borderRadius: "50%",
              background: "var(--success-bg)",
              color: "var(--success)",
            }}
            aria-hidden
          >
            ✓
          </div>
          <h2
            className="mb-2"
            style={{
              fontFamily: "var(--f-ui)",
              fontSize: 20,
              fontWeight: 600,
              color: "var(--fg)",
            }}
          >
            Thanks for your feedback
          </h2>
          <p
            className="text-[13px] mb-5"
            style={{ color: "var(--fg-muted)" }}
          >
            We read every submission and it directly shapes what we build next.
          </p>
          <button
            onClick={() => setStatus("idle")}
            className="text-[13px] underline"
            style={{ color: "var(--accent-strong)" }}
          >
            Submit more feedback
          </button>
        </div>
      </PageShell>
    );
  }

  return (
    <PageShell
      eyebrow="Workspace"
      title={<>Send us feedback</>}
      subtitle={
        <>
          Bug reports, feature requests, or anything else.
          {FEEDBACK_EMAIL && (
            <>
              {" "}You can also reach us at{" "}
              <a
                href={`mailto:${FEEDBACK_EMAIL}`}
                style={{ color: "var(--accent-strong)" }}
                className="underline"
              >
                {FEEDBACK_EMAIL}
              </a>
              .
            </>
          )}
        </>
      }
      maxWidth={640}
    >
      <form onSubmit={handleSubmit} className="space-y-5">
        <div>
          <label
            className="block text-[13px] font-medium mb-1.5"
            style={{ color: "var(--fg)" }}
          >
            Category
          </label>
          <div className="flex gap-2 flex-wrap">
            {([
              { value: "bug", label: "Bug report" },
              { value: "feature", label: "Feature request" },
              { value: "general", label: "General" },
            ] as const).map((opt) => {
              const active = category === opt.value;
              return (
                <button
                  key={opt.value}
                  type="button"
                  onClick={() => setCategory(opt.value)}
                  aria-pressed={active}
                  className="text-[12px] font-medium px-3 py-1.5 transition"
                  style={{
                    background: active ? "var(--accent-soft)" : "var(--bg-panel)",
                    color: active ? "var(--accent-strong)" : "var(--fg-muted)",
                    border: active
                      ? "1px solid var(--accent-strong)"
                      : "1px solid var(--border)",
                    borderRadius: "var(--r-md)",
                  }}
                >
                  {opt.label}
                </button>
              );
            })}
          </div>
        </div>

        <div>
          <label
            htmlFor="feedback-message"
            className="block text-[13px] font-medium mb-1.5"
            style={{ color: "var(--fg)" }}
          >
            Message
          </label>
          <textarea
            id="feedback-message"
            value={message}
            onChange={(e) => setMessage(e.target.value)}
            placeholder="What's on your mind?"
            rows={6}
            required
            className="w-full text-[13px] px-3 py-2 resize-none"
            style={{
              background: "var(--bg-panel)",
              border: "1px solid var(--border)",
              borderRadius: "var(--r-md)",
              color: "var(--fg)",
            }}
          />
        </div>

        <button
          type="submit"
          disabled={status === "sending" || !message.trim()}
          className="w-full text-[13px] font-medium px-4 py-2.5 transition disabled:opacity-50 disabled:cursor-not-allowed"
          style={{
            background: "var(--accent-fill)",
            color: "var(--accent-fg)",
            borderRadius: "var(--r-md)",
          }}
        >
          {status === "sending" ? "Sending..." : "Submit feedback"}
        </button>

        {status === "error" && (
          <p
            className="text-[12px] text-center"
            style={{ color: "var(--danger-strong)" }}
          >
            Something went wrong. Please try again or email us directly.
          </p>
        )}
        {status === "rate-limited" && (
          <p
            className="text-[12px] text-center"
            style={{ color: "var(--warn-strong)" }}
          >
            Too many submissions. Please wait a minute before trying again.
          </p>
        )}
      </form>
    </PageShell>
  );
}
