"use client";

import { useState } from "react";
import { useRouter } from "next/navigation";
import { useAuth } from "../../components/AuthProvider";
import type { UpdateMeRequest, UserInfo } from "../../components/types";
import {
  ACQUISITION_DETAIL_MAX,
  ACQUISITION_SOURCES,
  type AcquisitionSource,
} from "../../../lib/acquisitionSources";

// One-question onboarding survey. The app shell routes a user here while
// the server reports onboarding_survey_pending and renders this page
// without the sidebar; answering (or skipping) flips the flag through
// PATCH /api/auth/me and the shell lets the user through.
//
// Test selectors (heading text, option labels, button names, placeholder)
// are stable — page.test.tsx depends on them.

type SurveyBody = NonNullable<UpdateMeRequest["onboarding_survey"]>;

export default function WelcomePage() {
  const router = useRouter();
  const { user, setUser } = useAuth();
  const [source, setSource] = useState<AcquisitionSource | null>(null);
  const [detail, setDetail] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const leave = (updated: UserInfo | null) => {
    if (updated) {
      setUser(updated);
    } else if (user) {
      setUser({ ...user, onboarding_survey_pending: false });
    }
    router.replace("/inboxes");
  };

  const send = async (body: SurveyBody): Promise<"ok" | "done" | "failed"> => {
    const res = await fetch("/api/auth/me", {
      method: "PATCH",
      headers: { "Content-Type": "application/json" },
      credentials: "include",
      body: JSON.stringify({ onboarding_survey: body }),
    });
    if (res.ok) {
      leave((await res.json()) as UserInfo);
      return "ok";
    }
    if (res.status === 409) {
      // Answered elsewhere (another tab). Nothing to redo.
      leave(null);
      return "done";
    }
    return "failed";
  };

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!source || busy) return;
    setBusy(true);
    setError("");
    const trimmed = detail.trim();
    const body: SurveyBody =
      source === "other" && trimmed ? { source, detail: trimmed } : { source };
    try {
      if ((await send(body)) === "failed") {
        setError("Something went wrong. You can try again or skip for now.");
        setBusy(false);
      }
    } catch {
      setError("Something went wrong. You can try again or skip for now.");
      setBusy(false);
    }
  };

  const handleSkip = async () => {
    if (busy) return;
    setBusy(true);
    // Skip must never trap the user: whatever the server says (or if it
    // cannot be reached), leave. An unrecorded skip is asked again next
    // login, which is the right failure mode.
    try {
      if ((await send({ source: "skipped" })) === "failed") leave(null);
    } catch {
      leave(null);
    }
  };

  return (
    <main className="min-h-screen flex items-center justify-center px-6 py-12">
      <form
        onSubmit={handleSubmit}
        className="w-full"
        style={{ maxWidth: 560 }}
        aria-busy={busy}
      >
        <p
          className="text-[11px] font-medium uppercase tracking-wider mb-3"
          style={{ color: "var(--fg-muted)" }}
        >
          Welcome to e2a
        </p>
        <h1
          className="mb-2"
          style={{
            fontFamily: "var(--f-ui)",
            fontSize: 24,
            fontWeight: 600,
            color: "var(--fg)",
            letterSpacing: "-0.01em",
          }}
        >
          Where did you hear about e2a?
        </h1>
        <p className="text-[13px] mb-6" style={{ color: "var(--fg-muted)" }}>
          One question, then you&apos;re in. It helps us know where to show up.
        </p>

        <fieldset className="space-y-2 mb-5" disabled={busy}>
          <legend className="sr-only">Where did you hear about e2a?</legend>
          {ACQUISITION_SOURCES.map((opt) => {
            const active = source === opt.value;
            return (
              <label
                key={opt.value}
                className="flex items-center gap-3 px-4 py-3 cursor-pointer text-[13px] transition"
                style={{
                  background: active ? "var(--accent-soft)" : "var(--bg-panel)",
                  color: active ? "var(--accent-strong)" : "var(--fg)",
                  border: active ? "1px solid var(--accent-strong)" : "1px solid var(--border)",
                  borderRadius: "var(--r-md)",
                }}
              >
                <input
                  type="radio"
                  name="acquisition_source"
                  value={opt.value}
                  checked={active}
                  onChange={() => setSource(opt.value)}
                  className="accent-current"
                />
                <span className="font-medium">{opt.label}</span>
              </label>
            );
          })}
        </fieldset>

        {source === "other" && (
          <div className="mb-5">
            <input
              type="text"
              value={detail}
              onChange={(e) => setDetail(e.target.value)}
              maxLength={ACQUISITION_DETAIL_MAX}
              placeholder="Tell us more (optional)"
              aria-label="Tell us more (optional)"
              disabled={busy}
              className="w-full px-3 py-2 text-[13px]"
              style={{
                background: "var(--bg-panel)",
                color: "var(--fg)",
                border: "1px solid var(--border)",
                borderRadius: "var(--r-md)",
              }}
            />
          </div>
        )}

        {error && (
          <p role="alert" className="text-[13px] mb-4" style={{ color: "var(--danger)" }}>
            {error}
          </p>
        )}

        <div className="flex items-center justify-between gap-4">
          <button
            type="button"
            onClick={handleSkip}
            disabled={busy}
            className="text-[13px] underline"
            style={{ color: "var(--fg-muted)" }}
          >
            Skip
          </button>
          <button
            type="submit"
            disabled={!source || busy}
            className="px-4 py-2 text-[13px] font-medium transition disabled:opacity-50"
            style={{
              background: "var(--accent-fill)",
              color: "var(--accent-fg)",
              borderRadius: "var(--r-md)",
            }}
          >
            Continue
          </button>
        </div>
      </form>
    </main>
  );
}
