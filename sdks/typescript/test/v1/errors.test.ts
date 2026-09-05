import { describe, it, expect } from "vitest";
import {
  E2AError,
  E2AAuthError,
  E2APermissionError,
  E2ANotFoundError,
  E2AConflictError,
  E2AValidationError,
  E2AIdempotencyError,
  E2ALimitExceededError,
  E2ARateLimitError,
  E2AServerError,
  E2AConnectionError,
  toE2AError,
  fromApiException,
  connectionError,
  isRetryableStatus,
} from "../../src/v1/errors.js";
import { ApiException } from "../../src/v1/generated/apis/exception.js";

describe("toE2AError status → class mapping", () => {
  const cases: Array<[number, any, boolean]> = [
    [401, E2AAuthError, false],
    [402, E2ALimitExceededError, false],
    [403, E2APermissionError, false],
    [404, E2ANotFoundError, false],
    [410, E2ANotFoundError, false],
    [409, E2AConflictError, false],
    [422, E2AValidationError, false],
    [429, E2ARateLimitError, true],
    [500, E2AServerError, true],
    [503, E2AServerError, true],
  ];
  for (const [status, ctor, retryable] of cases) {
    it(`${status} → ${ctor.name} (retryable=${retryable})`, () => {
      const err = toE2AError({ status, code: "x", message: "m" });
      expect(err).toBeInstanceOf(ctor);
      expect(err).toBeInstanceOf(E2AError);
      expect(err.status).toBe(status);
      expect(err.retryable).toBe(retryable);
    });
  }
});

describe("idempotency codes route to E2AIdempotencyError", () => {
  it("idempotency_in_flight (409) is retryable", () => {
    const err = toE2AError({ status: 409, code: "idempotency_in_flight", message: "in flight" });
    expect(err).toBeInstanceOf(E2AIdempotencyError);
    expect(err.retryable).toBe(true);
  });
  it("idempotency_key_reuse (422) is NOT retryable", () => {
    const err = toE2AError({ status: 422, code: "idempotency_key_reuse", message: "reuse" });
    expect(err).toBeInstanceOf(E2AIdempotencyError);
    expect(err.retryable).toBe(false);
  });
});

describe("the permanent 402 (quota) / 429 (rate) split", () => {
  it("limit_exceeded (402) → E2ALimitExceededError, NOT retryable", () => {
    const err = toE2AError({ status: 402, code: "limit_exceeded", message: "monthly cap" });
    expect(err).toBeInstanceOf(E2ALimitExceededError);
    expect(err).not.toBeInstanceOf(E2ARateLimitError);
    expect(err.retryable).toBe(false);
  });
  it("rate_limited (429) → E2ARateLimitError, retryable", () => {
    const err = toE2AError({ status: 429, code: "rate_limited", message: "slow down" });
    expect(err).toBeInstanceOf(E2ARateLimitError);
    expect(err).not.toBeInstanceOf(E2ALimitExceededError);
    expect(err.retryable).toBe(true);
  });
  it("limit_exceeded code wins even on an unexpected status (code-first)", () => {
    const err = toE2AError({ status: 400, code: "limit_exceeded", message: "cap" });
    expect(err).toBeInstanceOf(E2ALimitExceededError);
    expect(err.retryable).toBe(false);
  });
});

describe("envelope details + headers", () => {
  it("preserves code, message, details, requestId, retry-after", () => {
    const err = toE2AError({
      status: 429,
      code: "rate_limited",
      message: "slow down",
      details: { resource: "send" },
      requestId: "req_abc",
      headers: { "retry-after": "12" },
    });
    expect(err.code).toBe("rate_limited");
    expect(err.message).toBe("slow down");
    expect(err.requestId).toBe("req_abc");
    expect(err.details).toEqual({ resource: "send" });
    expect(err.retryAfterSeconds).toBe(12);
  });

  it("unknown code falls back to the status bucket, not a bare E2AError", () => {
    const err = toE2AError({ status: 404, code: "recipient_unknown", message: "no" });
    expect(err).toBeInstanceOf(E2ANotFoundError);
    expect(err.code).toBe("recipient_unknown");
  });

  it("parses an HTTP-date Retry-After into retryAfterSeconds", () => {
    const err = toE2AError({
      status: 429,
      headers: { "retry-after": new Date(Date.now() + 10_000).toUTCString() },
    });
    // ~10s out, allowing for second-truncation + test timing.
    expect(err.retryAfterSeconds).toBeGreaterThanOrEqual(8);
    expect(err.retryAfterSeconds).toBeLessThanOrEqual(11);
  });
});

describe("code-first class selection (F2)", () => {
  it("maps a known code by code even on an unexpected status", () => {
    // `forbidden` arriving on a 400 must still be a permission error, not
    // validation (the status bucket) and not a bare E2AError.
    const err = toE2AError({ status: 400, code: "forbidden", message: "nope" });
    expect(err).toBeInstanceOf(E2APermissionError);
    expect(err.code).toBe("forbidden");
  });

  it("maps idempotency codes by code regardless of status", () => {
    const inFlight = toE2AError({ status: 200, code: "idempotency_in_flight", message: "x" });
    expect(inFlight).toBeInstanceOf(E2AIdempotencyError);
    expect(inFlight.retryable).toBe(true);
    const reuse = toE2AError({ status: 200, code: "idempotency_key_reuse", message: "x" });
    expect(reuse).toBeInstanceOf(E2AIdempotencyError);
    expect(reuse.retryable).toBe(false);
  });

  it("maps the *_not_found / *_taken / *_exists / invalid_* code families by pattern", () => {
    expect(toE2AError({ status: 400, code: "agent_not_found", message: "x" })).toBeInstanceOf(
      E2ANotFoundError,
    );
    expect(toE2AError({ status: 200, code: "slug_exists", message: "x" })).toBeInstanceOf(
      E2AConflictError,
    );
    // The *_taken conflict family (agent_taken / domain_taken / alias_taken).
    for (const code of ["agent_taken", "domain_taken", "alias_taken"]) {
      const err = toE2AError({ status: 200, code, message: "x" });
      expect(err).toBeInstanceOf(E2AConflictError);
      expect(err.retryable).toBe(false);
    }
    // invalid_* refinements of invalid_request are validation errors even when
    // the individual code is not in the table.
    for (const code of ["invalid_slug", "invalid_filter", "invalid_expires_at"]) {
      expect(toE2AError({ status: 200, code, message: "x" })).toBeInstanceOf(E2AValidationError);
    }
  });

  it("maps the published catalog's family overrides code-first", () => {
    // 501 not_implemented / events_log_disabled: server family but NOT
    // retryable — while a plain 501 with an unknown code stays retryable.
    for (const code of ["not_implemented", "events_log_disabled"]) {
      const err = toE2AError({ status: 501, code, message: "x" });
      expect(err).toBeInstanceOf(E2AServerError);
      expect(err.retryable).toBe(false);
    }
    expect(toE2AError({ status: 501, code: "totally_unknown_code", message: "x" }).retryable).toBe(
      true,
    );
    // 400 fixed per-account caps map to the quota family, not validation.
    for (const code of ["template_limit_reached", "webhook_limit_reached"]) {
      const err = toE2AError({ status: 400, code, message: "x" });
      expect(err).toBeInstanceOf(E2ALimitExceededError);
      expect(err.retryable).toBe(false);
    }
    // Outbound policy + review-hold + suppression + retention codes.
    expect(toE2AError({ status: 403, code: "blocked_by_policy", message: "x" })).toBeInstanceOf(
      E2APermissionError,
    );
    expect(toE2AError({ status: 403, code: "sending_paused", message: "x" })).toBeInstanceOf(
      E2APermissionError,
    );
    expect(toE2AError({ status: 403, code: "sending_paused", message: "x" }).retryable).toBe(false);
    expect(toE2AError({ status: 409, code: "message_not_pending", message: "x" })).toBeInstanceOf(
      E2AConflictError,
    );
    expect(toE2AError({ status: 422, code: "recipient_suppressed", message: "x" })).toBeInstanceOf(
      E2AValidationError,
    );
    expect(toE2AError({ status: 410, code: "gone", message: "x" })).toBeInstanceOf(
      E2ANotFoundError,
    );
  });

  it("unknown code falls back to the status bucket (regression: mappings unchanged)", () => {
    const cases: Array<[number, any]> = [
      [401, E2AAuthError],
      [402, E2ALimitExceededError],
      [403, E2APermissionError],
      [404, E2ANotFoundError],
      [410, E2ANotFoundError],
      [409, E2AConflictError],
      [422, E2AValidationError],
      [429, E2ARateLimitError],
      [500, E2AServerError],
    ];
    for (const [status, ctor] of cases) {
      const err = toE2AError({ status, code: "totally_unknown_code", message: "m" });
      expect(err).toBeInstanceOf(ctor);
      expect(err.code).toBe("totally_unknown_code");
    }
  });
});

describe("retry_after from details (F3)", () => {
  it("reads details.retry_after_seconds when the header is absent", () => {
    const err = toE2AError({
      status: 429,
      code: "rate_limited",
      message: "slow down",
      details: { retry_after_seconds: 30 },
    });
    expect(err).toBeInstanceOf(E2ARateLimitError);
    expect(err.retryAfterSeconds).toBe(30);
  });

  it("prefers the Retry-After header over details when both are present", () => {
    const err = toE2AError({
      status: 429,
      code: "rate_limited",
      message: "slow down",
      details: { retry_after_seconds: 30 },
      headers: { "retry-after": "5" },
    });
    expect(err.retryAfterSeconds).toBe(5);
  });
});

describe("fromApiException", () => {
  it("parses the {error:{code,message}} envelope + x-request-id header", () => {
    const apiEx = new ApiException(
      403,
      "HTTP 403",
      { error: { code: "forbidden", message: "this agent-scoped credential is bound to a different agent", details: null } },
      { "x-request-id": "req_xyz", "content-type": "application/json" },
    );
    const err = fromApiException(apiEx);
    expect(err).toBeInstanceOf(E2APermissionError);
    expect(err.code).toBe("forbidden");
    expect(err.message).toBe("this agent-scoped credential is bound to a different agent");
    expect(err.requestId).toBe("req_xyz");
  });

  it("tolerates a non-envelope body", () => {
    const apiEx = new ApiException(500, "boom", "plain text", {});
    const err = fromApiException(apiEx);
    expect(err).toBeInstanceOf(E2AServerError);
    expect(err.status).toBe(500);
    expect(err.retryable).toBe(true);
  });

  // Regression: operations that declare ONLY success codes in the spec
  // (sendMessage / replyToMessage / forwardMessage) hand back the RAW body
  // STRING, not the parsed envelope. The machine `code` must still be recovered
  // — otherwise it degrades to the generic status bucket and the raw body leaks.
  it("parses the envelope from a RAW STRING body (send/reply/forward path)", () => {
    const raw = JSON.stringify({
      error: { code: "domain_not_verified", message: "the sending domain is not verified", request_id: "req_str" },
    });
    const apiEx = new ApiException(403, "Unknown API Status Code!", raw, {});
    const err = fromApiException(apiEx);
    expect(err.code).toBe("domain_not_verified"); // not the generic "forbidden"
    expect(err.message).toBe("the sending domain is not verified"); // not the raw dump
    expect(err.requestId).toBe("req_str"); // recovered from the body when no header
  });

  it("leaves a non-JSON string body as the status-bucket fallback", () => {
    const apiEx = new ApiException(403, "nope", "<html>gateway</html>", {});
    const err = fromApiException(apiEx);
    expect(err).toBeInstanceOf(E2APermissionError); // status bucket, no crash
    expect(err.code).not.toBe("domain_not_verified"); // didn't hallucinate a code
  });

  // Regression: the generated ApiException.message is a full dump (raw body +
  // all headers). fromApiException must NEVER pass that through — for a
  // non-envelope error it synthesizes a clean message instead, so the raw body,
  // headers, and any embedded secrets never reach a caller (or, via MCP, an
  // agent's context).
  it("never leaks the raw body/headers dump when the body isn't an envelope", () => {
    const apiEx = new ApiException(
      502,
      "Unknown API Status Code!",
      "upstream <html>Bad Gateway</html> req_BODY_SECRET not-json",
      { "x-request-id": "req_HDR_SECRET", "set-cookie": "session=secret" },
    );
    const err = fromApiException(apiEx);
    expect(err.message).toMatch(/e2a API error \(502\)/); // clean synthetic message
    for (const leak of ["req_BODY_SECRET", "<html>", "Headers:", "set-cookie", "session=secret"]) {
      expect(err.message).not.toContain(leak);
    }
  });
});

describe("connectionError + isRetryableStatus", () => {
  it("connectionError is status 0, retryable", () => {
    const err = connectionError("ECONNREFUSED", new Error("refused"));
    expect(err).toBeInstanceOf(E2AConnectionError);
    expect(err.status).toBe(0);
    expect(err.retryable).toBe(true);
  });
  it("isRetryableStatus", () => {
    expect(isRetryableStatus(429)).toBe(true);
    expect(isRetryableStatus(408)).toBe(true);
    expect(isRetryableStatus(503)).toBe(true);
    expect(isRetryableStatus(404)).toBe(false);
    expect(isRetryableStatus(200)).toBe(false);
  });
});
