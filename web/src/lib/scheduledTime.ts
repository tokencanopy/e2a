// Human "Sends …" label for a scheduled outbound send, in the viewer's local
// timezone. Mirrors the absolute format used elsewhere in the thread views
// ("Aug 1, 9:00 AM"); the year is appended only when it isn't the current one
// so the common (near-future) case stays terse.
export function formatScheduledSend(iso?: string): string {
  if (!iso) return "";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "";
  const opts: Intl.DateTimeFormatOptions = {
    month: "short",
    day: "numeric",
    hour: "numeric",
    minute: "2-digit",
  };
  if (d.getFullYear() !== new Date().getFullYear()) {
    opts.year = "numeric";
  }
  return `Sends ${d.toLocaleString(undefined, opts)}`;
}
