import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";

const mockAccountList = vi.fn();
const mockAccountDelete = vi.fn();
const mockAgentList = vi.fn();
const mockAgentCreate = vi.fn();
const mockAgentDelete = vi.fn();

vi.mock("../sdk.js", () => ({
  createClient: vi.fn(() => ({
    account: {
      suppressions: {
        list: mockAccountList,
        delete: mockAccountDelete,
      },
    },
    agents: {
      listSuppressions: mockAgentList,
      createSuppression: mockAgentCreate,
      deleteSuppression: mockAgentDelete,
    },
  })),
}));

describe("suppressions commands", () => {
  let stdout: ReturnType<typeof vi.spyOn>;

  beforeEach(() => {
    stdout = vi.spyOn(process.stdout, "write").mockImplementation(() => true);
    vi.spyOn(process.stderr, "write").mockImplementation(() => true);
    vi.spyOn(process, "exit").mockImplementation(() => {
      throw new Error("process.exit");
    });
  });

  afterEach(() => {
    vi.restoreAllMocks();
    vi.clearAllMocks();
  });

  it("lists the account suppression list as TSV by default", async () => {
    mockAccountList.mockReturnValue({
      toArray: vi.fn(async () => [{
        address: "gone@example.net",
        source: "bounce",
        reason: "smtp; 550 5.1.1\trecipient not found",
        sourceMessageId: "msg_1",
        createdAt: new Date("2026-07-17T21:12:26.000Z"),
      }]),
    });
    const { suppressionsList } = await import("../commands/suppressions.js");
    await suppressionsList({});
    expect(mockAccountList).toHaveBeenCalledWith({});
    expect(mockAgentList).not.toHaveBeenCalled();
    const line = String(stdout.mock.calls[0][0]);
    // The embedded tab in the reason must be sanitized, or columns shift.
    expect(line).toBe("gone@example.net\tbounce\tsmtp; 550 5.1.1 recipient not found\t2026-07-17T21:12:26.000Z\n");
  });

  it("lists a single agent's suppressions with --agent", async () => {
    mockAgentList.mockReturnValue({
      toArray: vi.fn(async () => [{
        agentEmail: "bot@agents.e2a.dev",
        address: "optout@example.net",
        source: "unsubscribe",
        createdAt: new Date("2026-07-20T09:00:00.000Z"),
      }]),
    });
    const { suppressionsList } = await import("../commands/suppressions.js");
    await suppressionsList({ agent: "bot@agents.e2a.dev", json: true });
    expect(mockAgentList).toHaveBeenCalledWith("bot@agents.e2a.dev", {});
    expect(mockAccountList).not.toHaveBeenCalled();
    const row = JSON.parse(String(stdout.mock.calls[0][0]));
    expect(row.address).toBe("optout@example.net");
    expect(row.source).toBe("unsubscribe");
  });

  it("caps list output via --limit and rejects a non-positive value", async () => {
    const toArray = vi.fn(async () => []);
    mockAccountList.mockReturnValue({ toArray });
    const { suppressionsList } = await import("../commands/suppressions.js");
    await suppressionsList({ limit: "5" });
    expect(toArray).toHaveBeenCalledWith({ limit: 5 });
    await expect(suppressionsList({ limit: "0" })).rejects.toThrow("process.exit");
  });

  it("add requires --agent (there is no account-level create)", async () => {
    const { suppressionsAdd } = await import("../commands/suppressions.js");
    await expect(suppressionsAdd("optout@example.net", {})).rejects.toThrow("process.exit");
    expect(mockAgentCreate).not.toHaveBeenCalled();
  });

  it("add creates a manual agent-scoped block with an optional reason", async () => {
    mockAgentCreate.mockResolvedValue({
      agentEmail: "bot@agents.e2a.dev",
      address: "optout@example.net",
      source: "manual",
      createdAt: new Date("2026-08-01T00:00:00.000Z"),
    });
    const { suppressionsAdd } = await import("../commands/suppressions.js");
    await suppressionsAdd("optout@example.net", {
      agent: "bot@agents.e2a.dev",
      reason: "asked us to stop",
    });
    expect(mockAgentCreate).toHaveBeenCalledWith("bot@agents.e2a.dev", {
      address: "optout@example.net",
      reason: "asked us to stop",
    });
    expect(String(stdout.mock.calls[0][0])).toContain("optout@example.net");
  });

  it("remove routes to the account list without --agent", async () => {
    mockAccountDelete.mockResolvedValue({ deleted: true, address: "gone@example.net" });
    const { suppressionsRemove } = await import("../commands/suppressions.js");
    await suppressionsRemove("gone@example.net", {});
    expect(mockAccountDelete).toHaveBeenCalledWith("gone@example.net");
    expect(mockAgentDelete).not.toHaveBeenCalled();
    expect(String(stdout.mock.calls[0][0])).toBe("deleted gone@example.net\n");
  });

  it("remove routes to the agent list with --agent", async () => {
    mockAgentDelete.mockResolvedValue({ deleted: true, address: "optout@example.net" });
    const { suppressionsRemove } = await import("../commands/suppressions.js");
    await suppressionsRemove("optout@example.net", { agent: "bot@agents.e2a.dev", json: true });
    expect(mockAgentDelete).toHaveBeenCalledWith("bot@agents.e2a.dev", "optout@example.net");
    expect(mockAccountDelete).not.toHaveBeenCalled();
    expect(JSON.parse(String(stdout.mock.calls[0][0]))).toEqual({
      deleted: true,
      address: "optout@example.net",
    });
  });
});
