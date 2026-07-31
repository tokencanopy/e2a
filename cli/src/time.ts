import { EXIT, fail } from "./exit.js";

// RFC 3339 date-time with an explicit UTC offset (Z or ±HH:MM). The offset is
// mandatory: `new Date()` reads a bare date-time as LOCAL time and a
// date-only value as UTC midnight, silently shifting the intended instant
// across timezones. Shared by scheduled sending (--send-at) and the contact
// timestamp filters so every CLI time argument follows the same rule.
const RFC3339_WITH_OFFSET =
  /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})(?::(\d{2}))?(\.\d+)?(Z|[+-]\d{2}:\d{2})$/;

/**
 * Parse a CLI time argument into a Date, rejecting date-only, offsetless, and
 * impossible values with a usage error. `flag` names the offending flag in
 * the message; `detail` (e.g. a command usage line) is appended when given.
 *
 * Impossible fields (2026-02-30, 24:00, +99:00) are rejected by round-trip:
 * the JS Date constructor ROLLS them over on modern V8 instead of returning
 * NaN, so a NaN check alone is not a validation. Fields are re-read from the
 * computed instant and must equal the input's.
 */
export function parseRfc3339(value: string, flag: string, detail?: string): Date {
  const suffix = detail ? `\n${detail}` : "";
  const invalid = (why: string): never =>
    fail(EXIT.USAGE, `${flag} is not a valid date-time: "${value}" (${why}; use RFC 3339, e.g. 2026-08-01T09:00:00Z)${suffix}`);

  const m = RFC3339_WITH_OFFSET.exec(value);
  if (!m) {
    return fail(
      EXIT.USAGE,
      `${flag} must be an RFC 3339 date-time WITH an explicit offset, e.g. 2026-08-01T09:00:00Z or 2026-08-01T09:00:00-07:00 (got "${value}")${suffix}`,
    );
  }
  const [, yearS, monthS, dayS, hourS, minuteS, secondS, fracS, offsetS] = m;
  const year = Number(yearS), month = Number(monthS), day = Number(dayS);
  const hour = Number(hourS), minute = Number(minuteS), second = secondS === undefined ? 0 : Number(secondS);
  const fracMs = fracS === undefined ? 0 : Math.round(Number(fracS) * 1000);
  if (month < 1 || month > 12 || day < 1 || day > 31 || hour > 23 || minute > 59 || second > 59) {
    return invalid("field out of range");
  }
  // Component round-trip in UTC catches the impossible dates (Feb 30, Apr 31)
  // that the Date constructor would silently roll into the next month.
  const base = Date.UTC(year, month - 1, day, hour, minute, second);
  const rt = new Date(base);
  if (
    rt.getUTCFullYear() !== year || rt.getUTCMonth() !== month - 1 || rt.getUTCDate() !== day ||
    rt.getUTCHours() !== hour || rt.getUTCMinutes() !== minute || rt.getUTCSeconds() !== second
  ) {
    return invalid("impossible calendar date or time");
  }
  let epoch = base + fracMs;
  if (offsetS !== "Z") {
    const offsetHour = Number(offsetS.slice(1, 3)), offsetMinute = Number(offsetS.slice(4, 6));
    if (offsetHour > 23 || offsetMinute > 59) {
      return invalid("offset out of range");
    }
    epoch -= (offsetS[0] === "+" ? 1 : -1) * (offsetHour * 60 + offsetMinute) * 60_000;
  }
  return new Date(epoch);
}
