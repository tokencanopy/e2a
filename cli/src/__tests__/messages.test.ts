import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";

const mockList = vi.fn();
const mockGet = vi.fn();
const mockGetLifecycle = vi.fn();

vi.mock("../sdk.js", () => ({
  createClient: vi.fn(() => ({
    messages: { list: mockList, get: mockGet, getLifecycle: mockGetLifecycle },
  })),
  requireAgentEmail: vi.fn(() => "bot@agents.e2a.dev"),
}));

function summaries(...items: Array<Record<string, unknown>>) {
  return (async function* () {
    for (const item of items) yield item;
  })();
}

const M1 = {
  id: "msg_1",
  headerFrom: "you@example.com",
  threadId: "thr_0123456789abcdef0123456789abcdef",
  createdAt: new Date("2026-07-01T10:00:00Z"),
  subject: "re: status",
};
const M2 = {
  id: "msg_2",
  headerFrom: "other@example.com",
  createdAt: new Date("2026-07-01T10:05:00Z"),
  subject: "re: status",
};

describe("messages commands", () => {
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

  it("lists messages as TSV, oldest first, with filters passed through", async () => {
    mockList.mockReturnValue(summaries(M1, M2));
    const { messagesList } = await import("../commands/messages.js");
    await messagesList({
      direction: "inbound",
      since: "2026-07-01T09:00:00Z",
      conversation: "conv-1",
    });

    expect(mockList).toHaveBeenCalledWith("bot@agents.e2a.dev", {
      sort: "asc",
      readStatus: "all", // poll-loop safe: never hide messages read elsewhere
      direction: "inbound",
      since: "2026-07-01T09:00:00Z",
      conversationId: "conv-1",
      limit: 100, // unbounded lists page at the server max (== the 100 default)
    });
    expect(mockStdout).toHaveBeenCalledWith(
      "msg_1\tyou@example.com\t2026-07-01T10:00:00.000Z\n",
    );
    expect(mockStdout).toHaveBeenCalledWith(
      "msg_2\tother@example.com\t2026-07-01T10:05:00.000Z\n",
    );
  });

  it("passes filter through verbatim and omits it when unset", async () => {
    mockList.mockReturnValue(summaries());
    const { messagesList } = await import("../commands/messages.js");

    await messagesList({ filter: "label:urgent" });
    expect(mockList).toHaveBeenCalledWith("bot@agents.e2a.dev", {
      sort: "asc",
      readStatus: "all",
      filter: "label:urgent",
      limit: 100,
    });

    mockList.mockClear();
    await messagesList({});
    expect(mockList).toHaveBeenCalledWith("bot@agents.e2a.dev", {
      sort: "asc",
      readStatus: "all",
      limit: 100,
    });
  });

  it("sanitizes TSV delimiters out of the sender-controlled From field", async () => {
    mockList.mockReturnValue(
      summaries({
        id: "msg_evil",
        headerFrom: 'Evil\tName\n"attacker@x.com"',
        createdAt: new Date("2026-07-01T10:00:00Z"),
        subject: "s",
      }),
    );
    const { messagesList } = await import("../commands/messages.js");
    await messagesList({});

    const line = mockStdout.mock.calls[0][0] as string;
    // Exactly three fields, no injected rows: tabs/newlines collapsed.
    expect(line.split("\t")).toHaveLength(3);
    expect(line.trim().split("\n")).toHaveLength(1);
    expect(line).toContain('Evil Name "attacker@x.com"');
  });

  it("emits NDJSON with the canonical headerFrom identity", async () => {
    mockList.mockReturnValue(summaries(M1));
    const { messagesList } = await import("../commands/messages.js");
    await messagesList({ json: true });

    const line = mockStdout.mock.calls[0][0] as string;
    const parsed = JSON.parse(line);
    expect(parsed.headerFrom).toBe("you@example.com");
    expect(parsed.threadId).toBe("thr_0123456789abcdef0123456789abcdef");
    expect(parsed.id).toBe("msg_1");
    expect(parsed.createdAt).toBe("2026-07-01T10:00:00.000Z");
  });

  it("passes --read-status through and rejects invalid values", async () => {
    mockList.mockReturnValue(summaries());
    const { messagesList } = await import("../commands/messages.js");
    await messagesList({ readStatus: "unread" });
    expect(mockList.mock.calls[0][1].readStatus).toBe("unread");

    await expect(messagesList({ readStatus: "sideways" })).rejects.toThrow("process.exit");
    expect(mockExit).toHaveBeenCalledWith(2);
  });

  it("stops after --limit items", async () => {
    mockList.mockReturnValue(summaries(M1, M2));
    const { messagesList } = await import("../commands/messages.js");
    await messagesList({ limit: "1" });

    const lines = mockStdout.mock.calls.map((c: unknown[]) => c[0]);
    expect(lines).toHaveLength(1);
    expect(lines[0]).toContain("msg_1");
  });

  it("exits USAGE (2) on a bad --direction or --limit", async () => {
    const { messagesList } = await import("../commands/messages.js");
    await expect(messagesList({ direction: "sideways" })).rejects.toThrow("process.exit");
    expect(mockExit).toHaveBeenCalledWith(2);

    mockExit.mockClear();
    await expect(messagesList({ limit: "zero" })).rejects.toThrow("process.exit");
    expect(mockExit).toHaveBeenCalledWith(2);
  });

  it("get --text prefers parsed text over raw body text", async () => {
    mockGet.mockResolvedValue({
      id: "msg_1",
      parsed: { text: "just the reply" },
      body: { text: "just the reply\n> quoted history" },
    });
    const { messagesGet } = await import("../commands/messages.js");
    await messagesGet("msg_1", { text: true });

    expect(mockGet).toHaveBeenCalledWith("bot@agents.e2a.dev", "msg_1");
    expect(mockStdout).toHaveBeenCalledWith("just the reply\n");
  });

  it("get --text falls back to body text, then empty", async () => {
    mockGet.mockResolvedValue({ id: "msg_1", body: { text: "raw body" } });
    const { messagesGet } = await import("../commands/messages.js");
    await messagesGet("msg_1", { text: true });
    expect(mockStdout).toHaveBeenCalledWith("raw body\n");

    mockGet.mockResolvedValue({ id: "msg_2" });
    await messagesGet("msg_2", { text: true });
    expect(mockStdout).toHaveBeenCalledWith("\n");
  });

  it("get --text respects an intentionally-empty parsed result (all-quoted reply)", async () => {
    // parsed.text === "" means the parser ran and the reply was ALL quoted
    // history — must NOT fall through to the unstripped raw body.
    mockGet.mockResolvedValue({
      id: "msg_q",
      parsed: { text: "" },
      body: { text: "> the entire quoted thread" },
    });
    const { messagesGet } = await import("../commands/messages.js");
    await messagesGet("msg_q", { text: true });
    expect(mockStdout).toHaveBeenCalledWith("\n");
  });

  it("get emits full JSON without --text", async () => {
    const full = {
      id: "msg_1",
      subject: "s",
      conversationId: "conv-1",
      threadId: "thr_0123456789abcdef0123456789abcdef",
    };
    mockGet.mockResolvedValue(full);
    const { messagesGet } = await import("../commands/messages.js");
    await messagesGet("msg_1", {});

    expect(mockStdout).toHaveBeenCalledWith(JSON.stringify(full) + "\n");
    expect(JSON.parse(mockStdout.mock.calls[0][0] as string).threadId)
      .toBe("thr_0123456789abcdef0123456789abcdef");
  });

  it("get exits USAGE (2) without a message id", async () => {
    const { messagesGet } = await import("../commands/messages.js");
    await expect(messagesGet(undefined, {})).rejects.toThrow("process.exit");
    expect(mockExit).toHaveBeenCalledWith(2);
  });

  it("get exits USAGE (2) when --text and --json are combined", async () => {
    const { messagesGet } = await import("../commands/messages.js");
    await expect(messagesGet("msg_1", { text: true, json: true })).rejects.toThrow(
      "process.exit",
    );
    expect(mockExit).toHaveBeenCalledWith(2);
  });

  it("lifecycle prints the canonical page and forwards agent, cursor, and limit", async () => {
    const page = {
      items: [{
        id: "mlt_1",
        messageId: "msg_1",
        direction: "outbound",
        stage: "submission",
        outcome: "accepted",
        reasonCode: "submission.upstream_accepted",
        retryable: false,
        evidence: { response: "250 queued" },
        correlationIds: { provider_message_id: "provider_1" },
        occurredAt: new Date("2026-07-21T12:00:00Z"),
        reconstructed: false,
      }],
      nextCursor: "cursor_2",
    };
    mockGetLifecycle.mockResolvedValue(page);
    const { messagesLifecycle } = await import("../commands/messages.js");

    await messagesLifecycle("msg_1", {
      agent: "other@example.com",
      cursor: "cursor_1",
      limit: "25",
      json: true,
    });

    expect(mockGetLifecycle).toHaveBeenCalledWith("bot@agents.e2a.dev", "msg_1", {
      cursor: "cursor_1",
      limit: 25,
    });
    expect(mockStdout).toHaveBeenCalledWith(JSON.stringify(page) + "\n");
  });

  it("lifecycle exits USAGE (2) without an id or with a limit outside 1..100", async () => {
    const { messagesLifecycle } = await import("../commands/messages.js");
    for (const [id, limit] of [[undefined, undefined], ["msg_1", "0"], ["msg_1", "101"], ["msg_1", "1.5"]] as const) {
      mockExit.mockClear();
      await expect(messagesLifecycle(id, { limit })).rejects.toThrow("process.exit");
      expect(mockExit).toHaveBeenCalledWith(2);
    }
    expect(mockGetLifecycle).not.toHaveBeenCalled();
  });
});
