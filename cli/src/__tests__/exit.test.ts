import { describe, expect, it } from "vitest";
import { EXIT, exitCodeForAPIError } from "../exit.js";

describe("API error exit classification", () => {
  it("reserves AUTH for credentials/scope and treats policy rejection as permanent request", () => {
    expect(exitCodeForAPIError({ code: "unauthorized", retryable: false })).toBe(EXIT.AUTH);
    expect(exitCodeForAPIError({ code: "forbidden", retryable: false })).toBe(EXIT.AUTH);
    expect(exitCodeForAPIError({ code: "blocked_by_policy", retryable: false })).toBe(EXIT.REQUEST);
  });

  it("keeps retryable API errors transient", () => {
    expect(exitCodeForAPIError({ code: "rate_limited", retryable: true })).toBe(EXIT.ERROR);
  });

  it("maps external_sending_not_enabled to the permanent-request code, not AUTH", () => {
    // It's a 403 PERMISSION error like forbidden/sending_paused, but only
    // unauthorized/forbidden get the AUTH bucket (fix your key) — this one is
    // "your recipients are out of scope for this key", which is REQUEST (5):
    // do not retry the identical invocation, but the credential itself is fine.
    expect(
      exitCodeForAPIError({ code: "external_sending_not_enabled", retryable: false }),
    ).toBe(EXIT.REQUEST);
  });
});

describe("account_read_only exit classification", () => {
  it("maps account_read_only to its own READ_ONLY code, not AUTH or REQUEST", () => {
    // The credential is valid (not AUTH) and the invocation is fine (not
    // REQUEST): the account itself is frozen for an abuse review, so no write
    // succeeds until an operator resumes it.
    expect(exitCodeForAPIError({ code: "account_read_only", retryable: false })).toBe(EXIT.READ_ONLY);
  });
});

describe("exit code contract", () => {
  it("published values are frozen — add codes, never renumber", () => {
    expect(EXIT.OK).toBe(0);
    expect(EXIT.ERROR).toBe(1);
    expect(EXIT.USAGE).toBe(2);
    expect(EXIT.HELD).toBe(3);
    expect(EXIT.AUTH).toBe(4);
    expect(EXIT.REQUEST).toBe(5);
    expect(EXIT.TIMEOUT).toBe(6);
    expect(EXIT.SEND_OUTCOME).toBe(7);
    expect(EXIT.WARN).toBe(8);
    expect(EXIT.CONFIG).toBe(9);
    expect(EXIT.READ_ONLY).toBe(10);
  });
});
