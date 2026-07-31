import { EXIT, fail } from "./exit.js";

// RFC 3339 date-time with an explicit UTC offset (Z or ±HH:MM). The offset is
// mandatory: `new Date()` reads a bare date-time as LOCAL time and a
// date-only value as UTC midnight, silently shifting the intended instant
// across timezones. Shared by scheduled sending (--send-at) and the contact
// timestamp filters so every CLI time argument follows the same rule.
const RFC3339_WITH_OFFSET =
  /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}(:\d{2})?(\.\d+)?(Z|[+-]\d{2}:\d{2})$/;

/**
 * Parse a CLI time argument into a Date, rejecting date-only and offsetless
 * values with a usage error. `flag` names the offending flag in the message;
 * `detail` (e.g. a command usage line) is appended when given.
 */
export function parseRfc3339(value: string, flag: string, detail?: string): Date {
  const suffix = detail ? `\n${detail}` : "";
  if (!RFC3339_WITH_OFFSET.test(value)) {
    return fail(
      EXIT.USAGE,
      `${flag} must be an RFC 3339 date-time WITH an explicit offset, e.g. 2026-08-01T09:00:00Z or 2026-08-01T09:00:00-07:00 (got "${value}")${suffix}`,
    );
  }
  const at = new Date(value);
  if (Number.isNaN(at.getTime())) {
    return fail(
      EXIT.USAGE,
      `${flag} is not a valid date-time: "${value}" (use RFC 3339, e.g. 2026-08-01T09:00:00Z)${suffix}`,
    );
  }
  return at;
}
