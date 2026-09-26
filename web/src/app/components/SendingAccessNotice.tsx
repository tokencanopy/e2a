"use client";

// Shared disclosure notice for the external sending access restriction
// (beta). Purely presentational — takes the already-fetched status and
// renders one of three things:
//
//   1. nothing, when the restriction doesn't apply to this account
//      (disabled deployment, shadow mode, out of cohort — `status` is
//      undefined or `enforcement_applies` is false);
//   2. a small neutral eligibility line, when enforcement applies but a
//      grant already lifts it (operator-approved or a paid base plan);
//   3. the full restriction banner with recovery actions, otherwise.
//
// Reused by the dashboard home page and the onboarding success panel so
// the copy can't drift between the two placements.

import Link from "next/link";
import {
  isSendingRestricted,
  sendingAccessEligibilityLabel,
  sendingAccessNoticeCopy,
  type SendingAccessStatus,
} from "../../lib/sendingAccess";

// Hosted-only billing gate (AGENTS.md: billing UI stays inert on self-host).
// A self-host build never has this set, so "Choose a paid plan" and the
// paid-plan clause in the body copy only ever appear on the hosted service.
const BILLING_API = (process.env.NEXT_PUBLIC_BILLING_API ?? "").replace(/\/$/, "");

export function SendingAccessNotice({
  status,
}: {
  status: SendingAccessStatus | null | undefined;
}) {
  if (!status || !status.enforcement_applies) return null;

  if (!isSendingRestricted(status)) {
    const label = sendingAccessEligibilityLabel(status);
    if (!label) return null;
    return (
      <p
        data-testid="sending-access-eligible"
        className="text-[12px]"
        style={{ color: "var(--fg-muted)" }}
      >
        {label}
      </p>
    );
  }

  const { headline, body } = sendingAccessNoticeCopy(status, {
    billingEnabled: Boolean(BILLING_API),
  });

  return (
    <div
      data-testid="sending-access-restricted-notice"
      role="status"
      className="mb-6 p-4"
      style={{
        background: "var(--warn-bg)",
        border: "1px solid var(--warn-bg)",
        borderRadius: "var(--r-md)",
      }}
    >
      <p className="text-[13px] font-semibold" style={{ color: "var(--warn-strong)" }}>
        {headline}
      </p>
      <p className="text-[13px] mt-1" style={{ color: "var(--fg-muted)" }}>
        {body}
      </p>
      <div className="flex items-center gap-4 mt-3 flex-wrap">
        <Link
          href="/domains"
          className="text-[12px] font-medium underline"
          style={{ color: "var(--warn-strong)" }}
        >
          Verify a domain
        </Link>
        <Link
          href="/sending-access"
          className="text-[12px] font-medium underline"
          style={{ color: "var(--warn-strong)" }}
        >
          Request approval
        </Link>
        {BILLING_API && (
          <Link
            href="/billing"
            className="text-[12px] font-medium underline"
            style={{ color: "var(--warn-strong)" }}
          >
            Choose a paid plan
          </Link>
        )}
      </div>
    </div>
  );
}
