// Shared helpers for the account soft-deletion surfaces: the restore
// interstitial (/account/restore), the sign-in refusal page
// (/account/unavailable), the Settings delete flow, and the one-time
// "account restored" notice in the app shell.

/** GET /api/account/deletion — the restricted session's view of its trashed account. */
export type AccountDeletionView = {
  email: string;
  deleted_at: string | null;
  purge_after: string | null;
  purge_in_progress: boolean;
};

/** A non-2xx response, normalized from the standard error envelope. */
export type AccountApiError = {
  status: number;
  code: string;
  message: string;
};

/**
 * Reads the standard `{"error":{"code","message","request_id"}}` envelope.
 * Falls back to the raw body text when the body is not an envelope, so a
 * proxy's plain-text error still yields something legible.
 */
export async function readApiError(res: Response): Promise<AccountApiError> {
  const text = await res.text().catch(() => "");
  try {
    const parsed = JSON.parse(text) as { error?: { code?: unknown; message?: unknown } };
    const code = typeof parsed?.error?.code === "string" ? parsed.error.code : "";
    const message = typeof parsed?.error?.message === "string" ? parsed.error.message : "";
    if (code || message) return { status: res.status, code, message };
  } catch {
    // Not JSON — fall through to the raw text.
  }
  return { status: res.status, code: "", message: text.trim() };
}

/** Long, locale-aware calendar date ("September 20, 2026"). */
export function formatLongDate(iso: string | null | undefined): string {
  if (!iso) return "";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  try {
    return new Intl.DateTimeFormat(undefined, {
      year: "numeric",
      month: "long",
      day: "numeric",
    }).format(d);
  } catch {
    return iso;
  }
}

const DAY_MS = 24 * 60 * 60 * 1000;

/** Whole days until `iso`, rounded up; 0 once it has passed. */
export function daysUntil(iso: string | null | undefined, now: number = Date.now()): number {
  if (!iso) return 0;
  const t = new Date(iso).getTime();
  if (Number.isNaN(t)) return 0;
  return Math.max(0, Math.ceil((t - now) / DAY_MS));
}

/**
 * Fraction (0..1) of the trash window already elapsed. Drives the restore
 * page's window meter; null when either bound is missing or inverted so the
 * meter is omitted rather than drawn wrong.
 */
export function trashWindowElapsed(
  deletedAt: string | null | undefined,
  purgeAfter: string | null | undefined,
  now: number = Date.now(),
): number | null {
  if (!deletedAt || !purgeAfter) return null;
  const start = new Date(deletedAt).getTime();
  const end = new Date(purgeAfter).getTime();
  if (Number.isNaN(start) || Number.isNaN(end) || end <= start) return null;
  return Math.min(1, Math.max(0, (now - start) / (end - start)));
}
