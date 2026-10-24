import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";

const mockGetProtection = vi.fn();
const mockReplaceProtection = vi.fn();

vi.mock("../sdk.js", () => ({
  createClient: vi.fn(() => ({
    agents: { getProtection: mockGetProtection, replaceProtection: mockReplaceProtection },
  })),
  requireAgentEmail: vi.fn(() => "bot@agents.e2a.dev"),
}));

// A doc with deliberately non-default values everywhere, so any knob the
// command wasn't asked to touch shows up as a diff if it gets reset.
function makeDoc() {
  return {
    inbound: {
      gate: { policy: "open", allowlist: [], action: "review" },
      scan: { sensitivity: "high" },
    },
    outbound: {
      gate: { policy: "allowlist", allowlist: ["trusted@x.com"], action: "review" },
      scan: { sensitivity: "medium" },
    },
    holds: { ttlSeconds: 3600, onExpiry: "approve", suppressNotifications: false },
  };
}

describe("protection commands", () => {
  let mockStdout: ReturnType<typeof vi.spyOn>;
  let mockExit: ReturnType<typeof vi.spyOn>;

  beforeEach(() => {
    mockStdout = vi.spyOn(process.stdout, "write").mockImplementation(() => true);
    vi.spyOn(process.stderr, "write").mockImplementation(() => true);
    mockExit = vi.spyOn(process, "exit").mockImplementation(() => {
      throw new Error("process.exit");
    });
  });

  afterEach(() => {
    vi.restoreAllMocks();
    vi.clearAllMocks();
  });

  it("set --outbound-review off flips ONLY the outbound gate action + scan", async () => {
    mockGetProtection.mockResolvedValue(makeDoc());
    mockReplaceProtection.mockImplementation(async (_email: string, doc: unknown) => doc);
    const { protectionSet } = await import("../commands/protection.js");
    await protectionSet("bot@agents.e2a.dev", { outboundReview: "off" });

    const put = mockReplaceProtection.mock.calls[0][1];
    expect(put.outbound.gate.action).toBe("flag");
    expect(put.outbound.scan.sensitivity).toBe("off");
    expect(put.outbound.requireReview).toBe(false);
    // Untouched knobs survive: gate policy/allowlist, inbound, holds.
    expect(put.outbound.gate.policy).toBe("allowlist");
    expect(put.outbound.gate.allowlist).toEqual(["trusted@x.com"]);
    expect(put.inbound).toEqual(makeDoc().inbound);
    expect(put.holds).toEqual(makeDoc().holds);
  });

  it("set --inbound-review on flips the gate action and leaves scan tuning alone", async () => {
    const doc = makeDoc();
    doc.inbound.gate.action = "flag";
    mockGetProtection.mockResolvedValue(doc);
    mockReplaceProtection.mockImplementation(async (_email: string, d: unknown) => d);
    const { protectionSet } = await import("../commands/protection.js");
    await protectionSet("bot@agents.e2a.dev", { inboundReview: "on" });

    const put = mockReplaceProtection.mock.calls[0][1];
    expect(put.inbound.gate.action).toBe("review");
    expect(put.inbound.scan.sensitivity).toBe("high"); // untouched
  });

  it("set --suppress-notifications on changes only notification delivery", async () => {
    mockGetProtection.mockResolvedValue(makeDoc());
    mockReplaceProtection.mockImplementation(async (_email: string, doc: unknown) => doc);
    const { protectionSet } = await import("../commands/protection.js");
    await protectionSet("bot@agents.e2a.dev", { suppressNotifications: "on" });

    const put = mockReplaceProtection.mock.calls[0][1];
    expect(put.holds.suppressNotifications).toBe(true);
    expect(put.inbound).toEqual(makeDoc().inbound);
    expect(put.outbound).toEqual(makeDoc().outbound);
    expect(put.holds.ttlSeconds).toBe(3600);
    expect(put.holds.onExpiry).toBe("approve");
  });

  it("set --outbound-review on re-enables a disabled scan (off→on round-trip restores holding)", async () => {
    // Under gate policy "open" every sender matches, so the gate action never
    // fires — HITL "on" without a scan would look on while holding nothing.
    const doc = makeDoc();
    doc.outbound.gate.action = "flag";
    doc.outbound.scan.sensitivity = "off";
    mockGetProtection.mockResolvedValue(doc);
    mockReplaceProtection.mockImplementation(async (_email: string, d: unknown) => d);
    const { protectionSet } = await import("../commands/protection.js");
    await protectionSet("bot@agents.e2a.dev", { outboundReview: "on" });

    const put = mockReplaceProtection.mock.calls[0][1];
    expect(put.outbound.gate.action).toBe("review");
    expect(put.outbound.scan.sensitivity).toBe("medium");
    // #989: the switch that actually holds every send, independent of the gate.
    expect(put.outbound.requireReview).toBe(true);
  });

  it("NEVER writes when the read fails — a transient GET error must not reset the doc", async () => {
    mockGetProtection.mockRejectedValue(new Error("boom"));
    const { protectionSet } = await import("../commands/protection.js");

    await expect(
      protectionSet("bot@agents.e2a.dev", { outboundReview: "off" }),
    ).rejects.toThrow("boom");
    expect(mockReplaceProtection).not.toHaveBeenCalled();
  });

  it("rejects missing email, missing knobs, and bad values with USAGE (2)", async () => {
    const { protectionSet } = await import("../commands/protection.js");

    await expect(protectionSet(undefined, { outboundReview: "off" })).rejects.toThrow("process.exit");
    await expect(protectionSet("a@b.c", {})).rejects.toThrow("process.exit");
    await expect(protectionSet("a@b.c", { outboundReview: "sideways" })).rejects.toThrow("process.exit");
    await expect(protectionSet("a@b.c", { suppressNotifications: "sideways" })).rejects.toThrow("process.exit");
    expect(mockExit).toHaveBeenCalledWith(2);
    expect(mockGetProtection).not.toHaveBeenCalled();
  });

  it("get prints a human summary (and raw JSON with --json)", async () => {
    mockGetProtection.mockResolvedValue(makeDoc());
    const { protectionGet } = await import("../commands/protection.js");
    await protectionGet("bot@agents.e2a.dev", {});

    const output = mockStdout.mock.calls.map((c: unknown[]) => c[0]).join("");
    expect(output).toContain("outbound: gate=allowlist/review scan=medium require_review=off");
    expect(output).toContain("inbound:  gate=open/review scan=high");
    expect(output).toContain("holds:    ttl=3600s on_expiry=approve");
    expect(output).toContain("notifications=enabled");

    mockStdout.mockClear();
    await protectionGet("bot@agents.e2a.dev", { json: true });
    expect(JSON.parse(mockStdout.mock.calls[0][0] as string)).toEqual(makeDoc());
  });
});
