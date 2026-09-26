"use client";

// One-time notice after an account comes back from the trash. GET /v1/account
// carries restored_at once the account was restored; the notice explains what
// the restore did NOT bring back (API keys, domain verification) until the
// user dismisses it. Dismissal is remembered per restored_at value, so a
// later trash-and-restore shows it again.
//
// Reads through `limitsKey` — the same SWR cache entry the Billing page and
// useSendingAccess use — so mounting it in the app shell shares one
// GET /v1/account with them instead of adding a request.

import Link from "next/link";
import { useState } from "react";
import useSWR from "swr";
import { getAccountInfo } from "./onboarding/api";
import { limitsKey } from "../../lib/swrKeys";

export const RESTORED_NOTICE_STORAGE_KEY = "e2a:account-restored-notice-dismissed";

// restored_at stays on the account forever. Past this age the notice is
// stale news (and would greet the same user again on every new browser), so
// it only shows for a restore this recent.
export const RESTORED_NOTICE_MAX_AGE_MS = 30 * 24 * 60 * 60 * 1000;

function readDismissed(): string | null {
  try {
    return window.localStorage.getItem(RESTORED_NOTICE_STORAGE_KEY);
  } catch {
    return null;
  }
}

function writeDismissed(value: string): void {
  try {
    window.localStorage.setItem(RESTORED_NOTICE_STORAGE_KEY, value);
  } catch {
    // Storage unavailable (private mode, quota, disabled): the in-memory
    // state below still hides the notice for this page session.
  }
}

function isRecent(iso: string): boolean {
  const t = new Date(iso).getTime();
  if (Number.isNaN(t)) return false;
  return Date.now() - t < RESTORED_NOTICE_MAX_AGE_MS;
}

export function RestoredNotice() {
  const { data } = useSWR(limitsKey, getAccountInfo);
  const [dismissedFor, setDismissedFor] = useState<string | null>(readDismissed);

  const restoredAt = data?.restored_at;
  if (!restoredAt || !isRecent(restoredAt) || dismissedFor === restoredAt) return null;

  const dismiss = () => {
    writeDismissed(restoredAt);
    setDismissedFor(restoredAt);
  };

  return (
    <div className="px-6 md:px-8 lg:px-12 pt-6 mx-auto w-full" style={{ maxWidth: 1080 }}>
      <div
        role="status"
        data-testid="account-restored-notice"
        className="flex items-start gap-3 p-4"
        style={{
          background: "var(--success-bg)",
          borderRadius: "var(--r-md)",
        }}
      >
        <div className="flex-1 min-w-0">
          <p className="text-[13px] font-semibold" style={{ color: "var(--success-strong)" }}>
            Your account was restored.
          </p>
          <p className="text-[13px] mt-1 leading-[1.55]" style={{ color: "var(--fg)" }}>
            API keys were revoked, so create new ones. Custom domains must be re-verified before
            they send.
          </p>
          <div className="flex items-center gap-x-4 gap-y-1 mt-2 flex-wrap">
            <Link
              href="/api-keys"
              className="inline-flex items-center min-h-[44px] md:min-h-[28px] text-[12px] font-medium underline"
              style={{ color: "var(--success-strong)" }}
            >
              Create an API key
            </Link>
            <Link
              href="/domains"
              className="inline-flex items-center min-h-[44px] md:min-h-[28px] text-[12px] font-medium underline"
              style={{ color: "var(--success-strong)" }}
            >
              Re-verify domains
            </Link>
          </div>
        </div>
        <button
          type="button"
          onClick={dismiss}
          aria-label="Dismiss restored account notice"
          className="shrink-0 inline-flex items-center justify-center"
          style={{
            width: 44,
            height: 44,
            margin: "-10px -10px 0 0",
            borderRadius: "var(--r-md)",
            color: "var(--success-strong)",
            background: "transparent",
          }}
        >
          <svg
            width="16"
            height="16"
            viewBox="0 0 24 24"
            fill="none"
            stroke="currentColor"
            strokeWidth="2"
            strokeLinecap="round"
            strokeLinejoin="round"
            aria-hidden="true"
          >
            <line x1="18" y1="6" x2="6" y2="18" />
            <line x1="6" y1="6" x2="18" y2="18" />
          </svg>
        </button>
      </div>
    </div>
  );
}
