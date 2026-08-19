import WebSocket from "ws";
import { EventEmitter } from "node:events";
import {
  E2AError,
  E2AAuthError,
  E2AConnectionReplacedError,
  E2APermissionError,
  E2ANotFoundError,
} from "./errors.js";
import type { WebhookEvent } from "./webhook-signature.js";
import { resolveBaseUrl, DEFAULT_BASE_URL } from "./env.js";

// Map a fatal (non-retryable) WebSocket handshake rejection status to a typed
// error — mirrors the Python SDK's _fatal_error_for_status (F6). A 4xx means the
// credential/request is wrong; reconnecting would loop forever, so the stream
// surfaces this and stops.
function fatalErrorForStatus(status: number): E2AError {
  const message = `WebSocket handshake rejected: HTTP ${status}`;
  if (status === 401) return new E2AAuthError({ code: "unauthorized", message, status, retryable: false });
  if (status === 403) return new E2APermissionError({ code: "forbidden", message, status, retryable: false });
  // 404 — the agent doesn't exist OR isn't yours (the server collapses the
  // cross-tenant case into not_found so the handshake can't enumerate agents).
  if (status === 404) return new E2ANotFoundError({ code: "not_found", message, status, retryable: false });
  return new E2AError({ code: "ws_handshake_rejected", message, status, retryable: false });
}

/**
 * e2a application close code: a NEWER connection for this agent superseded
 * this one (the server holds one connection per agent). Terminal — do not
 * reconnect. See docs/api.md "Connection lifecycle & close codes".
 */
export const WS_CLOSE_REPLACED = 4000;

// Map a server close CODE to a fatal (terminal, no-reconnect) typed error, per
// the documented close-code contract (docs/api.md; mirrors the Python SDK's
// _fatal_error_for_close):
//   4000 "replaced"     → E2AConnectionReplacedError — a newer connection for
//                         this agent took over; reconnecting would steal the
//                         socket back and loop.
//   1008                → E2APermissionError — genuine policy rejection;
//                         retrying the same connection cannot succeed.
//   4001–4999           → reserved e2a application codes; unknown ones are
//                         fatal by contract (forward-compat).
// Everything else (1001 shutting_down / ping_timeout, 1006 abnormal, 1011
// internal error, …) is transient → returns null → backoff reconnect.
function fatalErrorForClose(code: number, reason: string): E2AError | null {
  const suffix = `WebSocket closed by server: code=${code} reason="${reason}"`;
  if (code === WS_CLOSE_REPLACED) {
    return new E2AConnectionReplacedError({
      code: "ws_replaced",
      message: `a newer connection for this agent superseded this one; not reconnecting (one connection per agent) — ${suffix}`,
      status: 0,
      retryable: false,
    });
  }
  if (code === 1008) {
    return new E2APermissionError({
      code: "ws_policy_violation",
      message: `connection rejected by server policy; not reconnecting — ${suffix}`,
      status: 0,
      retryable: false,
    });
  }
  if (code >= 4000 && code <= 4999) {
    return new E2AError({
      code: "ws_closed",
      message: `terminal application close; not reconnecting — ${suffix}`,
      status: 0,
      retryable: false,
    });
  }
  return null;
}

function isEventEnvelope(value: unknown): value is WSEvent {
  if (!value || typeof value !== "object" || Array.isArray(value)) return false;
  const event = value as Record<string, unknown>;
  return typeof event.type === "string" &&
    typeof event.id === "string" &&
    typeof event.schema_version === "string" &&
    typeof event.created_at === "string" &&
    !!event.data && typeof event.data === "object" && !Array.isArray(event.data);
}

/**
 * A WebSocket frame from the e2a relay: the SAME versioned event envelope a
 * webhook delivery carries — `{type, id, schema_version, created_at, data}`.
 * Today the relay emits `email.received` events (data: {@link EmailReceivedData});
 * tolerate unknown `type` values — future WS event kinds parse into the same
 * envelope. Narrow with {@link isEmailReceived} (or `event.type === "email.received"`).
 *
 * The body is intentionally not included — fetch it via REST when (and only
 * when) you actually need it. `client.webhooks.fetchMessage(event)` is the
 * bridge (the WS envelope is a WebhookEvent), or directly:
 *
 *     if (isEmailReceived(event)) {
 *       const email = await client.messages.get(event.data.delivered_to, event.data.message_id);
 *     }
 *
 * Mirror of the Python SDK's `WSEvent`.
 */
export type WSEvent = WebhookEvent;

export interface WSListenerOptions {
  /** API key, sent as the `Authorization: Bearer` handshake header. */
  apiKey: string;
  /** Agent email to listen for. */
  agentEmail: string;
  /** Base URL (http/https). Falls back to `E2A_API_URL`, then the deprecated
   *  `E2A_BASE_URL` — the same resolution {@link E2AClient} uses. Default
   *  `https://api.e2a.dev`; override for self-host. */
  baseUrl?: string;
  /**
   * Auto-reconnect on disconnect. Defaults to true.
   * Reconnect uses exponential backoff (1s → maxBackoffMs).
   *
   * Applies only to TRANSIENT closes (network drops, server restart/shutdown,
   * ping timeout, internal error). Terminal close codes — 4000 "replaced" (a
   * newer connection for this agent took over), 1008 (policy rejection), and
   * other 4xxx application codes — never reconnect: the stream stops with a
   * typed error ({@link E2AConnectionReplacedError} for 4000).
   */
  reconnect?: boolean;
  /** Initial reconnect delay in ms. Defaults to 1000 (1 second). */
  reconnectDelay?: number;
  /** Maximum reconnect delay in ms. Defaults to 30000 (30 seconds). */
  maxBackoffMs?: number;
}

export interface WSListenerEvents {
  event: [event: WSEvent];
  open: [];
  close: [code: number, reason: string];
  error: [error: Error];
}

/**
 * Notification-only WebSocket listener.
 *
 * Connects to `/v1/agents/{address}/ws` and emits `"event"` events, each a
 * versioned {@link WSEvent} envelope with lightweight metadata in `data`.
 * The protocol is server→client only — the client never sends application
 * frames.
 *
 * Auth: the API key is sent as the `Authorization: Bearer` handshake header, so
 * it never appears in the URL (no leak to access logs / proxy traces / Referer).
 *
 * For modern code, prefer {@link E2AClient.listen} which wraps this
 * class with an async-iteration-friendly API while still exposing the
 * EventEmitter interface for `error` / `open` / `close`.
 */
export class WSListener extends EventEmitter<WSListenerEvents> {
  private ws: WebSocket | null = null;
  private closed = false;
  private readonly url: string;
  private readonly shouldReconnect: boolean;
  private readonly initialDelayMs: number;
  private readonly maxBackoffMs: number;
  private currentBackoffMs: number;
  // Handle of a pending reconnect timer, so close() can cancel it. Without
  // this, a close() issued during the backoff window leaves the timer armed and
  // dial() opens a fresh socket after the caller thought the stream was closed.
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null;

  constructor(private readonly opts: WSListenerOptions) {
    super();
    const base = (opts.baseUrl ?? resolveBaseUrl() ?? DEFAULT_BASE_URL).replace(/\/+$/, "");
    const wsBase = base.replace(/^http/, "ws");
    this.url = `${wsBase}/v1/agents/${encodeURIComponent(opts.agentEmail)}/ws`;
    this.shouldReconnect = opts.reconnect ?? true;
    this.initialDelayMs = opts.reconnectDelay ?? 1000;
    this.maxBackoffMs = opts.maxBackoffMs ?? 30_000;
    this.currentBackoffMs = this.initialDelayMs;
  }

  /** Open the WebSocket connection. */
  connect(): void {
    this.closed = false;
    this.currentBackoffMs = this.initialDelayMs;
    this.dial();
  }

  /** Close the connection permanently (no reconnect). */
  close(): void {
    this.closed = true;
    // Cancel a pending reconnect so a close() during the backoff window can't
    // resurrect the connection after the caller is done with it.
    if (this.reconnectTimer !== null) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
    if (this.ws) {
      this.ws.close(1000, "client close");
      this.ws = null;
    }
  }

  private dial(): void {
    // A reconnect scheduled before close() may still fire; bail if we're closed
    // so we never open a socket the caller has already torn down.
    if (this.closed) return;
    // Auth rides in the handshake header, never the URL — keeps the long-lived
    // API key out of access logs / proxy traces. Node's `ws` supports handshake
    // headers (a browser WebSocket could not, which is why this SDK targets Node).
    const ws = new WebSocket(this.url, {
      headers: { Authorization: `Bearer ${this.opts.apiKey}` },
    });
    // A fatal (4xx) handshake rejection — captured here, acted on in `close`.
    // The credential/request is wrong, so reconnecting would loop forever (F6).
    let fatal: E2AError | null = null;
    ws.on("unexpected-response", (_req, res: { statusCode?: number }) => {
      const status = res.statusCode ?? 0;
      if (status >= 400 && status < 500) fatal = fatalErrorForStatus(status);
    });

    ws.on("open", () => {
      // Successful connection — reset backoff so the next disconnect
      // starts fresh from the initial delay rather than continuing to
      // grow unbounded across multiple reconnect cycles.
      this.currentBackoffMs = this.initialDelayMs;
      this.emit("open");
    });

    ws.on("message", (data: WebSocket.RawData) => {
      try {
        const parsed: unknown = JSON.parse(data.toString());
        // Every frame is the versioned event envelope; a frame without a
        // string `type` is not one. Unknown `type` VALUES are fine (future
        // WS event kinds) — consumers narrow on type.
        if (!isEventEnvelope(parsed)) {
          this.emit("error", new Error("WS frame is missing required event envelope fields"));
          return;
        }
        this.emit("event", parsed as WSEvent);
      } catch (err) {
        this.emit("error", err instanceof Error ? err : new Error(String(err)));
      }
    });

    ws.on("close", (code: number, reason: Buffer) => {
      const reasonStr = reason.toString();
      this.emit("close", code, reasonStr);
      this.ws = null;
      if (fatal) {
        // Fatal handshake (auth/4xx) — surface the typed error and STOP; a
        // reconnect would just loop on the same rejection (F6 parity with Python).
        this.closed = true;
        this.emit("error", fatal);
        return;
      }
      // Server-sent terminal close codes (4000 "replaced", 1008 policy, other
      // 4xxx) — surface the typed error and STOP. Only consulted when we did
      // not initiate the close ourselves.
      if (!this.closed) {
        // A peer's normal close is a clean terminal condition, not a network
        // failure. Reconnecting here would turn an intentional shutdown into
        // a surprising infinite listener loop.
        if (code === 1000) {
          this.closed = true;
          return;
        }
        const fatalClose = fatalErrorForClose(code, reasonStr);
        if (fatalClose) {
          this.closed = true;
          this.emit("error", fatalClose);
          return;
        }
      }
      if (!this.closed && this.shouldReconnect) {
        this.reconnectTimer = setTimeout(() => {
          this.reconnectTimer = null;
          this.dial();
        }, this.currentBackoffMs);
        // Double the delay for the next reconnect, capped. Same shape
        // as Python's listen() backoff: 1s, 2s, 4s, 8s, …, capped.
        this.currentBackoffMs = Math.min(
          this.currentBackoffMs * 2,
          this.maxBackoffMs,
        );
      }
    });

    ws.on("error", (err: Error) => {
      // Suppress the noisy transport error that rides alongside a fatal
      // handshake — the typed error is emitted from `close`. Surface others.
      if (!fatal) this.emit("error", err);
    });

    this.ws = ws;
  }
}

/**
 * Hard cap on notifications buffered by a {@link WSStream} while no iterator is
 * awaiting. Beyond this, the oldest un-yielded event is dropped (with a one-time
 * warning) so a stalled consumer can't exhaust memory. Notifications are
 * advisory — the underlying message stays fetchable via REST — so bounded loss
 * under sustained backpressure is preferable to an OOM.
 */
export const WS_MAX_BUFFERED_EVENTS = 1000;

/**
 * Hybrid AsyncIterable + EventEmitter returned by {@link E2AClient.listen}.
 *
 * Iterate for the happy path — each item is a {@link WSEvent} envelope:
 *
 *     for await (const event of client.listen("bot@acme.dev")) {
 *       if (!isEmailReceived(event)) continue; // tolerate future event kinds
 *       const email = await client.webhooks.fetchMessage(event);
 *     }
 *
 * Use `.on("error", …)` / `.on("close", …)` for connection-level
 * concerns. Call `.close()` to terminate iteration cleanly.
 */
export class WSStream extends EventEmitter<WSListenerEvents>
  implements AsyncIterable<WSEvent> {
  private readonly listener: WSListener;
  // Notifications waiting to be yielded when no iterator is currently awaiting.
  // Hard-capped at WS_MAX_BUFFERED_EVENTS — a stalled consumer drops the OLDEST
  // event (logging once) rather than ballooning memory.
  private readonly buffer: WSEvent[] = [];
  // One-time latch so the overflow warning fires once, not on every drop.
  private bufferOverflowed = false;
  // Pending iterator promises waiting for the next notification.
  private readonly waiters: Array<{
    resolve: (value: IteratorResult<WSEvent>) => void;
    reject: (err: Error) => void;
  }> = [];
  private closed = false;
  // A terminal typed error waiting to be observed by iteration. Set when a
  // fatal error arrives with no waiter in flight (the `for await` body is busy
  // between pulls); the next `next()` drains any buffered events, then rejects
  // with this — so the typed error is never silently dropped. Delivered once.
  private pendingError: E2AError | null = null;
  // Whether the underlying listener will auto-reconnect on a transient close.
  // When false, a transient disconnect is terminal for iteration (there is no
  // redial coming), so the stream must end rather than leave a pull hanging.
  private readonly reconnectEnabled: boolean;

  constructor(opts: WSListenerOptions) {
    super();
    this.reconnectEnabled = opts.reconnect ?? true;
    this.listener = new WSListener(opts);

    // Forward connection-level events to consumers who prefer the
    // EventEmitter interface (or want both).
    this.listener.on("open", () => this.emit("open"));
    this.listener.on("close", (code, reason) => {
      this.emit("close", code, reason);
      // A normal close is terminal regardless of the reconnect setting. A
      // transient close is also terminal when reconnect is disabled, because
      // the listener schedules no redial. End iteration cleanly in both cases
      // so a pending next() cannot hang forever. This is deferred to a microtask
      // because a fatal close emits its typed "error" synchronously right after
      // this "close"; letting that run first lets the error path
      // (finishWithError) win, and finish() then no-ops on the already-closed
      // stream. Mirrors the Python SDK's close behavior.
      if (code === 1000 || !this.reconnectEnabled) {
        queueMicrotask(() => this.finish());
      }
    });
    this.listener.on("error", (err) => {
      // Node's EventEmitter THROWS when "error" is emitted with no registered
      // "error" listener. The documented usage is `for await (const e of
      // stream)`, which registers async-iterator *waiters*, not an EventEmitter
      // listener — so emitting unconditionally would crash the whole process on
      // a routine transient disconnect. Only emit when someone is listening.
      if (this.listenerCount("error") > 0) this.emit("error", err);
      // Only a typed (fatal) error ends the stream — e.g. an auth/4xx handshake
      // rejection or a terminal server close code. The listener has stopped
      // reconnecting, so end iteration and surface the typed error rather than
      // hang (F6). If a waiter is in flight it rejects immediately; otherwise
      // the error is held in `pendingError` until the next pull observes it, so
      // a terminal close that lands while the for-await body is busy is never
      // silently swallowed.
      //
      // Transient errors (network blips, a single malformed frame) ride
      // alongside an automatic reconnect in WSListener — swallow them here so
      // the async iterator keeps waiting for the reconnected stream, matching
      // the Python SDK, which logs-and-reconnects and never surfaces transient
      // failures to `async for`. (When reconnect is disabled, the "close"
      // handler above ends iteration cleanly instead.)
      if (err instanceof E2AError) {
        this.finishWithError(err);
      }
    });

    this.listener.on("event", (notif) => {
      this.emit("event", notif);
      this.deliver(notif);
    });

    this.listener.connect();
  }

  /** Close the connection and end iteration. */
  close(): void {
    this.closed = true;
    // Explicit close is a clean stop — drop any pending terminal error so the
    // loop exits with done=true rather than throwing after the caller asked out.
    this.pendingError = null;
    this.listener.close();
    // Resolve any in-flight awaits with done=true so the loop exits.
    while (this.waiters.length > 0) {
      this.waiters.shift()!.resolve({ value: undefined, done: true });
    }
  }

  [Symbol.asyncIterator](): AsyncIterator<WSEvent> {
    return {
      next: (): Promise<IteratorResult<WSEvent>> => {
        // Buffered events drain first — even after a terminal error — so events
        // that arrived before the close are delivered in order before the error.
        if (this.buffer.length > 0) {
          return Promise.resolve({ value: this.buffer.shift()!, done: false });
        }
        // A terminal error that landed with no waiter in flight surfaces here,
        // exactly once, then the stream reads as a clean terminal done.
        if (this.pendingError) {
          const err = this.pendingError;
          this.pendingError = null;
          return Promise.reject(err);
        }
        if (this.closed) {
          return Promise.resolve({ value: undefined, done: true });
        }
        return new Promise((resolve, reject) => {
          this.waiters.push({ resolve, reject });
        });
      },
      return: (): Promise<IteratorResult<WSEvent>> => {
        this.close();
        return Promise.resolve({ value: undefined, done: true });
      },
    };
  }

  private deliver(notif: WSEvent): void {
    if (this.waiters.length > 0) {
      this.waiters.shift()!.resolve({ value: notif, done: false });
      return;
    }
    this.buffer.push(notif);
    if (this.buffer.length > WS_MAX_BUFFERED_EVENTS) {
      // A consumer that stalls (e.g. awaits a slow fetch inside the loop) must
      // not grow this without bound. Drop the oldest un-yielded event — the WS
      // frame is advisory (the message stays fetchable via REST / reconcilable
      // via list) — and warn once so the backpressure is loud, not silent.
      this.buffer.shift();
      if (!this.bufferOverflowed) {
        this.bufferOverflowed = true;
        console.warn(
          `[e2a] WSStream dropped events: more than ${WS_MAX_BUFFERED_EVENTS} un-consumed notifications buffered. The consumer is not keeping up — do less work inside the for-await loop or fetch out of band.`,
        );
      }
    }
  }

  // End iteration cleanly (done=true). Idempotent — no-ops once the stream has
  // already terminated (e.g. a fatal "error" beat this "close"-driven finish).
  private finish(): void {
    if (this.closed) return;
    this.closed = true;
    while (this.waiters.length > 0) {
      this.waiters.shift()!.resolve({ value: undefined, done: true });
    }
  }

  // End iteration with a terminal typed error. A waiter in flight rejects
  // immediately; with none, the error is parked in `pendingError` for the next
  // pull to observe (buffered events drain first — see next()). Idempotent.
  private finishWithError(err: E2AError): void {
    if (this.closed) return;
    this.closed = true;
    this.pendingError = err;
    if (this.waiters.length > 0) {
      // A waiter exists ⇒ the buffer is empty (deliver()/next() invariant), so
      // reject now and mark the error observed.
      this.pendingError = null;
      while (this.waiters.length > 0) {
        this.waiters.shift()!.reject(err);
      }
    }
  }
}
