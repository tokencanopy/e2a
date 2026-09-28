// Read-only accounts (docs/design/account-read-only.md). An account whose
// sending is paused for an abuse review is read-only: the server refuses
// every write with 403 `account_read_only` (the API error is the backstop),
// GET /v1/account reports `read_only: true`, and the dashboard shows a
// persistent banner and disables the cheap-to-disable write controls.

export const ACCOUNT_READ_ONLY_CODE = "account_read_only";

/** Customer copy for the banner and for any refused write. */
export const ACCOUNT_READ_ONLY_MESSAGE =
  "Your account is read-only while sending is paused for abuse review. Contact support.";

/** Tooltip/title for a disabled write control. */
export const ACCOUNT_READ_ONLY_CONTROL_TITLE =
  "Unavailable while your account is read-only (sending is paused for abuse review).";

/**
 * Recognizes the account_read_only envelope in a failed response body and
 * returns the dashboard copy for it, or null for any other body. Tolerates a
 * non-JSON body.
 */
export function readOnlyMessageFromBody(body: string): string | null {
  if (!body) return null;
  try {
    const parsed = JSON.parse(body) as { error?: { code?: unknown } };
    return parsed?.error?.code === ACCOUNT_READ_ONLY_CODE ? ACCOUNT_READ_ONLY_MESSAGE : null;
  } catch {
    return null;
  }
}
