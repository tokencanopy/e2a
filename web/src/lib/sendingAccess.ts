// Pure helpers for the external sending access restriction (beta): eligibility
// checks, disclosure copy, the composer's client-side recipient preflight, and
// 403 error-envelope parsing. Shared by the dashboard notice, onboarding
// success panel, the review-queue composer, and the /sending-access page.
//
// No React, no fetch — mirrors the lib/messageLifecycle.ts convention: this
// module owns the logic, callers own the fetch + render.

/** Mirrors SendingAccessView (GET /v1/account → sending_access, beta).
 *  Describes what the account may do, never a promise that a given send
 *  also passes pause/quota/content/domain checks. The field is entirely
 *  omitted by the server when unavailable; callers must treat `undefined`
 *  as "no restriction info" (render nothing), never as "restricted". */
export type SendingAccessStatus = {
  enforcement_applies: boolean;
  shared_external_approved: boolean;
  paid_external_sending_entitled: boolean;
  owner_recipient_verified: boolean;
  /** The routes this deployment accepts for lifting the restriction. Open
   *  set; absent only from servers that predate the field, which accept all
   *  three. Hosted e2a lists only "operator_approval". */
  available_unlocks?: string[];
};

export type SendingAccessUnlock = "operator_approval" | "verified_domain" | "paid_entitlement";

const ALL_UNLOCKS: SendingAccessUnlock[] = ["operator_approval", "verified_domain", "paid_entitlement"];

/** True when the deployment accepts `unlock`. A missing list (older server)
 *  means every unlock, exactly as the server behaved before the field. */
export function unlockAvailable(
  status: SendingAccessStatus | null | undefined,
  unlock: SendingAccessUnlock,
): boolean {
  const list = status?.available_unlocks ?? ALL_UNLOCKS;
  return list.includes(unlock);
}

/** True when the paid entitlement actually lifts the restriction on this
 *  deployment — holding it is only a signal where paid_entitlement is not
 *  an available unlock. */
function paidUnlocks(status: SendingAccessStatus): boolean {
  return status.paid_external_sending_entitled && unlockAvailable(status, "paid_entitlement");
}

/** True only when the deployment enforces the control for this account AND
 *  no account-level grant lifts it (operator approval, or a paid base plan
 *  where the deployment accepts one). A missing/undefined status —
 *  disabled, shadow mode, or an account outside the rollout cohort — is
 *  never "restricted". */
export function isSendingRestricted(
  status: SendingAccessStatus | null | undefined,
): boolean {
  return Boolean(
    status?.enforcement_applies &&
      !status.shared_external_approved &&
      !paidUnlocks(status),
  );
}

/** Short neutral line for an account where enforcement applies but a grant
 *  already lifts the restriction — shown in place of the full disclosure
 *  banner wherever access status is surfaced (paid takes precedence, since
 *  an account can hold both grants at once). Null when there's nothing to
 *  say: enforcement doesn't apply, or the account is still restricted (see
 *  `sendingAccessNoticeCopy` for that case instead). */
export function sendingAccessEligibilityLabel(
  status: SendingAccessStatus | null | undefined,
): string | null {
  if (!status?.enforcement_applies) return null;
  if (paidUnlocks(status)) {
    return "Paid plan: external sending enabled";
  }
  if (status.shared_external_approved) {
    return "Operator-approved external sending";
  }
  return null;
}

/** The body for the single "External sending is enabled" card: which route
 *  lifted the restriction. Null while still restricted. */
export function sendingAccessEnabledRoute(
  status: SendingAccessStatus | null | undefined,
): string | null {
  if (!status?.enforcement_applies || isSendingRestricted(status)) return null;
  if (status.shared_external_approved) {
    return "An operator approved external sending for this account.";
  }
  return "Your paid base plan includes external sending for this account.";
}

/** Which recovery routes to offer a restricted account: approval always;
 *  a verified domain only where the deployment accepts it; a paid plan only
 *  where the deployment accepts it AND billing is enabled (hosted-only UI). */
export function offeredUnlocks(
  status: SendingAccessStatus,
  opts: { billingEnabled: boolean },
): { domain: boolean; approval: true; paid: boolean } {
  return {
    domain: unlockAvailable(status, "verified_domain"),
    approval: true,
    paid: opts.billingEnabled && unlockAvailable(status, "paid_entitlement"),
  };
}

export type SendingAccessNoticeCopy = { headline: string; body: string };

/** Disclosure copy for the restriction banner (dashboard + onboarding +
 *  /sending-access). Callers must already have confirmed
 *  `isSendingRestricted(status)` — this always returns the "restricted"
 *  copy. Pass `formBelow` only when the request form renders beneath it. The headline says nothing about inboxes (an account may have none
 *  yet); the recovery sentence lists only the routes this deployment
 *  honors. */
export function sendingAccessNoticeCopy(
  status: SendingAccessStatus,
  opts: { billingEnabled: boolean; formBelow?: boolean },
): SendingAccessNoticeCopy {
  const headline = "External sending is restricted for this account.";
  const offered = offeredUnlocks(status, opts);
  const routes: string[] = [];
  if (offered.domain) routes.push("verify your own domain");
  routes.push("request approval");
  if (offered.paid) routes.push("activate a paid base plan");
  let recovery: string;
  if (routes.length === 1) {
    // "below" only where the request form is actually rendered under the
    // copy (the /sending-access page with no pending request); the inbox
    // notice links to it instead.
    recovery = opts.formBelow
      ? "To email other recipients, request approval below."
      : "To email other recipients, request approval.";
  } else if (routes.length === 2) {
    recovery = `To email other recipients, ${routes[0]} or ${routes[1]}.`;
  } else {
    recovery = `To email other recipients, ${routes.slice(0, -1).join(", ")}, or ${routes[routes.length - 1]}.`;
  }
  const reach = status.owner_recipient_verified
    ? "Receive emails from anyone. Send to your verified account email or agent inboxes in this account."
    : "Receive emails from anyone. Agent inboxes in this account are available for testing.";
  return { headline, body: `${reach} ${recovery}` };
}

// ── Composer preflight ──────────────────────────────────────────────────

/** Pull the bare address out of an "Name <addr@x>" token or a plain
 *  address; a blank/whitespace-only token resolves to "". */
function extractAddress(raw: string): string {
  const trimmed = raw.trim();
  if (!trimmed) return "";
  const angle = trimmed.match(/<([^>]+)>/);
  return (angle ? angle[1] : trimmed).trim();
}

/** Split a comma-separated recipient field (the composer's To/Cc/Bcc text
 *  inputs) into individual address tokens, dropping empty entries. */
export function parseRecipientList(csv: string): string[] {
  return csv
    .split(",")
    .map((s) => s.trim())
    .filter(Boolean);
}

/** Client-side guidance only — the server is the final authority (a
 *  verified sending domain, a same-account agent not yet reflected here, or
 *  a policy change can all move an address in or out of "allowed"). Returns
 *  the deduplicated, order-preserving subset of `recipients` that are
 *  neither a same-account agent inbox nor — when the sign-in email is
 *  verified — the account owner's own address. */
export function disallowedRecipients(
  recipients: string[],
  opts: {
    accountAgentEmails: string[];
    ownerEmail?: string | null;
    ownerVerified: boolean;
  },
): string[] {
  const allowed = new Set(
    opts.accountAgentEmails
      .map((e) => e.trim().toLowerCase())
      .filter(Boolean),
  );
  if (opts.ownerVerified && opts.ownerEmail) {
    allowed.add(opts.ownerEmail.trim().toLowerCase());
  }
  const seen = new Set<string>();
  const out: string[] = [];
  for (const raw of recipients) {
    const address = extractAddress(raw);
    if (!address) continue;
    const key = address.toLowerCase();
    if (allowed.has(key) || seen.has(key)) continue;
    seen.add(key);
    out.push(address);
  }
  return out;
}

// ── Error envelope parsing ──────────────────────────────────────────────

export type ErrorEnvelopeInfo = { code: string; message: string };

/** Best-effort parse of a /v1 ErrorEnvelope body. api.ts's `request()`
 *  throws with the raw response text as the thrown Error's message; this
 *  recovers the structured {code, message} when the body was JSON, falling
 *  back to the raw text otherwise (matching the read-only surfaces'
 *  `readErrorBody` helper). */
export function parseErrorEnvelope(rawMessage: string): ErrorEnvelopeInfo {
  try {
    const body = JSON.parse(rawMessage) as {
      error?: { code?: string; message?: string };
    };
    return {
      code: body.error?.code ?? "",
      message: body.error?.message || rawMessage,
    };
  } catch {
    return { code: "", message: rawMessage };
  }
}

export type ExternalSendingNotEnabledInfo = {
  message: string;
  allowedRecipients: string[];
  recoveryUrl?: string;
};

/** Recognizes the external_sending_not_enabled 403 in a failed send's error
 *  text, surfacing its ExternalSendingNotEnabledDetails (allowed_recipients,
 *  recovery_url). Null for any other error — including a non-JSON body or a
 *  different code — so callers fall back to rendering the raw message. */
export function parseExternalSendingNotEnabledError(
  rawMessage: string,
): ExternalSendingNotEnabledInfo | null {
  let parsed: {
    error?: {
      code?: string;
      message?: string;
      details?: { allowed_recipients?: unknown; recovery_url?: unknown };
    };
  };
  try {
    parsed = JSON.parse(rawMessage);
  } catch {
    return null;
  }
  if (parsed.error?.code !== "external_sending_not_enabled") return null;
  const details = parsed.error.details ?? {};
  const allowedRecipients = Array.isArray(details.allowed_recipients)
    ? details.allowed_recipients.filter(
        (v): v is string => typeof v === "string",
      )
    : [];
  const recoveryUrl =
    typeof details.recovery_url === "string" ? details.recovery_url : undefined;
  return {
    message:
      parsed.error.message ||
      "External sending is not enabled for this account.",
    allowedRecipients,
    recoveryUrl,
  };
}
