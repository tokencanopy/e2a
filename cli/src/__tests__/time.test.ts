import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";

// parseRfc3339 is the single gate every CLI time argument passes through
// (--send-at, contact filters, next actions). These tests pin the two ways a
// naive new Date(raw) silently lies: reading offsetless values in local time,
// and ROLLING OVER impossible calendar dates on modern V8 instead of
// returning NaN.

describe("parseRfc3339", () => {
  beforeEach(() => {
    vi.spyOn(process.stderr, "write").mockImplementation(() => true);
    vi.spyOn(process, "exit").mockImplementation(() => {
      throw new Error("process.exit");
    });
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("accepts Z and explicit ±HH:MM offsets as the same unambiguous instant", async () => {
    const { parseRfc3339 } = await import("../time.js");
    expect(parseRfc3339("2026-08-01T09:00:00Z", "--x").toISOString()).toBe("2026-08-01T09:00:00.000Z");
    expect(parseRfc3339("2026-08-01T09:00:00-07:00", "--x").toISOString()).toBe("2026-08-01T16:00:00.000Z");
    expect(parseRfc3339("2026-08-01T09:00:00+05:30", "--x").toISOString()).toBe("2026-08-01T03:30:00.000Z");
    // Seconds may be omitted per RFC 3339; fractional seconds survive.
    expect(parseRfc3339("2026-08-01T09:00-07:00", "--x").toISOString()).toBe("2026-08-01T16:00:00.000Z");
    expect(parseRfc3339("2026-08-01T09:00:00.250Z", "--x").getMilliseconds()).toBe(250);
  });

  it("rejects date-only and offsetless values", async () => {
    const { parseRfc3339 } = await import("../time.js");
    for (const value of ["2026-08-01", "2026-08-01T09:00:00", "2026-08-01 09:00:00Z", "not-a-date"]) {
      expect(() => parseRfc3339(value, "--x")).toThrow("process.exit");
    }
    expect(process.stderr.write).toHaveBeenCalledWith(expect.stringContaining("explicit offset"));
  });

  it("rejects impossible dates the Date constructor would roll over", async () => {
    const { parseRfc3339 } = await import("../time.js");
    // Modern V8 turns these into Mar 2 / May 1 / next-day 00:00 instead of
    // NaN — the NaN backstop alone never fires.
    for (const value of [
      "2026-02-30T09:00:00Z",
      "2026-04-31T09:00:00Z",
      "2026-08-01T24:00:00Z",
      "2026-08-01T09:60:00Z",
      "2026-13-01T09:00:00Z",
      "2026-08-01T09:00:00+24:00",
      "2026-08-01T09:00:00+05:60",
    ]) {
      expect(() => parseRfc3339(value, "--x"), value).toThrow("process.exit");
    }
  });

  it("keeps leap years and month lengths honest", async () => {
    const { parseRfc3339 } = await import("../time.js");
    expect(parseRfc3339("2028-02-29T09:00:00Z", "--x").toISOString()).toBe("2028-02-29T09:00:00.000Z");
    expect(() => parseRfc3339("2027-02-29T09:00:00Z", "--x")).toThrow("process.exit");
  });

  it("matches the MCP/zod rule on its three edge divergences", async () => {
    const { parseRfc3339 } = await import("../time.js");
    // Years 0000–0099 are valid RFC 3339 and zod accepts them; Date.UTC would
    // map them to 19xx, so the round-trip must not use it.
    expect(parseRfc3339("0026-08-01T09:00:00Z", "--x").getUTCFullYear()).toBe(26);
    // A fraction requires seconds (RFC 3339 time-second is not optional when
    // time-secfrac is present).
    expect(() => parseRfc3339("2026-08-01T09:00.500Z", "--x")).toThrow("process.exit");
    // Sub-millisecond fractions truncate like server-side parsing; rounding
    // would roll .9999 into the next second.
    expect(parseRfc3339("2026-08-01T09:00:00.9999Z", "--x").toISOString()).toBe("2026-08-01T09:00:00.999Z");
  });
});
