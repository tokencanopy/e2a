import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";

const mockAccountGet = vi.fn();
const mockAccountDelete = vi.fn();
const mockSaveConfig = vi.fn();
const mockQuestion = vi.fn();
const mockClose = vi.fn();

vi.mock("../sdk.js", () => ({
  createClient: vi.fn(() => ({
    account: { get: mockAccountGet, delete: mockAccountDelete },
  })),
}));

vi.mock("../config.js", () => ({
  saveConfig: mockSaveConfig,
}));

vi.mock("node:readline/promises", () => ({
  createInterface: vi.fn(() => ({ question: mockQuestion, close: mockClose })),
}));

const TRASH_RECEIPT = {
  deleted: true,
  mode: "trash",
  purgeAfter: new Date("2026-10-26T00:00:00Z"),
  messagesDeleted: 0,
  agentsDeleted: 3,
  domainsDeleted: 1,
  apiKeysDeleted: 2,
  sessionsDeleted: 1,
  userDeleted: false,
};

const PERMANENT_RECEIPT = {
  deleted: true,
  mode: "permanent",
  messagesDeleted: 42,
  agentsDeleted: 3,
  domainsDeleted: 1,
  apiKeysDeleted: 2,
  sessionsDeleted: 1,
  userDeleted: true,
};

describe("account delete command", () => {
  let mockStdout: ReturnType<typeof vi.spyOn>;
  let mockStderr: ReturnType<typeof vi.spyOn>;
  let mockExit: ReturnType<typeof vi.spyOn>;
  let originalIsTTY: typeof process.stdin.isTTY;

  beforeEach(() => {
    mockStdout = vi.spyOn(process.stdout, "write").mockImplementation(() => true);
    mockStderr = vi.spyOn(process.stderr, "write").mockImplementation(() => true);
    mockExit = vi.spyOn(process, "exit").mockImplementation(() => {
      throw new Error("process.exit");
    });
    originalIsTTY = process.stdin.isTTY;
    mockAccountGet.mockResolvedValue({ user: { email: "owner@example.com", id: "usr_1" } });
  });

  afterEach(() => {
    process.stdin.isTTY = originalIsTTY;
    vi.restoreAllMocks();
    vi.clearAllMocks();
  });

  describe("--yes (non-interactive)", () => {
    it("defaults to the trash path: omits permanent, clears local config, and reports purgeAfter", async () => {
      mockAccountDelete.mockResolvedValue(TRASH_RECEIPT);
      const { accountDelete } = await import("../commands/account.js");
      await accountDelete({ yes: true });

      expect(mockAccountDelete).toHaveBeenCalledWith({ permanent: undefined });
      expect(mockAccountGet).not.toHaveBeenCalled(); // no prompt needed with --yes
      expect(mockSaveConfig).toHaveBeenCalledWith({ api_key: "", key_scope: "" });
      const out = mockStdout.mock.calls.map((c: unknown[]) => c[0]).join("");
      expect(out).toContain("Account moved to the trash.");
      expect(out).toContain("2026-10-26T00:00:00.000Z");
      const err = mockStderr.mock.calls.map((c: unknown[]) => c[0]).join("");
      expect(err).toContain("API key used for this command is now revoked");
    });

    it("normalizes a literal false --permanent (as the arg parser's hasFlag() produces) to omitted", async () => {
      // bin/e2a.ts always calls accountDelete({ permanent: hasFlag(...) }),
      // which is a real `false` — not `undefined` — when the flag is absent.
      // The command must still omit `permanent` from the wire call rather
      // than forwarding a literal `false` (the generated API appends
      // `permanent=false` for any non-undefined value).
      mockAccountDelete.mockResolvedValue(TRASH_RECEIPT);
      const { accountDelete } = await import("../commands/account.js");
      await accountDelete({ yes: true, permanent: false, json: false });

      expect(mockAccountDelete).toHaveBeenCalledWith({ permanent: undefined });
    });

    it("--permanent sends permanent:true and reports irreversible erasure", async () => {
      mockAccountDelete.mockResolvedValue(PERMANENT_RECEIPT);
      const { accountDelete } = await import("../commands/account.js");
      await accountDelete({ yes: true, permanent: true });

      expect(mockAccountDelete).toHaveBeenCalledWith({ permanent: true });
      const out = mockStdout.mock.calls.map((c: unknown[]) => c[0]).join("");
      expect(out).toContain("Account permanently deleted. This cannot be undone.");
      expect(mockSaveConfig).toHaveBeenCalledWith({ api_key: "", key_scope: "" });
    });

    it("--json prints the raw receipt and nothing else on stdout", async () => {
      mockAccountDelete.mockResolvedValue(TRASH_RECEIPT);
      const { accountDelete } = await import("../commands/account.js");
      await accountDelete({ yes: true, json: true });

      expect(mockStdout.mock.calls).toEqual([[JSON.stringify(TRASH_RECEIPT) + "\n"]]);
    });
  });

  describe("interactive confirmation", () => {
    beforeEach(() => {
      process.stdin.isTTY = true;
    });

    it("proceeds when the typed answer matches the account email", async () => {
      mockQuestion.mockResolvedValue("owner@example.com");
      mockAccountDelete.mockResolvedValue(TRASH_RECEIPT);
      const { accountDelete } = await import("../commands/account.js");
      await accountDelete({});

      expect(mockAccountGet).toHaveBeenCalledOnce();
      expect(mockQuestion).toHaveBeenCalledWith(
        expect.stringContaining("owner@example.com"),
      );
      expect(mockClose).toHaveBeenCalledOnce();
      expect(mockAccountDelete).toHaveBeenCalledWith({ permanent: undefined });
    });

    it("refuses (exit USAGE=2) when the typed answer does not match, without calling delete", async () => {
      mockQuestion.mockResolvedValue("not-the-right-email@example.com");
      const { accountDelete } = await import("../commands/account.js");

      await expect(accountDelete({})).rejects.toThrow("process.exit");
      expect(mockExit).toHaveBeenCalledWith(2);
      expect(mockAccountDelete).not.toHaveBeenCalled();
      expect(mockSaveConfig).not.toHaveBeenCalled();
    });
  });

  describe("non-TTY without --yes", () => {
    it("refuses (exit USAGE=2) before any network call", async () => {
      process.stdin.isTTY = false;
      const { accountDelete } = await import("../commands/account.js");

      await expect(accountDelete({})).rejects.toThrow("process.exit");
      expect(mockExit).toHaveBeenCalledWith(2);
      expect(mockAccountGet).not.toHaveBeenCalled();
      expect(mockAccountDelete).not.toHaveBeenCalled();
      const err = mockStderr.mock.calls.map((c: unknown[]) => c[0]).join("");
      expect(err).toContain("stdin is not a TTY");
    });
  });

  describe("API errors", () => {
    it("propagates a server rejection instead of printing a partial success", async () => {
      // A 409 send_in_progress (or any API failure) must not be swallowed —
      // the top-level catch in bin/e2a.ts maps it to the right exit code via
      // exitCodeForAPIError; the command itself never catches it.
      mockAccountDelete.mockRejectedValue(new Error("send_in_progress"));
      const { accountDelete } = await import("../commands/account.js");

      await expect(accountDelete({ yes: true })).rejects.toThrow("send_in_progress");
      expect(mockSaveConfig).not.toHaveBeenCalled();
      expect(mockStdout).not.toHaveBeenCalled();
    });
  });
});
