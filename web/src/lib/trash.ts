// Trash retention helpers shared by the account-wide /trash page and each
// inbox's Trash tab.
//
// TRASH_RETENTION_DAYS mirrors the backend window (identity.TrashRetention,
// default 30 days). Display-only — the janitor owns the real clock; if the
// backend window is ever tuned, update this constant (or, better, switch the
// API to emit a server-computed purge_at and delete this file).
import { formatLongDate } from "./accountDeletion";

export const TRASH_RETENTION_DAYS = 30;

// daysLeft returns the whole days remaining until a trashed resource is
// purged, given its deleted_at timestamp. Clamped at 0.
export function daysLeft(deletedAt: string): number {
  const purgeAt =
    new Date(deletedAt).getTime() + TRASH_RETENTION_DAYS * 24 * 3600 * 1000;
  return Math.max(0, Math.ceil((purgeAt - Date.now()) / (24 * 3600 * 1000)));
}

// A permanent ("Delete forever") delete the server deferred: the resource
// emailed recipients outside e2a recently, so it stays in the trash until
// purge_after instead of being purged now (receipt erase_deferred).
export function isDeferred(
  receipt: unknown,
): receipt is { erase_deferred: true; purge_after?: string } {
  return (
    typeof receipt === "object" &&
    receipt !== null &&
    (receipt as { erase_deferred?: unknown }).erase_deferred === true
  );
}

// deferredCopy is the notice shown for a deferred "Delete forever"; lead is
// the opening clause ("This inbox emailed people outside e2a recently").
export function deferredCopy(lead: string, purgeAfter?: string): string {
  const when = formatLongDate(purgeAfter);
  return (
    `${lead}, so it can't be deleted forever yet. ` +
    `It stays in the trash${when ? ` until ${when}` : ""}, then it's deleted automatically. You can still restore it until then.`
  );
}
