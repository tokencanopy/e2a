import { render, screen, waitFor } from "../../../../test-utils/swr";
import userEvent from "@testing-library/user-event";
import { PendingRow } from "./PendingRow";
import type { PendingMessageSummary } from "../../../components/types";

jest.mock("../../../components/messages/MessageLifecycleTimeline", () => ({
  MessageLifecycleData: ({ email, messageId }: { email: string; messageId: string }) => (
    <div>lifecycle:{email}:{messageId}</div>
  ),
}));

const AGENT = "support@acme.dev";
const summary: PendingMessageSummary = {
  id: "msg_1",
  agent_email: AGENT,
  direction: "outbound",
  subject: "Re: refund",
  to: ["customer@bigco.com"],
  status: "pending_review",
  created_at: new Date(Date.now() - 60_000).toISOString(),
};

// MessageView wire shape the detail fetch returns; projectPending reads
// review_status + body.
const detailWire = {
  message_id: "msg_1",
  from: AGENT,
  to: ["customer@bigco.com"],
  cc: [],
  recipient: "customer@bigco.com",
  subject: "Re: refund",
  conversation_id: "conv_1",
  review_status: "pending_review",
  created_at: summary.created_at,
  body: { text: "Hello, your refund is on the way.", html: "" },
};

const mockFetch = jest.fn();
beforeEach(() => {
  mockFetch.mockReset();
  global.fetch = mockFetch as unknown as typeof fetch;
});

// Detail + approve/reject now go through the account-scoped /v1/reviews
// resource (id is globally unique; no agent address in the path).
const detailURL = `/v1/reviews/msg_1`;
function stage(overrides: Record<string, unknown> = {}) {
  mockFetch.mockImplementation((url: string, init?: { method?: string }) => {
    if (url === `${detailURL}/approve` && init?.method === "POST")
      return Promise.resolve({ ok: true, status: 200, text: () => Promise.resolve(JSON.stringify({ status: "sent", message_id: "msg_1" })) });
    if (url === `${detailURL}/reject` && init?.method === "POST")
      return Promise.resolve({ ok: true, status: 200, text: () => Promise.resolve(JSON.stringify({ status: "review_rejected", message_id: "msg_1" })) });
    if (url === detailURL)
      return Promise.resolve({ ok: true, status: 200, text: () => Promise.resolve(JSON.stringify({ ...detailWire, ...overrides })) });
    return Promise.resolve({ ok: false, status: 404, text: () => Promise.resolve("nf") });
  });
}

describe("PendingRow", () => {
  it("shows a gate hold explanation in the collapsed row", () => {
    const gate = {
      ...summary,
      hold_reason: {
        type: "gate",
        code: "sender_gate",
        summary: "This sender isn't allowed by the inbox policy.",
      },
    };
    render(<PendingRow summary={gate} expanded={false} onToggle={() => {}} onResolved={() => {}} />);
    expect(screen.getByText("This sender isn't allowed by the inbox policy.")).toBeInTheDocument();
  });

  it("shows a scheduled-send chip when a held draft carries a future send_at (#815)", () => {
    const scheduled = {
      ...summary,
      scheduled_at: new Date(Date.now() + 60 * 60_000).toISOString(),
    };
    render(<PendingRow summary={scheduled} expanded={false} onToggle={() => {}} onResolved={() => {}} />);
    expect(screen.getByText(/^Sends /)).toBeInTheDocument();
  });

  it("labels a lapsed schedule as sending on approval", () => {
    const lapsed = {
      ...summary,
      scheduled_at: new Date(Date.now() - 60_000).toISOString(),
    };
    render(<PendingRow summary={lapsed} expanded={false} onToggle={() => {}} onResolved={() => {}} />);
    expect(screen.getByText("Sends on approval")).toBeInTheDocument();
  });

  it("omits the scheduled chip when there is no schedule", () => {
    render(<PendingRow summary={summary} expanded={false} onToggle={() => {}} onResolved={() => {}} />);
    expect(screen.queryByText(/^Sends/)).not.toBeInTheDocument();
  });

  it("shows scan rationale on expansion and confidence only after disclosure", async () => {
    const user = userEvent.setup();
    const scan = {
      ...summary,
      hold_reason: {
        type: "scan",
        code: "outbound_scan",
        summary: "Content screening found a potential risk.",
      },
    };
    stage({
      hold_reason: {
        ...scan.hold_reason,
        category: "prompt_injection_direct",
        detail: "It asks the agent to ignore its instructions and wire funds.",
        confidence: 0.92,
      },
      protection: [{ source: "scan", detector: "gemini" }],
    });
    render(<PendingRow summary={scan} expanded onToggle={() => {}} onResolved={() => {}} />);
    expect(await screen.findByText(/ignore its instructions and wire funds/)).toBeInTheDocument();
    expect(screen.queryByText("confidence 0.92")).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: /Screening details/ }));
    expect(screen.getByText("confidence 0.92")).toBeInTheDocument();
    expect(screen.getByText("detector gemini")).toBeInTheDocument();
  });

  it("annotates direction: outbound shows inbox → recipient + Outbound", () => {
    stage();
    render(<PendingRow summary={summary} expanded={false} onToggle={() => {}} onResolved={() => {}} />);
    expect(screen.getByText("Outbound")).toBeInTheDocument();
    expect(screen.getByText(/support@acme\.dev → customer@bigco\.com/)).toBeInTheDocument();
  });

  it("annotates direction: inbound shows sender → inbox + Inbound", () => {
    const inbound = {
      ...summary,
      direction: "inbound" as const,
      from: "suspicious@spammy.biz",
      to: [AGENT],
    };
    render(<PendingRow summary={inbound} expanded={false} onToggle={() => {}} onResolved={() => {}} />);
    expect(screen.getByText("Inbound")).toBeInTheDocument();
    expect(screen.getByText(/suspicious@spammy\.biz → support@acme\.dev/)).toBeInTheDocument();
  });

  it("inbound hold: release/block actions, no editor", async () => {
    stage();
    const inbound = {
      ...summary,
      direction: "inbound" as const,
      from: "suspicious@spammy.biz",
      to: [AGENT],
    };
    render(<PendingRow summary={inbound} expanded onToggle={() => {}} onResolved={() => {}} />);
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Approve & release" })).toBeInTheDocument(),
    );
    // No draft editing for an inbound (incoming) message.
    expect(screen.queryByRole("button", { name: "Edit draft" })).not.toBeInTheDocument();
  });

  it("collapsed shows the summary; not the body", () => {
    stage();
    render(<PendingRow summary={summary} expanded={false} onToggle={() => {}} onResolved={() => {}} />);
    expect(screen.getByText("Re: refund")).toBeInTheDocument();
    expect(screen.queryByText(/your refund is on the way/)).not.toBeInTheDocument();
  });

  it("expanded fetches + renders the body read-first with an action bar", async () => {
    stage();
    render(<PendingRow summary={summary} expanded onToggle={() => {}} onResolved={() => {}} />);
    await waitFor(() => expect(screen.getByText(/your refund is on the way/)).toBeInTheDocument());
    expect(screen.getByRole("button", { name: "Approve & send" })).toBeInTheDocument();
    // Editor is opt-in — no Subject input until Edit draft.
    expect(screen.queryByText("Subject")).not.toBeInTheDocument();
  });

  it("renders an HTML body the way the recipient's inbox would (sandboxed iframe), not the (HTML body) placeholder", async () => {
    stage({ body: { text: "", html: "<p>Hello, please see the <b>attached update</b>.</p>" } });
    render(<PendingRow summary={summary} expanded onToggle={() => {}} onResolved={() => {}} />);
    await waitFor(() => {
      const frame = screen.getByTitle("Email body") as HTMLIFrameElement;
      expect(frame.getAttribute("srcdoc")).toContain("please see the <b>attached update</b>");
    });
    expect(screen.queryByText("(HTML body)")).not.toBeInTheDocument();
  });

  it("falls back to plain text (no iframe) when the draft has no HTML part", async () => {
    stage();
    render(<PendingRow summary={summary} expanded onToggle={() => {}} onResolved={() => {}} />);
    await waitFor(() => expect(screen.getByText(/your refund is on the way/)).toBeInTheDocument());
    expect(screen.queryByTitle("Email body")).not.toBeInTheDocument();
  });

  it("loads the lifecycle lazily beside Details", async () => {
    stage();
    const user = userEvent.setup();
    render(<PendingRow summary={summary} expanded onToggle={() => {}} onResolved={() => {}} />);
    await screen.findByText(/your refund is on the way/);

    expect(screen.queryByText(`lifecycle:${AGENT}:msg_1`)).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: /lifecycle/i }));
    expect(screen.getByText(`lifecycle:${AGENT}:msg_1`)).toBeInTheDocument();
  });

  it("approve POSTs to /approve and calls onResolved", async () => {
    stage();
    const onResolved = jest.fn();
    const user = userEvent.setup();
    render(<PendingRow summary={summary} expanded onToggle={() => {}} onResolved={onResolved} />);
    await waitFor(() => screen.getByRole("button", { name: "Approve & send" }));
    await user.click(screen.getByRole("button", { name: "Approve & send" }));
    await waitFor(() => expect(onResolved).toHaveBeenCalled());
    expect(mockFetch).toHaveBeenCalledWith(`${detailURL}/approve`, expect.objectContaining({ method: "POST" }));
  });

  it("Edit draft reveals the editor and approve sends only changed fields", async () => {
    stage();
    const user = userEvent.setup();
    render(<PendingRow summary={summary} expanded onToggle={() => {}} onResolved={() => {}} />);
    await waitFor(() => screen.getByRole("button", { name: "Edit draft" }));
    await user.click(screen.getByRole("button", { name: "Edit draft" }));
    const subject = await screen.findByDisplayValue("Re: refund");
    await user.clear(subject);
    await user.type(subject, "Re: refund (approved)");
    await user.click(screen.getByRole("button", { name: "Approve & send edited" }));
    await waitFor(() =>
      expect(mockFetch).toHaveBeenCalledWith(`${detailURL}/approve`, expect.objectContaining({ method: "POST" })),
    );
    const call = mockFetch.mock.calls.find((c) => c[0] === `${detailURL}/approve`);
    const body = JSON.parse(call![1].body as string);
    expect(body.subject).toBe("Re: refund (approved)"); // only the changed field
    expect(body).not.toHaveProperty("body"); // body untouched → omitted
  });

  it("Reject reveals a reason field and POSTs to /reject", async () => {
    stage();
    const onResolved = jest.fn();
    const user = userEvent.setup();
    render(<PendingRow summary={summary} expanded onToggle={() => {}} onResolved={onResolved} />);
    await waitFor(() => screen.getByRole("button", { name: /^Reject/ }));
    await user.click(screen.getByRole("button", { name: /^Reject/ }));
    await user.type(screen.getByPlaceholderText("reject reason (optional)"), "tone off");
    await user.click(screen.getByRole("button", { name: "Reject draft" }));
    await waitFor(() => expect(onResolved).toHaveBeenCalled());
    const call = mockFetch.mock.calls.find((c) => c[0] === `${detailURL}/reject`);
    expect(JSON.parse(call![1].body as string).reason).toBe("tone off");
  });
});

// Regression: approving a row whose detail came from the SWR cache must not
// blank the message.
//
// The form fields were seeded only in SWR's `onSuccess`, which does NOT fire
// when the value is served from cache — and the review queue now shares its
// per-message cache entry with inbox threads, so a warm entry is the common
// case, not a corner. With the fields left empty, `diffApproveEdits` saw ""
// against the real subject/body/recipients and emitted them as CHANGES;
// the server treats a present field as "use this value", including empty
// string, so approving would have sent a message with no subject, no body
// and no recipients.
describe("PendingRow approve from a warm cache", () => {
  it("sends no overrides when the reviewer never opened the editor", async () => {
    const { render: rawRender } = jest.requireActual("@testing-library/react");
    const { mutate } = jest.requireActual("swr");
    const { messageDetailKey } = jest.requireActual("../../../../lib/swrKeys");

    stage();
    // Pre-populate the shared entry, exactly as an inbox thread would have.
    await mutate(messageDetailKey("msg_1"), detailWire, { revalidate: false });

    const user = userEvent.setup();
    rawRender(
      <PendingRow summary={summary} expanded onToggle={() => {}} onResolved={() => {}} />,
    );

    await screen.findByText(/Hello, your refund is on the way\./);
    await user.click(screen.getByRole("button", { name: /approve & send/i }));

    const approveCall = mockFetch.mock.calls.find(
      ([url, init]: [string, { method?: string }]) =>
        url === `${detailURL}/approve` && init?.method === "POST",
    );
    expect(approveCall).toBeDefined();
    const body = JSON.parse((approveCall![1] as { body: string }).body || "{}");
    // No destructive overrides: the agent-authored draft goes out as written.
    expect(body.subject).toBeUndefined();
    expect(body.text).toBeUndefined();
    expect(body.to).toBeUndefined();
  });
});
