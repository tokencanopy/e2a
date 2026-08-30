import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";

const mockList = vi.fn();
const mockCreate = vi.fn();
const mockGet = vi.fn();

vi.mock("../sdk.js", () => ({
  createClient: vi.fn(() => ({
    agents: { list: mockList, create: mockCreate, get: mockGet },
  })),
  requireAgentEmail: vi.fn(() => "bot@agents.e2a.dev"),
}));

const AGENT = {
  id: "agt_1",
  email: "tether@agents.e2a.dev",
  name: "tether",
  domain: "agents.e2a.dev",
  domainVerified: true,
  createdAt: new Date("2026-07-01T10:00:00Z"),
};

describe("agents commands", () => {
  let mockStdout: ReturnType<typeof vi.spyOn>;
  let mockStderr: ReturnType<typeof vi.spyOn>;
  let mockExit: ReturnType<typeof vi.spyOn>;
  let mockFetch: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    mockStdout = vi.spyOn(process.stdout, "write").mockImplementation(() => true);
    mockStderr = vi.spyOn(process.stderr, "write").mockImplementation(() => true);
    mockExit = vi.spyOn(process, "exit").mockImplementation(() => {
      throw new Error("process.exit");
    });
    // Bare-name expansion discovers shared_domain from GET /v1/info when it
    // isn't already known — stub it so these tests never hit the network.
    mockFetch = vi.fn().mockResolvedValue({
      ok: true,
      json: async () => ({ shared_domain: "agents.e2a.dev" }),
    });
    vi.stubGlobal("fetch", mockFetch);
  });

  afterEach(() => {
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
    vi.clearAllMocks();
  });

  it("create passes email + name and prints the created address", async () => {
    mockCreate.mockResolvedValue(AGENT);
    const { agentsCreate } = await import("../commands/agents.js");
    await agentsCreate("tether@agents.e2a.dev", { name: "tether" });

    expect(mockCreate).toHaveBeenCalledWith({ email: "tether@agents.e2a.dev", name: "tether" });
    expect(mockStdout).toHaveBeenCalledWith("tether@agents.e2a.dev\n");
    // A full address needs no shared-domain discovery.
    expect(mockFetch).not.toHaveBeenCalled();
  });

  it("create without an email exits USAGE (2)", async () => {
    const { agentsCreate } = await import("../commands/agents.js");
    await expect(agentsCreate(undefined, {})).rejects.toThrow("process.exit");
    expect(mockExit).toHaveBeenCalledWith(2);
  });

  it("create expands a bare name using a live-discovered shared domain", async () => {
    mockCreate.mockResolvedValue({ ...AGENT, email: "mybot@agents.e2a.dev" });
    const { agentsCreate } = await import("../commands/agents.js");
    await agentsCreate("mybot", {});

    expect(mockFetch).toHaveBeenCalledWith("https://e2a.dev/v1/info");
    expect(mockCreate).toHaveBeenCalledWith({ email: "mybot@agents.e2a.dev", name: undefined });
  });

  it("create refuses to guess the operator's domain when discovery finds no shared domain", async () => {
    mockFetch.mockResolvedValue({ ok: false, status: 404, json: async () => ({}) });
    const { agentsCreate } = await import("../commands/agents.js");

    await expect(agentsCreate("mybot", {})).rejects.toThrow("process.exit");
    expect(mockExit).toHaveBeenCalledWith(2);
    expect(mockStderr).toHaveBeenCalledWith(expect.stringContaining("no shared domain"));
    expect(mockCreate).not.toHaveBeenCalled();
  });

  it("list prints TSV (email, name, verification)", async () => {
    mockList.mockReturnValue(
      (async function* () {
        yield AGENT;
      })(),
    );
    const { agentsList } = await import("../commands/agents.js");
    await agentsList({});

    expect(mockStdout).toHaveBeenCalledWith("tether@agents.e2a.dev\ttether\tverified\n");
  });

  it("list sanitizes agent names containing tabs/newlines in TSV output", async () => {
    const agentWithTabInName = {
      ...AGENT,
      name: "agent\twith\ttabs",
    };
    mockList.mockReturnValue(
      (async function* () {
        yield agentWithTabInName;
      })(),
    );
    const { agentsList } = await import("../commands/agents.js");
    await agentsList({});

    // Tabs and newlines should be replaced with spaces
    expect(mockStdout).toHaveBeenCalledWith(
      "tether@agents.e2a.dev\tagent with tabs\tverified\n",
    );
  });

  it("list sanitizes agent names containing newlines in TSV output", async () => {
    const agentWithNewlineInName = {
      ...AGENT,
      name: "agent\nwith\nnewlines",
    };
    mockList.mockReturnValue(
      (async function* () {
        yield agentWithNewlineInName;
      })(),
    );
    const { agentsList } = await import("../commands/agents.js");
    await agentsList({});

    // Newlines should be replaced with spaces
    expect(mockStdout).toHaveBeenCalledWith(
      "tether@agents.e2a.dev\tagent with newlines\tverified\n",
    );
  });

  it("get prints the agent summary", async () => {
    mockGet.mockResolvedValue(AGENT);
    const { agentsGet } = await import("../commands/agents.js");
    await agentsGet("tether@agents.e2a.dev", {});

    const output = mockStdout.mock.calls.map((c: unknown[]) => c[0]).join("");
    expect(output).toContain("email:    tether@agents.e2a.dev");
    expect(output).toContain("domain:   agents.e2a.dev (verified)");
    expect(output).toContain("created:  2026-07-01T10:00:00.000Z");
  });
});
