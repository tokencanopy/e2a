// Pure helpers for the external sending access restriction (beta): eligibility
// checks, disclosure copy, the composer's client-side recipient preflight, and
// 403 error-envelope parsing. Shared by the dashboard notice, onboarding
// success panel, the review-queue composer, and the /sending-access page.
//
// No React, no fetch — mirrors the lib/messageLifecycle.ts convention: this
// module owns the logic, callers own the fetch + render.

/** Mirrors SendingAccessView (GET /v1/account → sending_access, beta).
 *  Booleans only — describes what the account may do, never a promise that
 *  a given send also passes pause/quota/content/domain checks. The field is
 *  entirely omitted by the server when unavailable; callers must treat
 *  `undefined` as "no restriction info" (render nothing), never as
 *  "restricted". */
export type SendingAccessStatus = {
  enforcement_applies: boolean;
  shared_external_approved: boolean;
  paid_external_sending_entitled: boolean;
  owner_recipient_verified: boolean;
};

/** True only when the deployment enforces the control for this account AND
 *  neither grant (operator approval or a paid base plan) already lifts it.
 *  A missing/undefined status — disabled, shadow mode, or an account
 *  outside the rollout cohort — is never "restricted". */
export function isSendingRestricted(
  status: SendingAccessStatus | null | undefined,
): boolean {
  return Boolean(
    status?.enforcement_applies &&
      !status.shared_external_approved &&
      !status.paid_external_sending_entitled,
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
  if (status.paid_external_sending_entitled) {
    return "Paid plan: external sending enabled";
  }
  if (status.shared_external_approved) {
    return "Operator-approved external sending";
  }
  return null;
}

export type SendingAccessNoticeCopy = { headline: string; body: string };

/** Disclosure copy for the restriction banner (dashboard + onboarding).
 *  Callers must already have confirmed `isSendingRestricted(status)` —
 *  this always returns the "restricted" copy, never the eligible one. */
export function sendingAccessNoticeCopy(
  status: SendingAccessStatus,
  opts: { billingEnabled: boolean },
): SendingAccessNoticeCopy {
  const headline = "Your inbox is ready. External sending is restricted.";
  const planClause = opts.billingEnabled ? ", activate a paid base plan" : "";
  const body = status.owner_recipient_verified
    ? `Receive emails from anyone. Send to your verified account email or agent inboxes in this account. To email other recipients, send from your own verified domain or request approval${planClause}.`
    : `Receive emails from anyone. Agent inboxes in this account are available for testing. To email other recipients, send from your own verified domain or request approval${planClause}.`;
  return { headline, body };
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
