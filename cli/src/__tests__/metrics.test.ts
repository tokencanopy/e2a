import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";

const mockGetMetrics = vi.fn();
const mockAccountMetrics = vi.fn();

vi.mock("../sdk.js", () => ({
  createClient: vi.fn(() => ({
    messages: { getMetrics: mockGetMetrics },
    account: { metrics: mockAccountMetrics },
  })),
  requireAgentEmail: vi.fn(() => "bot@agents.localhost"),
}));

import { metrics } from "../commands/metrics.js";

function makeSummary(over: Record<string, number> = {}) {
  return {
    accepted: 0, submitted: 0, delivered: 0,
    bouncedHard: 0, bouncedSoft: 0, bouncedUndetermined: 0,
    complained: 0, suppressed: 0, sendFailed: 0,
    received: 0, dmarcPass: 0, dmarcFail: 0, dmarcNone: 0, dmarcError: 0,
    reviewHeld: 0, reviewApproved: 0, reviewRejected: 0,
    reviewExpiredApproved: 0, reviewExpiredRejected: 0,
    ...over,
  };
}

function makeRates(over: Record<string, number | null> = {}) {
  return {
    deliveredRate: null, bounceRate: null, complaintRate: null, suppressionBlockRate: null,
    ...over,
  };
}

function makeAgentView(over: Record<string, unknown> = {}) {
  return {
    agentEmail: "bot@agents.localhost",
    start: new Date("2026-07-09T00:00:00Z"),
    end: new Date("2026-08-08T00:00:00Z"),
    messagesInWindow: 10,
    messagesWithLifecycle: 10,
    reconstructedObservations: 0,
    summary: makeSummary({ accepted: 10, submitted: 8, delivered: 6 }),
    rates: makeRates({ deliveredRate: 0.6, bounceRate: 0.25 }),
    counters: [],
    ...over,
  };
}

function makeAccountView(over: Record<string, unknown> = {}) {
  return {
    start: new Date("2026-07-09T00:00:00Z"),
    end: new Date("2026-08-08T00:00:00Z"),
    messagesInWindow: 10,
    messagesWithLifecycle: 10,
    reconstructedObservations: 0,
    summary: makeSummary({ accepted: 10, delivered: 6 }),
    rates: makeRates({ deliveredRate: 0.6 }),
    counters: [],
    agents: [],
    agentsTruncated: false,
    ...over,
  };
}

describe("metrics command", () => {
  let mockStdout: ReturnType<typeof vi.spyOn>;

  beforeEach(() => {
    mockStdout = vi.spyOn(process.stdout, "write").mockImplementation(() => true);
    vi.spyOn(process.stderr, "write").mockImplementation(() => true);
    vi.spyOn(process, "exit").mockImplementation(() => {
      throw new Error("process.exit");
    });
  });

  afterEach(() => {
    vi.restoreAllMocks();
    vi.clearAllMocks();
  });

  const out = () => mockStdout.mock.calls.map((c: unknown[]) => String(c[0])).join("");

  it("reads one inbox when given an agent address", async () => {
    mockGetMetrics.mockResolvedValue(makeAgentView());
    await metrics("bot@agents.localhost", {});
    expect(mockGetMetrics).toHaveBeenCalledWith("bot@agents.localhost", {
      start: undefined,
      end: undefined,
    });
    expect(mockAccountMetrics).not.toHaveBeenCalled();
    expect(out()).toContain("delivered 60.0%");
  });

  it("reads the account rollup when no address is given", async () => {
    mockAccountMetrics.mockResolvedValue(makeAccountView());
    await metrics(undefined, {});
    expect(mockAccountMetrics).toHaveBeenCalledWith({
      start: undefined,
      end: undefined,
      groupBy: undefined,
    });
    expect(mockGetMetrics).not.toHaveBeenCalled();
    expect(out()).toContain("account totals");
  });

  // A null rate means the denominator was zero. Rendering it as "0.0%" would
  // read as total delivery failure, which is the opposite of "nothing sent".
  it("prints n/a, never 0%, for a rate with no denominator", async () => {
    mockGetMetrics.mockResolvedValue(
      makeAgentView({ summary: makeSummary(), rates: makeRates(), messagesInWindow: 0, messagesWithLifecycle: 0 }),
    );
    await metrics("bot@agents.localhost", {});
    expect(out()).toContain("delivered n/a");
    expect(out()).not.toContain("0.0%");
  });

  it("distinguishes a real zero rate from a missing one", async () => {
    mockGetMetrics.mockResolvedValue(
      makeAgentView({ rates: makeRates({ deliveredRate: 0, bounceRate: null }) }),
    );
    await metrics("bot@agents.localhost", {});
    expect(out()).toContain("delivered 0.0%");
    expect(out()).toContain("bounced n/a");
  });

  it("passes --by-agent through and lists the breakdown busiest first", async () => {
    mockAccountMetrics.mockResolvedValue(
      makeAccountView({
        agents: [
          {
            agentEmail: "busy@agents.localhost",
            messagesInWindow: 8,
            messagesWithLifecycle: 8,
            reconstructedObservations: 0,
            summary: makeSummary({ accepted: 8, delivered: 8 }),
            rates: makeRates({ deliveredRate: 1 }),
            counters: [],
          },
          {
            agentEmail: "quiet@agents.localhost",
            messagesInWindow: 2,
            messagesWithLifecycle: 2,
            reconstructedObservations: 0,
            summary: makeSummary({ accepted: 2 }),
            rates: makeRates({ deliveredRate: 0 }),
            counters: [],
          },
        ],
      }),
    );
    await metrics(undefined, { byAgent: true });
    expect(mockAccountMetrics).toHaveBeenCalledWith({
      start: undefined,
      end: undefined,
      groupBy: "agent",
    });
    const text = out();
    expect(text).toContain("by agent");
    expect(text.indexOf("busy@agents.localhost")).toBeLessThan(text.indexOf("quiet@agents.localhost"));
  });

  it("rejects --by-agent alongside an inbox instead of ignoring it", async () => {
    await expect(metrics("bot@agents.localhost", { byAgent: true })).rejects.toThrow("process.exit");
    expect(mockGetMetrics).not.toHaveBeenCalled();
    expect(mockAccountMetrics).not.toHaveBeenCalled();
  });

  it("warns when the ledger does not cover every message in the window", async () => {
    mockGetMetrics.mockResolvedValue(
      makeAgentView({ messagesInWindow: 100, messagesWithLifecycle: 40 }),
    );
    await metrics("bot@agents.localhost", {});
    expect(out()).toContain("60 of 100 messages have no lifecycle record");
  });

  it("stays silent about coverage when the ledger is complete", async () => {
    mockGetMetrics.mockResolvedValue(makeAgentView());
    await metrics("bot@agents.localhost", {});
    expect(out()).not.toContain("no lifecycle record");
  });

  it("reports a truncated breakdown rather than implying it is the whole account", async () => {
    mockAccountMetrics.mockResolvedValue(makeAccountView({ agentsTruncated: true }));
    await metrics(undefined, { byAgent: true });
    expect(out()).toContain("more agents have traffic than are listed");
  });

  it("parses RFC 3339 window flags and forwards them as Dates", async () => {
    mockGetMetrics.mockResolvedValue(makeAgentView());
    await metrics("bot@agents.localhost", {
      start: "2026-07-01T00:00:00Z",
      end: "2026-07-08T00:00:00Z",
    });
    const [, params] = mockGetMetrics.mock.calls[0];
    expect(params.start.toISOString()).toBe("2026-07-01T00:00:00.000Z");
    expect(params.end.toISOString()).toBe("2026-07-08T00:00:00.000Z");
  });

  it("rejects an offsetless timestamp rather than reading it as local time", async () => {
    await expect(
      metrics("bot@agents.localhost", { start: "2026-07-01T00:00:00" }),
    ).rejects.toThrow("process.exit");
    expect(mockGetMetrics).not.toHaveBeenCalled();
  });

  it("rejects an inverted window before calling the API", async () => {
    await expect(
      metrics("bot@agents.localhost", {
        start: "2026-07-08T00:00:00Z",
        end: "2026-07-01T00:00:00Z",
      }),
    ).rejects.toThrow("process.exit");
    expect(mockGetMetrics).not.toHaveBeenCalled();
  });

  it("emits the raw response under --json", async () => {
    const view = makeAgentView();
    mockGetMetrics.mockResolvedValue(view);
    await metrics("bot@agents.localhost", { json: true });
    expect(JSON.parse(out())).toMatchObject({ agentEmail: "bot@agents.localhost" });
  });
});
