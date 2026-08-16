// Retry + idempotency for the e2a SDK (Slice 8b-1).
//
// Implemented as a wrapping HttpLibrary rather than generated middleware: middleware
// can transform a request/response but cannot re-send, and re-sending is the
// whole point. Because we re-send the SAME RequestContext, the Idempotency-Key
// header and the already-serialized body are reused verbatim across attempts —
// which is exactly what makes a POST retry safe (the server dedupes on the key
// and hashes the raw bytes). The key is minted ONCE, before the first attempt.

import { from, Observable } from "./generated/rxjsStub.js";
import { HttpMethod } from "./generated/http/http.js";
import type { HttpLibrary, RequestContext, ResponseBody, ResponseContext } from "./generated/http/http.js";
import { isRetryableStatus, parseErrorEnvelope } from "./errors.js";

export interface RetryOptions {
  /** Max retry attempts after the first try. Default 2. */
  maxRetries?: number;
  /** Base backoff in ms (doubles per attempt). Default 200. */
  baseDelayMs?: number;
  /** Exponential-backoff ceiling in ms. Default 8000. */
  maxDelayMs?: number;
  /** Upper bound for an honored Retry-After, in ms. Default 60000 — a hostile
   *  or buggy upstream can otherwise wedge a request for years. */
  maxRetryAfterMs?: number;
  /** Optional total deadline across all attempts (incl. backoff), in ms. When
   *  the next sleep would exceed it, stop and return/throw the last result. */
  maxElapsedMs?: number;
  /** Per-attempt request timeout in ms. A timed-out attempt aborts the in-flight
   *  fetch and is surfaced as a (retryable) connection failure, so it composes
   *  with maxRetries/maxElapsedMs. Undefined or <=0 disables. */
  timeoutMs?: number;
  /** Injectable for tests. */
  sleep?: (ms: number) => Promise<void>;
  random?: () => number;
  genIdempotencyKey?: () => string;
  now?: () => number;
}

const IDEMPOTENCY_HEADER = "Idempotency-Key";

// 409 code the server uses to signal "same Idempotency-Key already in flight
// on another attempt" (internal/httpapi/idempotency.go) — safe to retry (the
// server dedupes on the key). idempotency_key_reuse is a 422 body-mismatch
// caller bug and must NEVER be
// retried, so the code string is matched exactly rather than the status alone.
const IDEMPOTENCY_IN_FLIGHT_CODE = "idempotency_in_flight";

function defaultSleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

function abortError(reason?: unknown): unknown {
  if (reason !== undefined) return reason;
  return new DOMException("The operation was aborted", "AbortError");
}

function defaultUuid(): string {
  // Every supported runtime (Node 18+, browsers, edge) has crypto.randomUUID;
  // the fallback is a non-crypto last resort and is effectively unreachable.
  const c = (globalThis as { crypto?: { randomUUID?: () => string } }).crypto;
  if (c?.randomUUID) return c.randomUUID();
  return `${Date.now().toString(16)}-${Math.random().toString(16).slice(2)}${Math.random().toString(16).slice(2)}`;
}

export class RetryHttpLibrary implements HttpLibrary {
  constructor(
    private readonly inner: HttpLibrary,
    private readonly opts: RetryOptions = {},
  ) {}

  send(request: RequestContext): Observable<ResponseContext> {
    return from(this.sendWithRetry(request));
  }

  private async sendWithRetry(request: RequestContext): Promise<ResponseContext> {
    // Decide retry-safety BEFORE mutating headers — it depends on whether the
    // generated layer already emitted an Idempotency-Key stub (server-deduped
    // POSTs) which ensureIdempotencyKey would otherwise fill in and obscure.
    const retrySafe = this.isRetrySafe(request);
    if (retrySafe) this.ensureIdempotencyKey(request);
    const max = this.opts.maxRetries ?? 2;
    const now = this.opts.now ?? Date.now;
    const start = now();
    const signal = request.getSignal?.();

    let attempt = 0;
    for (;;) {
      this.throwIfAborted(signal);

      // Bound this attempt: a signal that aborts on the caller's signal OR after
      // timeoutMs. Set on the shared RequestContext so the generated fetch layer
      // honors it.
      const att = this.attemptSignal(signal, this.opts.timeoutMs);
      if (att.signal) request.setSignal(att.signal);

      let resp: ResponseContext | undefined;
      let connErr: unknown;
      try {
        resp = await this.inner.send(request).toPromise();
      } catch (e) {
        // A timeout aborts the fetch; surface it as a connection-level failure
        // (retried like any other for retry-safe requests) with a clear message
        // rather than the opaque AbortError.
        connErr = att.timedOut() ? new Error(`request timed out after ${this.opts.timeoutMs}ms`) : e;
      } finally {
        att.cleanup();
      }

      // A genuine caller abort (not our timeout) stops immediately — don't
      // reclassify it as a retryable connection error below.
      this.throwIfAborted(signal);

      const isConn = resp === undefined;
      let retryable = retrySafe && (isConn || isRetryableStatus(resp!.httpStatusCode));
      // A bare 409 is not in isRetryableStatus, but idempotency_in_flight is
      // retry-safe. idempotency_key_reuse is a non-retryable 422. Only
      // server-deduped keyed writes
      // (the population that can ever see this code) pay the cost of parsing
      // the body to tell the two apart.
      if (!retryable && retrySafe && !isConn && resp!.httpStatusCode === 409 && this.hasIdempotencyHeader(request)) {
        retryable = await this.isIdempotencyInFlight(resp!);
      }
      if (!retryable || attempt >= max) {
        if (resp !== undefined) return resp;
        throw connErr;
      }

      const delay = this.backoffMs(attempt, resp);
      // Total-deadline guard: don't start a wait that would blow the deadline.
      if (this.opts.maxElapsedMs !== undefined && now() - start + delay > this.opts.maxElapsedMs) {
        if (resp !== undefined) return resp;
        throw connErr;
      }
      await this.sleep(delay, signal);
      attempt += 1;
    }
  }

  // Whether this request may be safely re-sent after a transient failure.
  //
  // Retrying a non-idempotent POST after an ambiguous failure can double-create
  // a resource: the server commits the write, then the connection drops or a 5xx
  // is returned, and a blind retry creates a SECOND row. So we only retry:
  //   - reads (GET/HEAD/OPTIONS) — no side effects;
  //   - HTTP-idempotent writes (PUT/PATCH/DELETE) — repeating reaches the same
  //     end state — EXCEPT account deletion, which is irreversible and whose
  //     post-success retry would surface a spurious 404 to the caller;
  //   - server-deduped POSTs (send/reply/forward/approve, rotate-secret,
  //     create-api-key, create-webhook),
  //     recognised by the Idempotency-Key header the generated layer emits ONLY
  //     for those ops — the server replays the first result on a keyed retry.
  // Every other POST (create agent/domain, reject, verify, redeliver, test)
  // carries no server-honored key and is NOT retried.
  // Mirrors the Python SDK's per-operation retry gating (rotate-secret →
  // _write_idempotent there, retried + keyed, same as here).
  private isRetrySafe(request: RequestContext): boolean {
    const method = request.getHttpMethod();
    switch (method) {
      case HttpMethod.GET:
      case HttpMethod.HEAD:
      case HttpMethod.OPTIONS:
        return true;
      case HttpMethod.PUT:
      case HttpMethod.PATCH:
        return true;
      case HttpMethod.DELETE:
        return !this.isAccountDeletion(request.getUrl());
      case HttpMethod.POST:
        return this.hasIdempotencyHeader(request);
      default:
        return false;
    }
  }

  // The generated layer sets an Idempotency-Key header param (possibly an empty
  // stub) on the server-deduped POSTs. Its mere presence — not its
  // value — marks an op the server will dedupe, hence safe to retry.
  private hasIdempotencyHeader(request: RequestContext): boolean {
    const headers = request.getHeaders();
    for (const k of Object.keys(headers)) {
      if (k.toLowerCase() === IDEMPOTENCY_HEADER.toLowerCase()) return true;
    }
    return false;
  }

  // Whether a 409 response's envelope carries `error.code ===
  // "idempotency_in_flight"`. Wraps resp.body in a memoizing decorator FIRST
  // (and reassigns it onto resp) so reading it here doesn't consume the
  // underlying fetch body stream out from under the generated layer, which
  // reads response.body.text() again once this returns.
  private async isIdempotencyInFlight(resp: ResponseContext): Promise<boolean> {
    resp.body = this.memoizeBody(resp.body);
    try {
      const text = await resp.body.text();
      return parseErrorEnvelope(text)?.error?.code === IDEMPOTENCY_IN_FLIGHT_CODE;
    } catch {
      return false; // unreadable/unparseable body — treat as a bare 409, don't retry
    }
  }

  private memoizeBody(body: ResponseBody): ResponseBody {
    let cached: Promise<string> | undefined;
    return {
      text: () => (cached ??= body.text()),
      binary: () => body.binary(),
    };
  }

  // DELETE /v1/account (account deletion) — irreversible; exclude from retry.
  private isAccountDeletion(url: string): boolean {
    const path = url.split("?")[0];
    return path.endsWith("/v1/account") || path.endsWith("/account");
  }

  // Mint an Idempotency-Key once, before the first attempt, for a server-deduped
  // POST or DELETE the caller didn't already key. Setting it on the shared
  // RequestContext means every retry of this request reuses the same key.
  //
  // The generated layer calls setHeaderParam("Idempotency-Key", serialize(undefined))
  // on send/reply/forward/approve/create-api-key, leaving the header *present but
  // empty* when the caller passed no key. So "present" isn't enough to mean "caller-supplied" —
  // only a non-empty value counts; otherwise we mint (overwriting the empty stub).
  private ensureIdempotencyKey(request: RequestContext): void {
    const method = request.getHttpMethod();
    if (method !== HttpMethod.POST && method !== HttpMethod.DELETE) return;
    const headers = request.getHeaders();
    let present = false;
    for (const k of Object.keys(headers)) {
      if (k.toLowerCase() !== IDEMPOTENCY_HEADER.toLowerCase()) continue;
      present = true;
      const v = headers[k];
      if (v != null && String(v).trim() !== "") return; // genuine caller-supplied key wins
      break; // present-but-empty generated stub → fall through and mint
    }
    if (!present) return; // not a server-keyed op (no stub) — never seen here when gated
    request.setHeaderParam(IDEMPOTENCY_HEADER, (this.opts.genIdempotencyKey ?? defaultUuid)());
  }

  // Per-attempt timeout. Returns a signal that aborts on the caller's signal OR
  // after timeoutMs, a cleanup to clear the timer/listener, and a flag that
  // distinguishes a timeout from a caller abort (so the caller's abort still
  // propagates, while a timeout is retried). With no timeout the caller's signal
  // passes straight through.
  private attemptSignal(
    caller: AbortSignal | undefined,
    timeoutMs: number | undefined,
  ): { signal: AbortSignal | undefined; cleanup: () => void; timedOut: () => boolean } {
    if (!timeoutMs || timeoutMs <= 0) {
      return { signal: caller, cleanup: () => {}, timedOut: () => false };
    }
    const ctrl = new AbortController();
    let didTimeout = false;
    const onCallerAbort = () => ctrl.abort(caller?.reason);
    if (caller) {
      if (caller.aborted) ctrl.abort(caller.reason);
      else caller.addEventListener("abort", onCallerAbort, { once: true });
    }
    const timer = setTimeout(() => {
      didTimeout = true;
      ctrl.abort(new DOMException(`Request timed out after ${timeoutMs}ms`, "TimeoutError"));
    }, timeoutMs);
    return {
      signal: ctrl.signal,
      cleanup: () => {
        clearTimeout(timer);
        caller?.removeEventListener("abort", onCallerAbort);
      },
      timedOut: () => didTimeout,
    };
  }

  private throwIfAborted(signal: AbortSignal | undefined): void {
    if (signal?.aborted) throw abortError(signal.reason);
  }

  // Backoff: honor Retry-After (seconds or HTTP-date) up to maxRetryAfterMs;
  // otherwise exponential + jitter capped at maxDelayMs.
  private backoffMs(attempt: number, resp?: ResponseContext): number {
    const ra = resp?.headers ? this.retryAfterMs(resp.headers) : undefined;
    if (ra !== undefined) return Math.min(ra, this.opts.maxRetryAfterMs ?? 60000);

    const maxDelay = this.opts.maxDelayMs ?? 8000;
    const base = this.opts.baseDelayMs ?? 200;
    const exp = Math.min(base * 2 ** attempt, maxDelay);
    const rand = (this.opts.random ?? Math.random)();
    return Math.round(exp * (0.5 + 0.5 * rand)); // full jitter over [0.5, 1.0]
  }

  private retryAfterMs(headers: Record<string, string>): number | undefined {
    for (const k of Object.keys(headers)) {
      if (k.toLowerCase() !== "retry-after") continue;
      const v = headers[k];
      const secs = Number(v);
      if (Number.isFinite(secs) && secs >= 0) return secs * 1000;
      // RFC 9110 §10.2.3 allows an HTTP-date — common behind CDNs.
      const at = Date.parse(v);
      if (Number.isFinite(at)) return Math.max(0, at - (this.opts.now ?? Date.now)());
      return undefined;
    }
    return undefined;
  }

  private async sleep(ms: number, signal: AbortSignal | undefined): Promise<void> {
    // Injected sleep (tests) — still respect a pre-aborted signal.
    if (this.opts.sleep) {
      this.throwIfAborted(signal);
      return this.opts.sleep(ms);
    }
    if (!signal) return defaultSleep(ms);
    // Race the backoff against an abort so a cancelled request stops promptly.
    return new Promise<void>((resolve, reject) => {
      if (signal.aborted) {
        reject(abortError(signal.reason));
        return;
      }
      const t = setTimeout(() => {
        signal.removeEventListener("abort", onAbort);
        resolve();
      }, ms);
      const onAbort = () => {
        clearTimeout(t);
        reject(abortError(signal.reason));
      };
      signal.addEventListener("abort", onAbort, { once: true });
    });
  }
}
