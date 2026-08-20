import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import type { WSEvent } from "../../src/v1/ws.js";
import { isEmailReceived } from "../../src/v1/webhook-signature.js";

interface CloseContractCase {
  code: number;
  reason: string;
  classification: "normal" | "transient" | "terminal" | "replaced";
}

const closeContract = JSON.parse(readFileSync(
  join(__dirname, "../../../../internal/ws/testdata/close-contract.json"),
  "utf8",
)) as CloseContractCase[];

// These are pure unit tests for the WS surface. Network-level tests
// would require spinning up a fake WS server, which isn't worth it for
// this PR — the type fix and exponential backoff are easy to verify by
// reading the code, and the iteration semantics are tested in
// client.test.ts where we can mock the WSListener.

describe("WSEvent envelope", () => {
  it("is the versioned event envelope — the same shape as a webhook delivery", () => {
    // Parse the shared golden fixture: the WS channel emits the SAME
    // envelope+payload the webhook channel delivers (the server's ws tests
    // assert frame parity against this very file).
    const raw = readFileSync(
      join(__dirname, "../../../../internal/eventpayload/testdata/email.received.json"),
      "utf8",
    );
    const event: WSEvent = JSON.parse(raw);
    expect(event.type).toBe("email.received");
    expect(event.schema_version).toBe("1");
    expect(event.id).toMatch(/^evt_/);
    if (!isEmailReceived(event)) throw new Error("guard should narrow email.received");
    expect(event.data.message_id).toMatch(/^msg_/);
    expect(event.data.delivered_to).toBe("support@agents.example.com");
    expect(event.data.direction).toBe("inbound");
    expect(event.data.lifecycle_transitions?.[0].reason_code).toBe("acceptance.inbound_smtp");
    expect(event.data.lifecycle_transitions?.[0].evidence).toEqual({});
    expect(event.data.lifecycle_transitions?.[0].reconstructed).toBe(false);
  });

  it("tolerates unknown event types (forward-compat)", () => {
    const event: WSEvent = {
      type: "email.some_future_kind",
      id: "evt_x",
      schema_version: "1",
      created_at: "2026-07-01T10:30:00Z",
      data: { anything: true },
    };
    expect(isEmailReceived(event)).toBe(false);
    expect(event.type).toBe("email.some_future_kind");
  });

  it("preserves nullable and reconstructed lifecycle wire evidence", () => {
    const event: WSEvent = JSON.parse(JSON.stringify({
      type: "email.received",
      id: "evt_reconstructed",
      schema_version: "1",
      created_at: "2026-07-22T00:00:00Z",
      data: {
        lifecycle_transitions: [{
          id: "mlt_recon_1",
          message_id: "msg_1",
          direction: "inbound",
          recipient: null,
          stage: "accepted",
          outcome: "accepted",
          reason_code: "acceptance.inbound_smtp",
          retryable: false,
          evidence: { source: "message", future: { nested: true } },
          correlation_ids: { future_id: "future_1" },
          occurred_at: "2026-07-22T00:00:00Z",
          reconstructed: true,
        }],
      },
    }));
    if (!isEmailReceived(event)) throw new Error("guard should narrow email.received");
    const transition = event.data.lifecycle_transitions?.[0];
    expect(transition?.recipient).toBeNull();
    expect(transition?.evidence.future).toEqual({ nested: true });
    expect(transition?.correlation_ids.future_id).toBe("future_1");
    expect(transition?.reconstructed).toBe(true);
  });
});

describe("WSListener exponential backoff", () => {
  it("reads maxBackoffMs option", async () => {
    // Smoke test that the option threading works without actually
    // dialing a WebSocket. We import lazily so the `ws` package isn't
    // required to load this file.
    const { WSListener } = await import("../../src/v1/ws.js");
    const l = new WSListener({
      apiKey: "k",
      agentEmail: "bot@agents.e2a.dev",
      reconnect: false, // don't actually loop
      reconnectDelay: 100,
      maxBackoffMs: 5000,
    });
    // Defaults are correct (no exception, fields not throwing on
    // construction).
    expect(l).toBeDefined();
    l.close();
  });
});

describe("WSListener base URL resolution", () => {
  // Same contract as E2AClient (client.test.ts's "base URL resolution"
  // suite): opts.baseUrl > E2A_API_URL > E2A_BASE_URL (deprecated) > the
  // api.e2a.dev default. WSListener used to bypass this entirely with its
  // own hardcoded "https://api.e2a.dev" fallback, so a self-hoster who
  // exported E2A_API_URL and constructed WSListener directly (rather than
  // going through E2AClient.listen) would have their API key sent to the
  // operator's host in the WS handshake Authorization header regardless.
  let savedEnv: Record<string, string | undefined>;

  beforeEach(() => {
    savedEnv = {
      E2A_API_URL: process.env.E2A_API_URL,
      E2A_BASE_URL: process.env.E2A_BASE_URL,
    };
    delete process.env.E2A_API_URL;
    delete process.env.E2A_BASE_URL;
  });

  afterEach(() => {
    for (const [k, v] of Object.entries(savedEnv)) {
      if (v === undefined) delete process.env[k];
      else process.env[k] = v;
    }
  });

  async function dialedHost(opts: { baseUrl?: string } = {}): Promise<string> {
    const calls: Array<{ url: string }> = [];
    vi.resetModules();
    vi.doMock("ws", () => {
      class FakeWS {
        constructor(url: string) {
          calls.push({ url });
        }
        on() {
          return this;
        }
        close() {}
      }
      return { default: FakeWS };
    });
    const { WSListener } = await import("../../src/v1/ws.js");
    const l = new WSListener({ apiKey: "k", agentEmail: "bot@x.dev", reconnect: false, ...opts });
    l.connect();
    l.close();
    vi.doUnmock("ws");
    vi.resetModules();
    expect(calls).toHaveLength(1);
    return new URL(calls[0].url.replace(/^ws/, "http")).origin;
  }

  it("defaults to the API host, same as E2AClient", async () => {
    expect(await dialedHost()).toBe("https://api.e2a.dev");
  });

  it("reads E2A_API_URL", async () => {
    process.env.E2A_API_URL = "https://api.self-host.example";
    expect(await dialedHost()).toBe("https://api.self-host.example");
  });

  it("still honours the deprecated E2A_BASE_URL, with a warning", async () => {
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    process.env.E2A_BASE_URL = "https://legacy.example";
    expect(await dialedHost()).toBe("https://legacy.example");
    expect(warn).toHaveBeenCalledWith(expect.stringContaining("E2A_BASE_URL is deprecated"));
    warn.mockRestore();
  });

  it("prefers E2A_API_URL over the deprecated name", async () => {
    process.env.E2A_API_URL = "https://canonical.example";
    process.env.E2A_BASE_URL = "https://legacy.example";
    expect(await dialedHost()).toBe("https://canonical.example");
  });

  it("lets an explicit baseUrl beat the environment", async () => {
    process.env.E2A_API_URL = "https://api.self-host.example";
    expect(await dialedHost({ baseUrl: "https://explicit.example" })).toBe("https://explicit.example");
  });
});

describe("WSListener auth", () => {
  it("sends the API key as an Authorization: Bearer handshake header, not in the URL", async () => {
    const calls: Array<{ url: string; opts: unknown }> = [];
    vi.resetModules();
    vi.doMock("ws", () => {
      class FakeWS {
        constructor(url: string, opts: unknown) {
          calls.push({ url, opts });
        }
        on() {
          return this;
        }
        close() {}
      }
      return { default: FakeWS };
    });

    const { WSListener } = await import("../../src/v1/ws.js");
    const l = new WSListener({ apiKey: "secret_key", agentEmail: "bot@x.dev", reconnect: false });
    l.connect();
    l.close();
    vi.doUnmock("ws");
    vi.resetModules();

    expect(calls).toHaveLength(1);
    // Credential is in the header, never the URL.
    expect(calls[0].url).not.toContain("token=");
    expect(calls[0].url).not.toContain("secret_key");
    expect(calls[0].opts).toEqual({ headers: { Authorization: "Bearer secret_key" } });
  });
});

describe("WSListener envelope validation", () => {
  it("rejects frames missing required core envelope fields", async () => {
    class FakeWS {
      handlers = new Map<string, (...args: unknown[]) => void>();
      constructor(_url: string, _opts: unknown) {}
      on(event: string, fn: (...args: unknown[]) => void) { this.handlers.set(event, fn); return this; }
      close() {}
    }
    const socket = new FakeWS("", {});
    vi.resetModules();
    vi.doMock("ws", () => ({ default: class extends FakeWS {
      constructor(url: string, opts: unknown) { super(url, opts); Object.assign(socket, this); }
    } }));
    const { WSListener } = await import("../../src/v1/ws.js");
    const listener = new WSListener({ apiKey: "k", agentEmail: "bot@x.dev", reconnect: false });
    const errors: Error[] = [];
    listener.on("error", (error) => errors.push(error));
    listener.connect();
    socket.handlers.get("message")?.(Buffer.from(JSON.stringify({ type: "email.received", data: {} })));
    expect(errors[0]?.message).toMatch(/required event envelope fields/);
    vi.doUnmock("ws");
    vi.resetModules();
  });
});

describe("WSListener close-code contract (docs/api.md)", () => {
  // Drives the reconnect matrix by mocking the `ws` module: each fake socket
  // records its event handlers so the test can fire a server-initiated close
  // with a specific code/reason and observe whether the listener redials
  // (transient) or stops with a typed error (terminal).
  type Handler = (...args: unknown[]) => void;
  class FakeWS {
    static instances: FakeWS[] = [];
    handlers = new Map<string, Handler>();
    constructor(
      public url: string,
      public opts: unknown,
    ) {
      FakeWS.instances.push(this);
    }
    on(event: string, fn: Handler) {
      this.handlers.set(event, fn);
      return this;
    }
    close() {}
    serverClose(code: number, reason: string) {
      this.handlers.get("close")?.(code, Buffer.from(reason));
    }
  }

  async function listenWith(closeCode: number, reason: string) {
    FakeWS.instances = [];
    vi.resetModules();
    vi.doMock("ws", () => ({ default: FakeWS }));
    vi.useFakeTimers();
    const { WSListener } = await import("../../src/v1/ws.js");
    const errors = await import("../../src/v1/errors.js");
    const l = new WSListener({
      apiKey: "k",
      agentEmail: "bot@x.dev",
      reconnect: true,
      reconnectDelay: 10,
    });
    const seen: Error[] = [];
    const closes: Array<{ code: number; reason: string }> = [];
    l.on("error", (e) => seen.push(e));
    l.on("close", (code, r) => closes.push({ code, reason: r }));
    l.connect();
    expect(FakeWS.instances).toHaveLength(1);
    FakeWS.instances[0].serverClose(closeCode, reason);
    // Give any scheduled redial a chance to fire.
    await vi.advanceTimersByTimeAsync(60_000);
    vi.useRealTimers();
    vi.doUnmock("ws");
    vi.resetModules();
    return { dials: FakeWS.instances.length, errors: seen, closes, errorsMod: errors, listener: l };
  }

  it.each(closeContract)("classifies $code/$reason as $classification", async (tc) => {
    const { dials, errors } = await listenWith(tc.code, tc.reason);
    if (tc.classification === "transient") {
      expect(dials).toBeGreaterThan(1);
      expect(errors).toHaveLength(0);
      return;
    }
    expect(dials).toBe(1);
    if (tc.classification === "normal") {
      expect(errors).toHaveLength(0);
      return;
    }
    expect(errors).toHaveLength(1);
    expect((errors[0] as { code?: string }).code).toBe(
      tc.classification === "replaced" ? "ws_replaced" :
        tc.code === 1008 ? "ws_policy_violation" : "ws_closed",
    );
  });

  it("4000 'replaced' → E2AConnectionReplacedError, no reconnect", async () => {
    const { dials, errors, closes, errorsMod } = await listenWith(4000, "replaced");
    expect(dials).toBe(1); // never redialed
    expect(errors).toHaveLength(1);
    expect(errors[0]).toBeInstanceOf(errorsMod.E2AConnectionReplacedError);
    expect((errors[0] as InstanceType<typeof errorsMod.E2AError>).code).toBe("ws_replaced");
    expect((errors[0] as InstanceType<typeof errorsMod.E2AError>).retryable).toBe(false);
    expect(closes).toEqual([{ code: 4000, reason: "replaced" }]);
  });

  it("1008 policy rejection → E2APermissionError, no reconnect", async () => {
    const { dials, errors, errorsMod } = await listenWith(1008, "policy violation");
    expect(dials).toBe(1);
    expect(errors).toHaveLength(1);
    expect(errors[0]).toBeInstanceOf(errorsMod.E2APermissionError);
    expect((errors[0] as InstanceType<typeof errorsMod.E2AError>).code).toBe("ws_policy_violation");
  });

  it("unknown 4xxx application code → fatal E2AError, no reconnect (forward-compat)", async () => {
    const { dials, errors, errorsMod } = await listenWith(4321, "future_condition");
    expect(dials).toBe(1);
    expect(errors).toHaveLength(1);
    expect(errors[0]).toBeInstanceOf(errorsMod.E2AError);
    expect((errors[0] as InstanceType<typeof errorsMod.E2AError>).code).toBe("ws_closed");
    expect((errors[0] as InstanceType<typeof errorsMod.E2AError>).retryable).toBe(false);
  });

  it("1001 'shutting_down' (server restart) → reconnects with backoff", async () => {
    const { dials, errors } = await listenWith(1001, "shutting_down");
    expect(dials).toBeGreaterThan(1); // redialed
    expect(errors).toHaveLength(0);
  });

  it("1006 abnormal close (network drop) → reconnects", async () => {
    const { dials, errors } = await listenWith(1006, "");
    expect(dials).toBeGreaterThan(1);
    expect(errors).toHaveLength(0);
  });

  it("1011 internal server error → reconnects", async () => {
    const { dials, errors } = await listenWith(1011, "");
    expect(dials).toBeGreaterThan(1);
    expect(errors).toHaveLength(0);
  });

  it("client-initiated close → no reconnect and no error, even if the server echoes a code", async () => {
    FakeWS.instances = [];
    vi.resetModules();
    vi.doMock("ws", () => ({ default: FakeWS }));
    const { WSListener } = await import("../../src/v1/ws.js");
    const l = new WSListener({ apiKey: "k", agentEmail: "bot@x.dev", reconnect: true });
    const seen: Error[] = [];
    l.on("error", (e) => seen.push(e));
    l.connect();
    l.close(); // client closes first
    FakeWS.instances[0].serverClose(1000, "");
    vi.doUnmock("ws");
    vi.resetModules();
    expect(FakeWS.instances).toHaveLength(1);
    expect(seen).toHaveLength(0);
  });

  it("close() during the reconnect backoff cancels the pending redial (no zombie connection)", async () => {
    FakeWS.instances = [];
    vi.resetModules();
    vi.doMock("ws", () => ({ default: FakeWS }));
    vi.useFakeTimers();
    const { WSListener } = await import("../../src/v1/ws.js");
    const listener = new WSListener({
      apiKey: "k",
      agentEmail: "bot@x.dev",
      reconnect: true,
      reconnectDelay: 1000,
    });
    listener.connect();
    expect(FakeWS.instances).toHaveLength(1);
    FakeWS.instances[0].serverClose(1006, "");
    listener.close();
    await vi.advanceTimersByTimeAsync(60_000);
    vi.useRealTimers();
    vi.doUnmock("ws");
    vi.resetModules();
    expect(FakeWS.instances).toHaveLength(1);
  });
});

describe("E2AClient.listen()", () => {
  it("requires an email at point of use", async () => {
    const { E2AClient } = await import("../../src/v1/client.js");
    const c = new E2AClient({ apiKey: "k" });
    expect(() => c.listen("")).toThrow(/email is required/);
  });
});

describe("WSStream error handling (async-iterator consumers)", () => {
  // The documented usage is `for await (const e of client.listen(addr))` with
  // NO `.on("error")` listener. Node's EventEmitter throws if "error" is emitted
  // with no registered listener, so the stream must not emit unconditionally,
  // and transient failures must not end iteration (the socket reconnects).
  type Handler = (...args: unknown[]) => void;
  class FakeWS {
    static instances: FakeWS[] = [];
    handlers = new Map<string, Handler>();
    constructor(
      public url: string,
      public opts: unknown,
    ) {
      FakeWS.instances.push(this);
    }
    on(event: string, fn: Handler) {
      this.handlers.set(event, fn);
      return this;
    }
    close() {}
    serverOpen() {
      this.handlers.get("open")?.();
    }
    serverError(err: Error) {
      this.handlers.get("error")?.(err);
    }
    serverMessage(obj: unknown) {
      this.handlers.get("message")?.(Buffer.from(JSON.stringify(obj)));
    }
    serverClose(code: number, reason: string) {
      this.handlers.get("close")?.(code, Buffer.from(reason));
    }
  }

  async function makeStream(reconnectDelay = 10, reconnect = true) {
    FakeWS.instances = [];
    vi.resetModules();
    vi.doMock("ws", () => ({ default: FakeWS }));
    const ws = await import("../../src/v1/ws.js");
    const errors = await import("../../src/v1/errors.js");
    const stream = new ws.WSStream({
      apiKey: "k",
      agentEmail: "bot@x.dev",
      reconnect,
      reconnectDelay,
    });
    return { stream, errors };
  }

  it("does not crash on a transient transport error under for-await (no error listener) and keeps streaming after reconnect", async () => {
    vi.useFakeTimers();
    const { stream } = await makeStream(10);

    // Iterate, register NO EventEmitter "error" listener (the documented path).
    const iterator = stream[Symbol.asyncIterator]();
    const nextP = iterator.next(); // registers a waiter
    const first = FakeWS.instances[0];
    first.serverOpen();

    // Pre-fix: emit("error") with no listener throws out of the `ws` callback and
    // crashes the process. Post-fix: a transient error is swallowed — no throw.
    expect(() => first.serverError(new Error("ECONNREFUSED"))).not.toThrow();

    // ...and a transient error must NOT end iteration; the socket reconnects.
    first.serverClose(1006, ""); // abnormal close → schedule redial
    await vi.advanceTimersByTimeAsync(50);
    expect(FakeWS.instances.length).toBeGreaterThan(1); // redialed

    // The reconnected socket delivers an event → the pending waiter resolves.
    const latest = FakeWS.instances[FakeWS.instances.length - 1];
    latest.serverMessage({
      type: "email.received",
      id: "evt_1",
      schema_version: "1",
      created_at: "2026-07-01T10:30:00Z",
      data: {},
    });
    const res = await nextP;
    expect(res.done).toBe(false);
    expect((res.value as { id: string }).id).toBe("evt_1");

    stream.close();
    vi.useRealTimers();
    vi.doUnmock("ws");
    vi.resetModules();
  });

  it("rejects the for-await iterator with the typed error on a fatal close (4000 replaced) and does not reconnect", async () => {
    const { stream, errors } = await makeStream();
    const iterator = stream[Symbol.asyncIterator]();
    const nextP = iterator.next();

    // Fatal terminal close with no `.on("error")` listener registered. Pre-fix
    // this crashed the process; post-fix the typed error surfaces to `for await`.
    FakeWS.instances[0].serverClose(4000, "replaced");

    await expect(nextP).rejects.toBeInstanceOf(errors.E2AConnectionReplacedError);
    expect(FakeWS.instances).toHaveLength(1); // never redialed

    vi.doUnmock("ws");
    vi.resetModules();
  });

  it("preserves a fatal error that arrives with no waiter in flight until the next() observes it (P1)", async () => {
    const { stream, errors } = await makeStream();
    const iterator = stream[Symbol.asyncIterator]();

    // Simulate the real `for await` window: the loop body is processing an
    // event, so there is NO pending waiter at the instant the terminal close
    // lands. Pre-fix, this set `closed` and the typed error was silently lost —
    // the next `next()` returned a clean `{ done: true }`.
    FakeWS.instances[0].serverOpen();
    FakeWS.instances[0].serverClose(4000, "replaced");

    // The next iteration must still surface the typed error, not done:true.
    await expect(iterator.next()).rejects.toBeInstanceOf(errors.E2AConnectionReplacedError);
    // Delivered exactly once — a subsequent pull is a clean terminal done.
    await expect(iterator.next()).resolves.toEqual({ value: undefined, done: true });
    expect(FakeWS.instances).toHaveLength(1); // never redialed

    vi.doUnmock("ws");
    vi.resetModules();
  });

  it("drains buffered events before surfacing a fatal error that arrived with no waiter (P1 ordering)", async () => {
    const { stream, errors } = await makeStream();
    const iterator = stream[Symbol.asyncIterator]();

    FakeWS.instances[0].serverOpen();
    // An event is buffered (no waiter), then the terminal close lands.
    FakeWS.instances[0].serverMessage({
      type: "email.received",
      id: "evt_buffered",
      schema_version: "1",
      created_at: "2026-07-01T10:30:00Z",
      data: {},
    });
    FakeWS.instances[0].serverClose(4000, "replaced");

    // Buffered event drains first...
    const first = await iterator.next();
    expect(first.done).toBe(false);
    expect((first.value as { id: string }).id).toBe("evt_buffered");
    // ...then the typed error surfaces.
    await expect(iterator.next()).rejects.toBeInstanceOf(errors.E2AConnectionReplacedError);

    vi.doUnmock("ws");
    vi.resetModules();
  });

  it("ends iteration on a transient disconnect when reconnect is disabled instead of hanging (P2)", async () => {
    const { stream } = await makeStream(10, /* reconnect */ false);
    const iterator = stream[Symbol.asyncIterator]();
    const nextP = iterator.next(); // registers a waiter

    FakeWS.instances[0].serverOpen();
    // Transient close (network drop). With reconnect disabled the listener
    // schedules no redial; pre-fix the stream was never marked closed and this
    // waiter hung forever. Post-fix iteration ends cleanly (matches Python's
    // reconnect=False, which returns from iteration after the first disconnect).
    FakeWS.instances[0].serverClose(1006, "");

    await expect(nextP).resolves.toEqual({ value: undefined, done: true });
    expect(FakeWS.instances).toHaveLength(1); // never redialed

    vi.doUnmock("ws");
    vi.resetModules();
  });

  it("ends iteration cleanly on a normal peer close when reconnect is enabled", async () => {
    const { stream } = await makeStream(10, /* reconnect */ true);
    const iterator = stream[Symbol.asyncIterator]();
    const nextP = iterator.next();
    let result: IteratorResult<unknown> | undefined;
    void nextP.then((value) => {
      result = value;
    });

    FakeWS.instances[0].serverOpen();
    FakeWS.instances[0].serverClose(1000, "");
    await new Promise<void>((resolve) => queueMicrotask(resolve));

    expect(result).toEqual({ value: undefined, done: true });
    expect(FakeWS.instances).toHaveLength(1); // normal close never redials

    stream.close();
    vi.doUnmock("ws");
    vi.resetModules();
  });

  it("caps the un-consumed buffer, dropping the oldest events and warning once", async () => {
    const { stream } = await makeStream(10, /* reconnect */ false);
    const { WS_MAX_BUFFERED_EVENTS } = await import("../../src/v1/ws.js");
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});

    // Deliver more than the cap with no iterator awaiting, so every event is
    // initially queued in the stream's buffer.
    const overflow = 5;
    const total = WS_MAX_BUFFERED_EVENTS + overflow;
    for (let i = 0; i < total; i++) {
      FakeWS.instances[0].serverMessage({
        type: "email.received",
        id: `evt_${i}`,
        schema_version: "1",
        created_at: "2026-07-01T10:30:00Z",
        data: {},
      });
    }

    const iterator = stream[Symbol.asyncIterator]();
    const ids: string[] = [];
    for (let i = 0; i < WS_MAX_BUFFERED_EVENTS; i++) {
      const result = await iterator.next();
      expect(result.done).toBe(false);
      ids.push((result.value as { id: string }).id);
    }
    expect(ids).toHaveLength(WS_MAX_BUFFERED_EVENTS);
    expect(ids[0]).toBe(`evt_${overflow}`);
    expect(ids[ids.length - 1]).toBe(`evt_${total - 1}`);
    expect(warn).toHaveBeenCalledOnce();

    stream.close();
    warn.mockRestore();
    vi.doUnmock("ws");
    vi.resetModules();
  });
});
