import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";

const mockSend = vi.fn();
const mockReply = vi.fn();

vi.mock("../sdk.js", () => ({
  createClient: vi.fn(() => ({ messages: { send: mockSend, reply: mockReply } })),
  requireAgentEmail: vi.fn((override?: string) => override || "bot@agents.e2a.dev"),
}));

const mockReadFileSync = vi.fn();
vi.mock("node:fs", () => ({
  readFileSync: (...args: unknown[]) => mockReadFileSync(...args),
}));

describe("send/reply commands", () => {
  let mockStdout: ReturnType<typeof vi.spyOn>;
  let mockStderr: ReturnType<typeof vi.spyOn>;
  let mockExit: ReturnType<typeof vi.spyOn>;

  beforeEach(() => {
    mockStdout = vi.spyOn(process.stdout, "write").mockImplementation(() => true);
    mockStderr = vi.spyOn(process.stderr, "write").mockImplementation(() => true);
    mockExit = vi.spyOn(process, "exit").mockImplementation(() => {
      throw new Error("process.exit");
    });
  });

  afterEach(() => {
    mockStdout.mockRestore();
    mockStderr.mockRestore();
    mockExit.mockRestore();
    vi.clearAllMocks();
    // The held path sets process.exitCode (flush-safe) instead of exiting;
    // reset it so a passing suite doesn't report a nonzero exit.
    process.exitCode = 0;
  });

  it("sends a plain-text message and prints the message id", async () => {
    mockSend.mockResolvedValue({ messageId: "msg_abc", status: "sent" });
    const { send } = await import("../commands/send.js");
    await send({ to: ["you@example.com"], subject: "hi", body: "hello" });

    expect(mockSend).toHaveBeenCalledWith(
      "bot@agents.e2a.dev",
      { to: ["you@example.com"], subject: "hi", text: "hello" },
      undefined,
    );
    expect(mockStdout).toHaveBeenCalledWith("msg_abc\n");
    expect(mockExit).not.toHaveBeenCalled();
  });

  it("sends a single --reply-to as a scalar string, not an array", async () => {
    mockSend.mockResolvedValue({ messageId: "msg_rt1", status: "sent" });
    const { send } = await import("../commands/send.js");
    await send({
      to: ["you@example.com"],
      subject: "hi",
      body: "hello",
      replyTo: ["support@acme.com"],
    });
    expect(mockSend.mock.calls[0][1].replyTo).toBe("support@acme.com");
  });

  it("passes repeated --reply-to through as an address-list array", async () => {
    mockSend.mockResolvedValue({ messageId: "msg_rt", status: "sent" });
    const { send } = await import("../commands/send.js");
    await send({
      to: ["you@example.com"],
      subject: "hi",
      body: "hello",
      replyTo: ["support@acme.com", "owner@acme.com"],
    });
    expect(mockSend.mock.calls[0][1].replyTo).toEqual(["support@acme.com", "owner@acme.com"]);
  });

  it("omits reply_to when the --reply-to list is empty", async () => {
    mockSend.mockResolvedValue({ messageId: "msg_none", status: "sent" });
    const { send } = await import("../commands/send.js");
    await send({ to: ["you@example.com"], subject: "hi", body: "hello", replyTo: [] });
    expect(mockSend.mock.calls[0][1].replyTo).toBeUndefined();
  });

  it("passes repeated --cc and --bcc through as arrays", async () => {
    mockSend.mockResolvedValue({ messageId: "msg_cc", status: "sent" });
    const { send } = await import("../commands/send.js");
    await send({
      to: ["you@example.com"],
      subject: "hi",
      body: "hello",
      cc: ["cc1@example.com", "cc2@example.com"],
      bcc: ["audit@example.com"],
    });
    const call = mockSend.mock.calls[0][1];
    expect(call.cc).toEqual(["cc1@example.com", "cc2@example.com"]);
    expect(call.bcc).toEqual(["audit@example.com"]);
  });

  it("omits cc and bcc when their lists are empty", async () => {
    mockSend.mockResolvedValue({ messageId: "msg_nocc", status: "sent" });
    const { send } = await import("../commands/send.js");
    await send({ to: ["you@example.com"], subject: "hi", body: "hello", cc: [], bcc: [] });
    const call = mockSend.mock.calls[0][1];
    expect(call.cc).toBeUndefined();
    expect(call.bcc).toBeUndefined();
  });

  it("accepts exactly 50 recipients combined across --to, --cc, and --bcc", async () => {
    mockSend.mockResolvedValue({ messageId: "msg_50", status: "sent" });
    const { send } = await import("../commands/send.js");
    const cc = Array.from({ length: 24 }, (_, i) => `cc${i}@example.com`);
    const bcc = Array.from({ length: 25 }, (_, i) => `bcc${i}@example.com`);
    await send({ to: ["you@example.com"], subject: "hi", body: "hello", cc, bcc });
    expect(mockSend).toHaveBeenCalled();
    expect(mockExit).not.toHaveBeenCalled();
  });

  it("exits USAGE (2) when --to, --cc, and --bcc combined exceed 50 recipients", async () => {
    const { send } = await import("../commands/send.js");
    const cc = Array.from({ length: 25 }, (_, i) => `cc${i}@example.com`);
    const bcc = Array.from({ length: 25 }, (_, i) => `bcc${i}@example.com`);
    await expect(
      send({ to: ["you@example.com"], subject: "hi", body: "hello", cc, bcc }),
    ).rejects.toThrow("process.exit");
    expect(mockExit).toHaveBeenCalledWith(2);
    expect(mockSend).not.toHaveBeenCalled();
  });

  it("threads --idempotency-key through to the SDK request options", async () => {
    mockSend.mockResolvedValue({ messageId: "msg_i", status: "sent" });
    const { send } = await import("../commands/send.js");
    await send({
      to: ["you@example.com"],
      subject: "s",
      body: "b",
      idempotencyKey: "evt-42",
    });

    expect(mockSend.mock.calls[0][2]).toEqual({ idempotencyKey: "evt-42" });
  });

  it("treats an unknown status as non-retryable without calling it a review hold", async () => {
    mockSend.mockResolvedValue({ messageId: "msg_u", status: "some_future_hold" });
    const { send } = await import("../commands/send.js");
    await send({ to: ["you@example.com"], subject: "s", body: "b" });

    expect(mockStderr).toHaveBeenCalledWith(expect.stringContaining("some_future_hold"));
    expect(mockStderr).toHaveBeenCalledWith(expect.stringContaining("do NOT retry"));
    expect(mockStderr).toHaveBeenCalledWith(expect.stringContaining("msg_u"));
    expect(process.exitCode).toBe(7);
  });

  it("reports a failed status as terminal and non-retryable", async () => {
    mockSend.mockResolvedValue({ messageId: "msg_failed", status: "failed" });
    const { send } = await import("../commands/send.js");
    await send({ to: ["you@example.com"], subject: "s", body: "b" });

    expect(mockStderr).toHaveBeenCalledWith(expect.stringContaining("terminal status \"failed\""));
    expect(mockStderr).toHaveBeenCalledWith(expect.stringContaining("do NOT retry"));
    expect(mockStderr).toHaveBeenCalledWith(expect.stringContaining("msg_failed"));
    expect(mockStderr).not.toHaveBeenCalledWith(expect.stringContaining("unrecognized"));
    expect(process.exitCode).toBe(7);
  });

  it("exits OK when an async send is durably accepted", async () => {
    mockSend.mockResolvedValue({ messageId: "msg_accepted", status: "accepted" });
    const { send } = await import("../commands/send.js");

    await send({ to: ["you@example.com"], subject: "hi", body: "hello" });

    expect(mockStdout).toHaveBeenCalledWith("msg_accepted\n");
    expect(mockStderr).not.toHaveBeenCalled();
    expect(process.exitCode).toBe(0);
  });

  it("sets exit code HELD (3) with a warning when the send is pending_review", async () => {
    mockSend.mockResolvedValue({ messageId: "msg_held", status: "pending_review" });
    const { send } = await import("../commands/send.js");

    await send({ to: ["you@example.com"], subject: "hi", body: "hello" });

    // The id is still printed (the message exists, parked in the queue) …
    expect(mockStdout).toHaveBeenCalledWith("msg_held\n");
    // … and the held contract fires via exitCode (not process.exit, which
    // could truncate the piped stdout carrying the id).
    expect(mockStderr).toHaveBeenCalledWith(expect.stringContaining("pending_review"));
    expect(mockExit).not.toHaveBeenCalled();
    expect(process.exitCode).toBe(3);
  });

  it("derives a text fallback from --html-file when no --body is given", async () => {
    mockReadFileSync.mockReturnValue("<p>Status: <b>green</b></p>");
    mockSend.mockResolvedValue({ messageId: "msg_h", status: "sent" });
    const { send } = await import("../commands/send.js");
    await send({ to: ["you@example.com"], subject: "s", htmlFile: "/tmp/u.html" });

    const call = mockSend.mock.calls[0][1];
    expect(call.html).toBe("<p>Status: <b>green</b></p>");
    expect(call.text).toBe("Status: green");
  });

  it("passes conversation id through", async () => {
    mockSend.mockResolvedValue({ messageId: "msg_c", status: "sent" });
    const { send } = await import("../commands/send.js");
    await send({
      to: ["you@example.com"],
      subject: "s",
      body: "b",
      conversationId: "conv-1",
    });

    expect(mockSend.mock.calls[0][1].conversationId).toBe("conv-1");
  });

  it("parses --send-at into a Date, exits OK, and notes the scheduled time", async () => {
    const at = "2026-08-01T09:00:00Z";
    mockSend.mockResolvedValue({
      messageId: "msg_sched",
      status: "scheduled",
      scheduledAt: new Date(at),
    });
    const { send } = await import("../commands/send.js");
    await send({ to: ["you@example.com"], subject: "s", body: "b", sendAt: at });

    const bodyArg = mockSend.mock.calls[0][1];
    expect(bodyArg.sendAt).toBeInstanceOf(Date);
    expect((bodyArg.sendAt as Date).toISOString()).toBe(new Date(at).toISOString());
    // Scheduled is a successful, intended acceptance: id printed, exit 0, note on stderr.
    expect(mockStdout).toHaveBeenCalledWith("msg_sched\n");
    expect(mockStderr).toHaveBeenCalledWith(expect.stringContaining("scheduled"));
    expect(mockStderr).toHaveBeenCalledWith(expect.stringContaining("restoring at or after"));
    expect(mockStderr).toHaveBeenCalledWith(expect.stringContaining("leaves the send canceled"));
    // The note renders the instant via toISOString() (millisecond precision).
    expect(mockStderr).toHaveBeenCalledWith(expect.stringContaining(new Date(at).toISOString()));
    expect(process.exitCode).toBe(0);
  });

  it("exits USAGE (2) when --send-at is not a valid date-time", async () => {
    const { send } = await import("../commands/send.js");
    await expect(
      send({ to: ["you@example.com"], subject: "s", body: "b", sendAt: "not-a-date" }),
    ).rejects.toThrow("process.exit");
    expect(mockExit).toHaveBeenCalledWith(2);
    expect(mockSend).not.toHaveBeenCalled();
  });

  it("exits USAGE (2) when --to or --subject is missing", async () => {
    const { send } = await import("../commands/send.js");
    await expect(send({ to: [], subject: "s", body: "b" })).rejects.toThrow("process.exit");
    expect(mockExit).toHaveBeenCalledWith(2);
  });

  it("exits USAGE (2) when no body source is given", async () => {
    const { send } = await import("../commands/send.js");
    await expect(send({ to: ["a@b.c"], subject: "s" })).rejects.toThrow("process.exit");
    expect(mockExit).toHaveBeenCalledWith(2);
  });

  it("exits USAGE (2) when --html-file is unreadable", async () => {
    mockReadFileSync.mockImplementation(() => {
      throw new Error("ENOENT");
    });
    const { send } = await import("../commands/send.js");
    await expect(
      send({ to: ["a@b.c"], subject: "s", htmlFile: "/nope.html" }),
    ).rejects.toThrow("process.exit");
    expect(mockExit).toHaveBeenCalledWith(2);
    expect(mockStderr).toHaveBeenCalledWith(expect.stringContaining("/nope.html"));
  });

  it("replies in-thread and honors the held contract", async () => {
    mockReply.mockResolvedValue({ messageId: "msg_r", status: "pending_review" });
    const { reply } = await import("../commands/send.js");

    await reply("msg_orig", { body: "answer" });
    expect(mockReply).toHaveBeenCalledWith(
      "bot@agents.e2a.dev",
      "msg_orig",
      { text: "answer" },
      undefined,
    );
    expect(process.exitCode).toBe(3);
  });

  it("passes --cc and --bcc through on reply, with no combined-recipient cap", async () => {
    mockReply.mockResolvedValue({ messageId: "msg_rcc", status: "sent" });
    const { reply } = await import("../commands/send.js");

    await reply("msg_orig", {
      body: "answer",
      cc: ["cc1@example.com"],
      bcc: Array.from({ length: 60 }, (_, i) => `bcc${i}@example.com`),
    });
    const call = mockReply.mock.calls[0][2];
    expect(call.cc).toEqual(["cc1@example.com"]);
    expect(call.bcc).toHaveLength(60);
    expect(mockExit).not.toHaveBeenCalled();
  });

  it("exits OK when an async reply is durably accepted", async () => {
    mockReply.mockResolvedValue({ messageId: "msg_reply_accepted", status: "accepted" });
    const { reply } = await import("../commands/send.js");

    await reply("msg_orig", { body: "answer" });

    expect(mockStdout).toHaveBeenCalledWith("msg_reply_accepted\n");
    expect(mockStderr).not.toHaveBeenCalled();
    expect(process.exitCode).toBe(0);
  });

  it("passes --quote-history through to the SDK reply call", async () => {
    mockReply.mockResolvedValue({ messageId: "msg_q", status: "sent" });
    const { reply } = await import("../commands/send.js");

    await reply("msg_orig", { body: "answer", quoteHistory: true });

    expect(mockReply.mock.calls[0][2].quoteHistory).toBe(true);
    // A sent reply never sets a failure code; exitCode may be untouched
    // (undefined) when this test runs in isolation.
    expect(process.exitCode ?? 0).toBe(0);
  });

  it("omits quoteHistory from the reply body when the flag is not set", async () => {
    mockReply.mockResolvedValue({ messageId: "msg_nq", status: "sent" });
    const { reply } = await import("../commands/send.js");

    await reply("msg_orig", { body: "answer", quoteHistory: false });

    // Absent-or-false stays off the wire so the request matches the
    // documented server default exactly.
    expect(mockReply.mock.calls[0][2].quoteHistory).toBeUndefined();
  });

  it("sends markup-only HTML whose derived text fallback is empty", async () => {
    mockReadFileSync.mockReturnValue('<img src="cid:logo"><table><tr><td></td></tr></table>');
    mockSend.mockResolvedValue({ messageId: "msg_img", status: "sent" });
    const { send } = await import("../commands/send.js");
    await send({ to: ["you@example.com"], subject: "s", htmlFile: "/tmp/banner.html" });

    const call = mockSend.mock.calls[0][1];
    expect(call.text).toBe("");
    expect(call.html).toContain("<img");
    expect(mockExit).not.toHaveBeenCalled();
  });

  it("exits USAGE (2) when reply is missing the message id", async () => {
    const { reply } = await import("../commands/send.js");
    await expect(reply(undefined, { body: "answer" })).rejects.toThrow("process.exit");
    expect(mockExit).toHaveBeenCalledWith(2);
  });
});
