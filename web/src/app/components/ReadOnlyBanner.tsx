"use client";

// Persistent banner for a read-only account (sending paused for abuse
// review): mounted in the app shell, so it shows on every dashboard page and
// cannot be dismissed while the state lasts. The copy names the state and the
// way out, never the operator's reason.

import Link from "next/link";
import { useAccountReadOnly } from "./hooks/useAccountReadOnly";
import { ACCOUNT_READ_ONLY_MESSAGE } from "../../lib/readOnly";

export function ReadOnlyBanner() {
  const readOnly = useAccountReadOnly();
  if (!readOnly) return null;
  return (
    <div className="px-6 md:px-8 lg:px-12 pt-6 mx-auto w-full" style={{ maxWidth: 1080 }}>
      <div
        role="alert"
        data-testid="account-read-only-banner"
        className="flex items-start gap-3 p-4"
        style={{
          background: "var(--danger-bg)",
          borderRadius: "var(--r-md)",
        }}
      >
        <div className="flex-1 min-w-0">
          <p className="text-[13px] font-semibold" style={{ color: "var(--danger-strong)" }}>
            {ACCOUNT_READ_ONLY_MESSAGE}
          </p>
          <p className="text-[13px] mt-1 leading-[1.55]" style={{ color: "var(--fg)" }}>
            You can still read your inboxes and export your data. Changes — sending, creating or
            editing inboxes, domains, keys and webhooks — are unavailable until the review is
            complete.
          </p>
          <Link
            href="/feedback"
            className="inline-flex items-center min-h-[44px] md:min-h-[28px] mt-2 text-[12px] font-medium underline"
            style={{ color: "var(--danger-strong)" }}
          >
            Contact support
          </Link>
        </div>
      </div>
    </div>
  );
}
