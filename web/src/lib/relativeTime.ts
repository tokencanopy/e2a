// Compact "X ago" formatter used by every messaging surface (agent
// cards, thread rows, bubbles, review rows, etc.). Pure function with
// an injectable `now` for deterministic tests.
//
// Output policy:
//   • invalid / future timestamps → "—"
//   • <60s        → "just now"
//   • <60m        → "{N}m ago"
//   • <24h        → "{N}h ago"
//   • otherwise   → "{N}d ago"

export function formatRelativeAge(
  iso: string | null | undefined,
  now: Date = new Date(),
): string {
  if (!iso) return "—";
  const ts = new Date(iso).getTime();
  if (isNaN(ts)) return "—";
  const diff = now.getTime() - ts;
  if (diff < 0) return "—";
  const sec = Math.floor(diff / 1000);
  if (sec < 60) return "just now";
  const min = Math.floor(sec / 60);
  if (min < 60) return `${min}m ago`;
  const hr = Math.floor(min / 60);
  if (hr < 24) return `${hr}h ago`;
  return `${Math.floor(hr / 24)}d ago`;
}
