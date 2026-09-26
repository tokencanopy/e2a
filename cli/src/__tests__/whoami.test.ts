import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";

const mockAccountGet = vi.fn();

vi.mock("../sdk.js", () => ({
  createClient: vi.fn(() => ({ account: { get: mockAccountGet } })),
  requireAgentEmail: vi.fn(() => "bot@agents.e2a.dev"),
}));

vi.mock("../config.js", () => ({
  loadConfig: vi.fn(() => ({
    api_key: "e2a_testkey",
    api_url: "https://e2a.dev",
    agent_email: "bot@agents.e2a.dev",
    shared_domain: "agents.e2a.dev",
  })),
}));

function makeAccount(overrides: Record<string, unknown> = {}) {
  return {
    user: { id: "usr_1", email: "owner@example.com" },
    scope: "account",
    planCode: "free",
    upgradeUrl: "https://e2a.dev/upgrade",
    limits: { maxAgents: 5, maxDomains: 1, maxMessagesMonth: 1000, maxStorageBytes: 0 },
    usage: { agents: 2, domains: 0, messagesMonth: 17, storageBytes: 0 },
    ...overrides,
  };
}

describe("whoami command", () => {
  let mockStdout: ReturnType<typeof vi.spyOn>;

  beforeEach(() => {
    mockStdout = vi.spyOn(process.stdout, "write").mockImplementation(() => true);
  });

  afterEach(() => {
    mockStdout.mockRestore();
    vi.clearAllMocks();
  });

  it("prints identity, scope, plan, and usage for an account key", async () => {
    mockAccountGet.mockResolvedValue(makeAccount());
    const { whoami } = await import("../commands/whoami.js");
    await whoami({});

    const output = mockStdout.mock.calls.map((c: unknown[]) => c[0]).join("");
    expect(output).toContain("user:  owner@example.com (usr_1)");
    expect(output).toContain("scope: account");
    // Account keys aren't inbox-bound; the preflight shows the config default
    // that send/reply will actually use.
    expect(output).toContain("agent: bot@agents.e2a.dev (default from config/E2A_AGENT_EMAIL)");
    expect(output).toContain("plan:  free");
    expect(output).toContain("usage: 2/5 agents, 17/1000 messages this month");
  });

  it("shows the bound agent for an agent-scoped key", async () => {
    mockAccountGet.mockResolvedValue(
      makeAccount({ scope: "agent", agentEmail: "tether@agents.e2a.dev" }),
    );
    const { whoami } = await import("../commands/whoami.js");
    await whoami({});

    const output = mockStdout.mock.calls.map((c: unknown[]) => c[0]).join("");
    expect(output).toContain("scope: agent");
    expect(output).toContain("agent: tether@agents.e2a.dev");
  });

  it("emits raw JSON with --json", async () => {
    const account = makeAccount();
    mockAccountGet.mockResolvedValue(account);
    const { whoami } = await import("../commands/whoami.js");
    await whoami({ json: true });

    expect(mockStdout).toHaveBeenCalledWith(JSON.stringify(account) + "\n");
  });

  // ── sending_access (beta, additive) ──────────────────────────────

  it("shows the restricted-sending line only when enforced and neither grant applies", async () => {
    mockAccountGet.mockResolvedValue(
      makeAccount({
        sendingAccess: {
          enforcementApplies: true,
          sharedExternalApproved: false,
          paidExternalSendingEntitled: false,
          ownerRecipientVerified: true,
        },
      }),
    );
    const { whoami } = await import("../commands/whoami.js");
    await whoami({});

    const output = mockStdout.mock.calls.map((c: unknown[]) => c[0]).join("");
    expect(output).toContain("External sending: restricted");
    expect(output).toContain("e2a sending-access request");
  });

  it("says nothing new when enforcement does not apply", async () => {
    mockAccountGet.mockResolvedValue(
      makeAccount({
        sendingAccess: {
          enforcementApplies: false,
          sharedExternalApproved: false,
          paidExternalSendingEntitled: false,
          ownerRecipientVerified: true,
        },
      }),
    );
    const { whoami } = await import("../commands/whoami.js");
    await whoami({});

    const output = mockStdout.mock.calls.map((c: unknown[]) => c[0]).join("");
    expect(output).not.toContain("External sending:");
  });

  it("says nothing new when the account already has a grant (shared approval)", async () => {
    mockAccountGet.mockResolvedValue(
      makeAccount({
        sendingAccess: {
          enforcementApplies: true,
          sharedExternalApproved: true,
          paidExternalSendingEntitled: false,
          ownerRecipientVerified: true,
        },
      }),
    );
    const { whoami } = await import("../commands/whoami.js");
    await whoami({});

    const output = mockStdout.mock.calls.map((c: unknown[]) => c[0]).join("");
    expect(output).not.toContain("External sending:");
  });

  it("says nothing new when the account is paid-plan entitled", async () => {
    mockAccountGet.mockResolvedValue(
      makeAccount({
        sendingAccess: {
          enforcementApplies: true,
          sharedExternalApproved: false,
          paidExternalSendingEntitled: true,
          ownerRecipientVerified: true,
        },
      }),
    );
    const { whoami } = await import("../commands/whoami.js");
    await whoami({});

    const output = mockStdout.mock.calls.map((c: unknown[]) => c[0]).join("");
    expect(output).not.toContain("External sending:");
  });

  it("says nothing new when sending_access is omitted (older/self-host deployment)", async () => {
    mockAccountGet.mockResolvedValue(makeAccount());
    const { whoami } = await import("../commands/whoami.js");
    await whoami({});

    const output = mockStdout.mock.calls.map((c: unknown[]) => c[0]).join("");
    expect(output).not.toContain("External sending:");
  });

  it("--json includes the raw sending_access object verbatim", async () => {
    const account = makeAccount({
      sendingAccess: {
        enforcementApplies: true,
        sharedExternalApproved: false,
        paidExternalSendingEntitled: false,
        ownerRecipientVerified: true,
      },
    });
    mockAccountGet.mockResolvedValue(account);
    const { whoami } = await import("../commands/whoami.js");
    await whoami({ json: true });

    expect(mockStdout).toHaveBeenCalledWith(JSON.stringify(account) + "\n");
  });
});
