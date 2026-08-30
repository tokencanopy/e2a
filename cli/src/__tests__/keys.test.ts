import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";

const mockCreate = vi.fn();
const mockList = vi.fn();
const mockDelete = vi.fn();

vi.mock("../sdk.js", () => ({
  createClient: vi.fn(() => ({
    account: { apiKeys: { create: mockCreate, list: mockList, delete: mockDelete } },
  })),
}));

// Pin the hostname so the generated default key names are assertable.
// Also mock homedir for config.ts (which is imported by keys.ts).
vi.mock("node:os", () => ({
  hostname: () => "testbox",
  homedir: () => "/tmp/test-home",
}));

// Mock the config file system operations so loadConfig() uses defaults.
vi.mock("node:fs", () => ({
  readFileSync: () => {
    throw new Error("ENOENT");
  },
}));

const AGENT_KEY = {
  id: "key_agt1",
  key: "e2a_agt_plaintext",
  keyPrefix: "e2a_agt_",
  scope: "agent",
  agentEmail: "bot@agents.e2a.dev",
  name: "agent key @testbox",
};

const ACCOUNT_KEY = {
  id: "key_acct1",
  key: "e2a_acct_plaintext",
  keyPrefix: "e2a_acct_",
  scope: "account",
  name: "account key @testbox",
};

describe("keys commands", () => {
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
    // Bare-slug expansion now discovers shared_domain from GET /v1/info when
    // it isn't already known (config.ts's loadConfig() has no baked default
    // any more) — stub it so these tests never make a real network call.
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

  describe("create --agent (least-privilege inbox-bound key)", () => {
    it("sends scope=agent + agent_email and a host-stamped default name", async () => {
      mockCreate.mockResolvedValue(AGENT_KEY);
      const { keysCreate } = await import("../commands/keys.js");
      await keysCreate({ agent: "bot@agents.e2a.dev" });

      // scope must be the literal wire value "agent" — the SDK types it as an
      // enum and keys.ts casts to it, so a rename would compile but 400.
      expect(mockCreate).toHaveBeenCalledWith({
        name: "agent key @testbox",
        scope: "agent",
        agentEmail: "bot@agents.e2a.dev",
      });
      // A full address needs no shared-domain discovery.
      expect(mockFetch).not.toHaveBeenCalled();
    });

    it("expands bare agent slug on a live-discovered shared domain", async () => {
      mockCreate.mockResolvedValue(AGENT_KEY);
      const { keysCreate } = await import("../commands/keys.js");
      await keysCreate({ agent: "mybot" });

      expect(mockFetch).toHaveBeenCalledWith("https://e2a.dev/v1/info");
      expect(mockCreate).toHaveBeenCalledWith({
        name: "agent key @testbox",
        scope: "agent",
        agentEmail: "mybot@agents.e2a.dev",
      });
      // The confirmation must echo the EXPANDED address. Echoing the raw slug
      // would tell the user the key is bound to "mybot" when it is bound to
      // mybot@agents.e2a.dev — a confirmation that contradicts what was made.
      expect(mockStderr).toHaveBeenCalledWith(
        "Key key_agt1 created (agent-scoped: mybot@agents.e2a.dev). Shown once — store it now.\n",
      );
    });

    it("refuses to guess the operator's domain when discovery finds no shared domain", async () => {
      // A self-hosted deployment with no /v1/info shared_domain (or an
      // unreachable one) must NOT silently fall back to agents.e2a.dev.
      mockFetch.mockResolvedValue({ ok: false, status: 404, json: async () => ({}) });
      const { keysCreate } = await import("../commands/keys.js");

      await expect(keysCreate({ agent: "mybot" })).rejects.toThrow("process.exit");
      expect(mockExit).toHaveBeenCalledWith(2);
      expect(mockStderr).toHaveBeenCalledWith(expect.stringContaining("no shared domain"));
      expect(mockCreate).not.toHaveBeenCalled();
    });

    it("prints the plaintext key alone on stdout, the warning on stderr", async () => {
      mockCreate.mockResolvedValue(AGENT_KEY);
      const { keysCreate } = await import("../commands/keys.js");
      await keysCreate({ agent: "bot@agents.e2a.dev" });

      // The documented capture contract: KEY=$(e2a keys create --agent …) must
      // yield exactly the key. Anything else on stdout corrupts the variable.
      expect(mockStdout.mock.calls).toEqual([["e2a_agt_plaintext\n"]]);
      expect(mockStderr).toHaveBeenCalledWith(
        "Key key_agt1 created (agent-scoped: bot@agents.e2a.dev). Shown once — store it now.\n",
      );
    });

    it("honors an explicit --name over the generated default", async () => {
      mockCreate.mockResolvedValue(AGENT_KEY);
      const { keysCreate } = await import("../commands/keys.js");
      await keysCreate({ agent: "bot@agents.e2a.dev", name: "ci-runner" });

      expect(mockCreate).toHaveBeenCalledWith({
        name: "ci-runner",
        scope: "agent",
        agentEmail: "bot@agents.e2a.dev",
      });
    });

    it("--json emits the whole object and no bare key line", async () => {
      mockCreate.mockResolvedValue(AGENT_KEY);
      const { keysCreate } = await import("../commands/keys.js");
      await keysCreate({ agent: "bot@agents.e2a.dev", json: true });

      expect(mockStdout.mock.calls).toEqual([[JSON.stringify(AGENT_KEY) + "\n"]]);
      expect(mockStderr).not.toHaveBeenCalled();
    });

    it("propagates a server rejection instead of printing a partial success", async () => {
      // An unowned/typo'd inbox is a server-side 400/404. The command must not
      // swallow it — a harness that saw exit 0 would store an empty key.
      mockCreate.mockRejectedValue(new Error("agent not found"));
      const { keysCreate } = await import("../commands/keys.js");

      await expect(keysCreate({ agent: "typo@agents.e2a.dev" })).rejects.toThrow("agent not found");
      expect(mockStdout).not.toHaveBeenCalled();
    });
  });

  describe("create without --agent", () => {
    it("omits scope and agent_email entirely (server defaults to account)", async () => {
      mockCreate.mockResolvedValue(ACCOUNT_KEY);
      const { keysCreate } = await import("../commands/keys.js");
      await keysCreate({});

      expect(mockCreate).toHaveBeenCalledWith({ name: "account key @testbox" });
      const sent = mockCreate.mock.calls[0][0];
      expect(sent).not.toHaveProperty("scope");
      expect(sent).not.toHaveProperty("agentEmail");
    });

    it("labels the key account-scoped in the stderr notice", async () => {
      mockCreate.mockResolvedValue(ACCOUNT_KEY);
      const { keysCreate } = await import("../commands/keys.js");
      await keysCreate({});

      expect(mockStderr).toHaveBeenCalledWith(
        "Key key_acct1 created (account-scoped). Shown once — store it now.\n",
      );
    });
  });

  describe("list", () => {
    it("prints TSV with the bound inbox for agent keys and a gap for account keys", async () => {
      mockList.mockReturnValue(
        (async function* () {
          yield AGENT_KEY;
          yield ACCOUNT_KEY;
        })(),
      );
      const { keysList } = await import("../commands/keys.js");
      await keysList({});

      expect(mockStdout).toHaveBeenCalledWith(
        "key_agt1\te2a_agt_\tagent\tbot@agents.e2a.dev\tagent key @testbox\n",
      );
      // No agentEmail → an empty column, so the field count stays stable for cut/awk.
      expect(mockStdout).toHaveBeenCalledWith(
        "key_acct1\te2a_acct_\taccount\t\taccount key @testbox\n",
      );
    });

    it("sanitizes key names containing tabs/newlines in TSV output", async () => {
      const keyWithTabInName = {
        ...AGENT_KEY,
        name: "key\twith\ttabs",
      };
      mockList.mockReturnValue(
        (async function* () {
          yield keyWithTabInName;
        })(),
      );
      const { keysList } = await import("../commands/keys.js");
      await keysList({});

      // Tabs and newlines should be replaced with spaces
      expect(mockStdout).toHaveBeenCalledWith(
        "key_agt1\te2a_agt_\tagent\tbot@agents.e2a.dev\tkey with tabs\n",
      );
    });

    it("sanitizes key names containing newlines in TSV output", async () => {
      const keyWithNewlineInName = {
        ...AGENT_KEY,
        name: "key\nwith\nnewlines",
      };
      mockList.mockReturnValue(
        (async function* () {
          yield keyWithNewlineInName;
        })(),
      );
      const { keysList } = await import("../commands/keys.js");
      await keysList({});

      // Newlines should be replaced with spaces
      expect(mockStdout).toHaveBeenCalledWith(
        "key_agt1\te2a_agt_\tagent\tbot@agents.e2a.dev\tkey with newlines\n",
      );
    });
  });

  describe("delete", () => {
    it("echoes the id the server confirmed, not the caller's input", async () => {
      mockDelete.mockResolvedValue({ deleted: true, id: "key_agt1" });
      const { keysDelete } = await import("../commands/keys.js");
      await keysDelete("key_agt1");

      expect(mockDelete).toHaveBeenCalledWith("key_agt1");
      expect(mockStdout).toHaveBeenCalledWith("revoked key_agt1\n");
    });

    it("without an id exits USAGE (2) before touching the API", async () => {
      const { keysDelete } = await import("../commands/keys.js");
      await expect(keysDelete(undefined)).rejects.toThrow("process.exit");
      expect(mockExit).toHaveBeenCalledWith(2);
      expect(mockDelete).not.toHaveBeenCalled();
    });
  });
});
