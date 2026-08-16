import { describe, it, expect } from "vitest";
import { RetryHttpLibrary } from "../../src/v1/retry.js";
import {
  HttpMethod,
  RequestContext,
  ResponseContext,
} from "../../src/v1/generated/http/http.js";
import type { HttpLibrary } from "../../src/v1/generated/http/http.js";
import { from, Observable } from "../../src/v1/generated/rxjsStub.js";

type Step = { status?: number; headers?: Record<string, string>; throw?: unknown; body?: string };

// A body stub that mimics a real fetch Response: `.text()` may only be
// consumed ONCE (a second call would throw "body stream already read" on a
// real fetch). Catches any regression where the retry layer's 409
// idempotency_in_flight body-peek forgets to memoize before handing the
// response back upstream.
function singleReadBody(content: string) {
  let read = false;
  return {
    text: async () => {
      if (read) throw new Error("body already read (stream consumed twice)");
      read = true;
      return content;
    },
    binary: async () => new Blob([content]),
  };
}

// Records what each attempt saw (method + idempotency key) and replays a script.
class FakeHttp implements HttpLibrary {
  public seenKeys: (string | undefined)[] = [];
  public methods: HttpMethod[] = [];
  constructor(private steps: Step[]) {}
  send(req: RequestContext): Observable<ResponseContext> {
    this.methods.push(req.getHttpMethod());
    this.seenKeys.push(req.getHeaders()["Idempotency-Key"]);
    const step = this.steps.shift();
    if (!step) throw new Error("FakeHttp: no scripted step left");
    if (step.throw !== undefined) return from(Promise.reject(step.throw));
    const body = singleReadBody(step.body ?? "") as never;
    return from(Promise.resolve(new ResponseContext(step.status!, step.headers ?? {}, body)));
  }
}

const noSleep = async () => {};
// A server-deduped POST (send/reply/forward/approve/rotate-secret/create-api-key/
// create-webhook): the generated layer emits an empty Idempotency-Key stub, which
// marks the op safe to retry.
const post = () => {
  const r = new RequestContext("https://api.e2a.dev/v1/agents/a@x.com/messages", HttpMethod.POST);
  r.setHeaderParam("Idempotency-Key", ""); // empty generated stub
  return r;
};
// A bare, non-idempotent POST (create agent/domain, reject, redeliver,
// verify, test): no server-honored key — must NOT be retried.
const barePost = (url = "https://api.e2a.dev/v1/agents") =>
  new RequestContext(url, HttpMethod.POST);
const get = () => new RequestContext("https://api.e2a.dev/v1/agents/a@x.com/messages", HttpMethod.GET);
const del = (url: string) => new RequestContext(url, HttpMethod.DELETE);
const patch = (url = "https://api.e2a.dev/v1/agents/a@x.com") =>
  new RequestContext(url, HttpMethod.PATCH);

describe("RetryHttpLibrary retry behavior", () => {
  it("retries a 500 then returns the 200", async () => {
    const fake = new FakeHttp([{ status: 500 }, { status: 200 }]);
    const retry = new RetryHttpLibrary(fake, { sleep: noSleep });
    const resp = await retry.send(post()).toPromise();
    expect(resp.httpStatusCode).toBe(200);
    expect(fake.methods.length).toBe(2);
  });

  it("retries a connection error then returns the 200", async () => {
    const fake = new FakeHttp([{ throw: new Error("ECONNREFUSED") }, { status: 200 }]);
    const retry = new RetryHttpLibrary(fake, { sleep: noSleep });
    const resp = await retry.send(post()).toPromise();
    expect(resp.httpStatusCode).toBe(200);
    expect(fake.methods.length).toBe(2);
  });

  it("does NOT retry a non-retryable status (404)", async () => {
    const fake = new FakeHttp([{ status: 404 }]);
    const retry = new RetryHttpLibrary(fake, { sleep: noSleep });
    const resp = await retry.send(post()).toPromise();
    expect(resp.httpStatusCode).toBe(404);
    expect(fake.methods.length).toBe(1);
  });

  it("stops after maxRetries and returns the last response", async () => {
    const fake = new FakeHttp([{ status: 503 }, { status: 503 }, { status: 503 }]);
    const retry = new RetryHttpLibrary(fake, { sleep: noSleep, maxRetries: 2 });
    const resp = await retry.send(post()).toPromise();
    expect(resp.httpStatusCode).toBe(503);
    expect(fake.methods.length).toBe(3); // 1 + 2 retries
  });

  it("honors Retry-After for the backoff delay", async () => {
    const delays: number[] = [];
    const fake = new FakeHttp([{ status: 429, headers: { "retry-after": "12" } }, { status: 200 }]);
    const retry = new RetryHttpLibrary(fake, {
      sleep: async (ms) => { delays.push(ms); },
      maxRetries: 1,
    });
    await retry.send(post()).toPromise();
    expect(delays).toEqual([12000]);
  });
});

// A transport that hangs until the request's signal aborts, then rejects with
// the abort reason — mirroring how fetch behaves under an AbortSignal. Lets the
// per-attempt timeout fire against a request that never resolves on its own.
class HangingHttp implements HttpLibrary {
  public attempts = 0;
  send(req: RequestContext): Observable<ResponseContext> {
    this.attempts += 1;
    const signal = req.getSignal?.();
    return from(
      new Promise<ResponseContext>((_resolve, reject) => {
        if (signal?.aborted) return reject(signal.reason);
        signal?.addEventListener("abort", () => reject(signal.reason), { once: true });
      }),
    );
  }
}

describe("RetryHttpLibrary timeout", () => {
  it("times out a hung retry-safe request, retries, then throws a timeout error", async () => {
    const fake = new HangingHttp();
    const retry = new RetryHttpLibrary(fake, { sleep: noSleep, maxRetries: 1, timeoutMs: 5 });
    await expect(retry.send(get()).toPromise()).rejects.toThrow(/timed out/);
    expect(fake.attempts).toBe(2); // 1 + 1 retry, each timed out
  });

  it("does NOT retry a hung bare POST on timeout (throws after one attempt)", async () => {
    const fake = new HangingHttp();
    const retry = new RetryHttpLibrary(fake, { sleep: noSleep, maxRetries: 2, timeoutMs: 5 });
    await expect(retry.send(barePost()).toPromise()).rejects.toThrow(/timed out/);
    expect(fake.attempts).toBe(1); // non-idempotent POST: not retried
  });

  it("does not time out a fast response", async () => {
    const fake = new FakeHttp([{ status: 200 }]);
    const retry = new RetryHttpLibrary(fake, { sleep: noSleep, timeoutMs: 1000 });
    const resp = await retry.send(get()).toPromise();
    expect(resp.httpStatusCode).toBe(200);
  });

  it("propagates a caller abort as an abort (not a timeout) and stops", async () => {
    const fake = new HangingHttp();
    const ctrl = new AbortController();
    const req = get();
    req.setSignal(ctrl.signal);
    const retry = new RetryHttpLibrary(fake, { sleep: noSleep, maxRetries: 3, timeoutMs: 10000 });
    const p = retry.send(req).toPromise();
    ctrl.abort(new Error("caller cancelled"));
    await expect(p).rejects.toThrow(/caller cancelled/);
    expect(fake.attempts).toBe(1); // aborted, not retried
  });

  it("timeoutMs:0 installs no timeout — request is bounded only by the caller signal", async () => {
    const fake = new HangingHttp();
    const ctrl = new AbortController();
    const req = get();
    req.setSignal(ctrl.signal);
    const retry = new RetryHttpLibrary(fake, { sleep: noSleep, maxRetries: 3, timeoutMs: 0 });
    const p = retry.send(req).toPromise();
    // With no SDK timeout the hung request never self-aborts; only the caller ends it.
    ctrl.abort(new Error("only the caller stops it"));
    await expect(p).rejects.toThrow(/only the caller stops it/);
    expect(fake.attempts).toBe(1);
  });
});

describe("RetryHttpLibrary idempotency", () => {
  it("mints an Idempotency-Key ONCE and reuses it across retries", async () => {
    let n = 0;
    const fake = new FakeHttp([{ status: 500 }, { status: 200 }]);
    const retry = new RetryHttpLibrary(fake, { sleep: noSleep, genIdempotencyKey: () => `k-${n++}` });
    await retry.send(post()).toPromise();
    expect(fake.seenKeys[0]).toBe("k-0");
    expect(fake.seenKeys[1]).toBe("k-0"); // same key on the retry — not regenerated
    expect(n).toBe(1); // generated exactly once
  });

  it("does not mint a key for a safe method (GET)", async () => {
    const fake = new FakeHttp([{ status: 200 }]);
    const retry = new RetryHttpLibrary(fake, { sleep: noSleep });
    await retry.send(get()).toPromise();
    expect(fake.seenKeys[0]).toBeUndefined();
  });

  it("mints a key when the header is present but empty (generated-layer stub)", async () => {
    // The generated send/reply/forward/approve unconditionally set an empty
    // Idempotency-Key header when the caller passes none. A present-but-empty
    // header must NOT be mistaken for a caller-supplied key.
    const req = post();
    req.setHeaderParam("Idempotency-Key", "");
    const fake = new FakeHttp([{ status: 500 }, { status: 200 }]);
    const retry = new RetryHttpLibrary(fake, { sleep: noSleep, genIdempotencyKey: () => "minted" });
    await retry.send(req).toPromise();
    expect(fake.seenKeys).toEqual(["minted", "minted"]);
  });

  it("preserves a caller-supplied Idempotency-Key", async () => {
    const req = post();
    req.setHeaderParam("Idempotency-Key", "caller-key");
    const fake = new FakeHttp([{ status: 500 }, { status: 200 }]);
    const retry = new RetryHttpLibrary(fake, { sleep: noSleep, genIdempotencyKey: () => "should-not-be-used" });
    await retry.send(req).toPromise();
    expect(fake.seenKeys).toEqual(["caller-key", "caller-key"]);
  });
});

// Regression tests for the independent + adversarial review findings.
describe("RetryHttpLibrary review fixes", () => {
  it("aborting the signal stops the retry loop (no further attempt)", async () => {
    const ctl = new AbortController();
    const req = post();
    req.setSignal(ctl.signal);
    const fake = new FakeHttp([{ status: 500 }, { status: 200 }]);
    // The injected backoff aborts; the next loop iteration must throw, not retry.
    const retry = new RetryHttpLibrary(fake, { sleep: async () => { ctl.abort(); }, maxRetries: 3 });
    await expect(retry.send(req).toPromise()).rejects.toBeDefined();
    expect(fake.methods.length).toBe(1); // only the first attempt ran
  });

  it("clamps an oversized Retry-After to maxRetryAfterMs", async () => {
    const delays: number[] = [];
    const fake = new FakeHttp([{ status: 503, headers: { "retry-after": "99999999" } }, { status: 200 }]);
    const retry = new RetryHttpLibrary(fake, {
      sleep: async (ms) => { delays.push(ms); },
      maxRetries: 1,
      maxRetryAfterMs: 5000,
    });
    await retry.send(post()).toPromise();
    expect(delays).toEqual([5000]); // not ~3 years
  });

  it("honors an HTTP-date Retry-After", async () => {
    const FIXED = 1700000000000;
    const dateHeader = new Date(FIXED + 2000).toUTCString();
    const delays: number[] = [];
    const fake = new FakeHttp([{ status: 503, headers: { "retry-after": dateHeader } }, { status: 200 }]);
    const retry = new RetryHttpLibrary(fake, {
      sleep: async (ms) => { delays.push(ms); },
      maxRetries: 1,
      now: () => FIXED,
    });
    await retry.send(post()).toPromise();
    expect(delays).toEqual([2000]);
  });

  it("stops once the total deadline (maxElapsedMs) would be exceeded", async () => {
    const fake = new FakeHttp([{ status: 503 }, { status: 503 }, { status: 503 }]);
    const retry = new RetryHttpLibrary(fake, {
      sleep: noSleep,
      random: () => 1, // max jitter → backoff(0) = 200ms
      now: () => 1000, // frozen clock: elapsed stays 0, but delay 200 > 100
      maxElapsedMs: 100,
      maxRetries: 5,
    });
    const resp = await retry.send(post()).toPromise();
    expect(resp.httpStatusCode).toBe(503);
    expect(fake.methods.length).toBe(1); // deadline blocked the first retry
  });
});

// Per-operation gating: only reads, HTTP-idempotent writes, and server-deduped
// (keyed) POSTs are retried. Non-idempotent POSTs and account deletion are not —
// retrying them after an ambiguous failure could double-create or re-fire an
// irreversible delete. Mirrors the Python SDK's executor gating.
describe("RetryHttpLibrary per-operation gating", () => {
  it("does NOT retry a bare (non-idempotent) POST create on 500", async () => {
    const fake = new FakeHttp([{ status: 500 }]);
    const retry = new RetryHttpLibrary(fake, { sleep: noSleep });
    const resp = await retry.send(barePost()).toPromise();
    expect(resp.httpStatusCode).toBe(500);
    expect(fake.methods.length).toBe(1); // not retried — avoids double-create
  });

  it("does NOT mint an Idempotency-Key for a bare POST create", async () => {
    const fake = new FakeHttp([{ status: 200 }]);
    const retry = new RetryHttpLibrary(fake, { sleep: noSleep, genIdempotencyKey: () => "x" });
    await retry.send(barePost()).toPromise();
    expect(fake.seenKeys[0]).toBeUndefined(); // no useless key the server would ignore
  });

  it("does NOT retry DELETE /v1/account (irreversible)", async () => {
    const fake = new FakeHttp([{ status: 500 }]);
    const retry = new RetryHttpLibrary(fake, { sleep: noSleep });
    const resp = await retry
      .send(del("https://api.e2a.dev/v1/account?confirm=DELETE"))
      .toPromise();
    expect(resp.httpStatusCode).toBe(500);
    expect(fake.methods.length).toBe(1);
  });

  it("DOES retry an idempotent DELETE (webhook) on 500", async () => {
    const fake = new FakeHttp([{ status: 500 }, { status: 204 }]);
    const retry = new RetryHttpLibrary(fake, { sleep: noSleep });
    const resp = await retry.send(del("https://api.e2a.dev/v1/webhooks/wh_1")).toPromise();
    expect(resp.httpStatusCode).toBe(204);
    expect(fake.methods.length).toBe(2);
  });

  it("mints one key for server-deduped domain DELETE retries", async () => {
    const req = del("https://api.e2a.dev/v1/domains/mail.example.test?confirm=DELETE");
    req.setHeaderParam("Idempotency-Key", "");
    const fake = new FakeHttp([{ status: 500 }, { status: 200 }]);
    const retry = new RetryHttpLibrary(fake, {
      sleep: noSleep,
      genIdempotencyKey: () => "domain-delete-key",
    });
    const resp = await retry.send(req).toPromise();
    expect(resp.httpStatusCode).toBe(200);
    expect(fake.seenKeys).toEqual(["domain-delete-key", "domain-delete-key"]);
  });

  it("DOES retry an idempotent PATCH on 500", async () => {
    const fake = new FakeHttp([{ status: 500 }, { status: 200 }]);
    const retry = new RetryHttpLibrary(fake, { sleep: noSleep });
    const resp = await retry.send(patch()).toPromise();
    expect(resp.httpStatusCode).toBe(200);
    expect(fake.methods.length).toBe(2);
  });
});

// 409 is NOT in isRetryableStatus (a bare 409 is never retried), but the
// server's idempotency_in_flight code on a server-deduped keyed write IS
// retry-safe (same key, still committing). Other 409 codes are not retried;
// idempotency_key_reuse is the separate frozen 422 caller-error contract.
describe("RetryHttpLibrary 409 idempotency_in_flight", () => {
  const envelope = (code: string) => JSON.stringify({ error: { code, message: "x" } });

  it("retries a 409 idempotency_in_flight then returns the 200", async () => {
    const fake = new FakeHttp([
      { status: 409, body: envelope("idempotency_in_flight") },
      { status: 200 },
    ]);
    const retry = new RetryHttpLibrary(fake, { sleep: noSleep });
    const resp = await retry.send(post()).toPromise();
    expect(resp.httpStatusCode).toBe(200);
    expect(fake.methods.length).toBe(2);
  });

  it("does NOT retry an unrelated 409, and the body stays readable once returned", async () => {
    const body = envelope("conflict");
    const fake = new FakeHttp([{ status: 409, body }]);
    const retry = new RetryHttpLibrary(fake, { sleep: noSleep });
    const resp = await retry.send(post()).toPromise();
    expect(resp.httpStatusCode).toBe(409);
    expect(fake.methods.length).toBe(1); // never retried — caller bug, not transient
    // The retry layer already peeked the body to read the code; a downstream
    // consumer (the generated Api layer building the thrown ApiException)
    // must still be able to read it exactly once more via the memoized body.
    await expect(resp.body.text()).resolves.toBe(body);
  });

  it("does NOT retry a 422 idempotency_key_reuse", async () => {
    const fake = new FakeHttp([{ status: 422, body: envelope("idempotency_key_reuse") }]);
    const retry = new RetryHttpLibrary(fake, { sleep: noSleep });
    const resp = await retry.send(post()).toPromise();
    expect(resp.httpStatusCode).toBe(422);
    expect(fake.methods.length).toBe(1);
  });

  it("does NOT retry a bare 409 carrying an unrelated code", async () => {
    const fake = new FakeHttp([{ status: 409, body: envelope("conflict") }]);
    const retry = new RetryHttpLibrary(fake, { sleep: noSleep });
    const resp = await retry.send(post()).toPromise();
    expect(resp.httpStatusCode).toBe(409);
    expect(fake.methods.length).toBe(1);
  });

  it("does NOT peek/retry a 409 idempotency_in_flight on a request with no Idempotency-Key", async () => {
    // GET is retry-safe in general, but carries no Idempotency-Key — this
    // response shape can't legitimately occur for it, and the gate must not
    // fire regardless.
    const fake = new FakeHttp([{ status: 409, body: envelope("idempotency_in_flight") }]);
    const retry = new RetryHttpLibrary(fake, { sleep: noSleep });
    const resp = await retry.send(get()).toPromise();
    expect(resp.httpStatusCode).toBe(409);
    expect(fake.methods.length).toBe(1);
  });

  it("routes the 409 retry through the existing Retry-After backoff", async () => {
    const delays: number[] = [];
    const fake = new FakeHttp([
      { status: 409, body: envelope("idempotency_in_flight"), headers: { "retry-after": "3" } },
      { status: 200 },
    ]);
    const retry = new RetryHttpLibrary(fake, {
      sleep: async (ms) => { delays.push(ms); },
      maxRetries: 1,
    });
    await retry.send(post()).toPromise();
    expect(delays).toEqual([3000]);
  });
});
