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

// True when a scheduled send's fire time has already passed but it is still
// pending (e.g. deferred by the daily send cap). The scheduled queue surfaces
// these rather than hiding them, so the UI flags them distinctly.
export function isScheduleOverdue(iso?: string): boolean {
  if (!iso) return false;
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return false;
  return d.getTime() <= Date.now();
}

// Label for an overdue-but-pending scheduled send. Mirrors the absolute format
// of formatScheduledSend but frames it as a missed fire time still awaiting send.
export function formatOverdueSend(iso?: string): string {
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
  return `Overdue · was due ${d.toLocaleString(undefined, opts)}`;
}
