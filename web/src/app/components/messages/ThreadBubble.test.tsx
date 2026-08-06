// ThreadBubble body-selection precedence: a message's rich HTML (parsed.html)
// is rendered in the sandboxed EmailHtmlBody iframe; otherwise the plain
// parsed.text is shown as escaped text. Pins the fallback order so a regression
// can't silently drop back to rendering raw MIME.

import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { ThreadBubble } from "./ThreadBubble";
import { getMessageDetailWire } from "../onboarding/api";
import { getMessageLifecycle } from "../../../lib/messageLifecycle";
import {
  invalidateAgentMessages,
  invalidateAgentUnread,
} from "../../../lib/swrKeys";
import type { MessageSummary } from "../types";

// The fetcher returns the RAW wire — the bubble projects it — so these
// mocks resolve wire shapes. The projector itself stays real: mocking it
// would let a projection regression pass unnoticed.
jest.mock("../onboarding/api", () => ({
  ...jest.requireActual("../onboarding/api"),
  getMessageDetailWire: jest.fn(),
}));
jest.mock("../../../lib/swrKeys", () => ({
  ...jest.requireActual("../../../lib/swrKeys"),
  invalidateAgentMessages: jest.fn(),
  invalidateAgentUnread: jest.fn(),
}));
jest.mock("../../../lib/messageLifecycle", () => ({
  ...jest.requireActual("../../../lib/messageLifecycle"),
  getMessageLifecycle: jest.fn(),
}));
const mockGet = getMessageDetailWire as jest.MockedFunction<
  typeof getMessageDetailWire
>;
const mockInvalidateMessages =
  invalidateAgentMessages as jest.MockedFunction<typeof invalidateAgentMessages>;
const mockInvalidateUnread =
  invalidateAgentUnread as jest.MockedFunction<typeof invalidateAgentUnread>;
const mockGetLifecycle = getMessageLifecycle as jest.MockedFunction<
  typeof getMessageLifecycle
>;

// Each test uses a distinct id: useSWR keys the body cache by id and
// that cache is process-global, so reusing an id would leak one test's body
// into the next.
function msg(id: string): MessageSummary {
  return {
    id: id,
    direction: "inbound",
    from: "james@x.com",
    to: ["support@acme.dev"],
    recipient: "support@acme.dev",
    subject: "Hi",
    status: "",
    created_at: new Date().toISOString(),
  };
}
function inbound(wire: Record<string, unknown>) {
  mockGet.mockResolvedValue(wire as never);
}

// Outbound bodies reach the bubble through projectPending, which reads
// `body.text` off the wire (not a pre-projected `body_text`).
function outbound(text: string) {
  mockGet.mockResolvedValue({ body: { text, html: "" } } as never);
}

afterEach(() => {
  mockGet.mockReset();
  mockInvalidateMessages.mockReset();
  mockInvalidateUnread.mockReset();
  mockGetLifecycle.mockReset();
});

describe("ThreadBubble canonical lifecycle", () => {
  it("loads and expands the beta lifecycle for the selected message", async () => {
    outbound("sent body");
    mockGetLifecycle.mockResolvedValue({
      items: [{
        id: "mlt_thread_sent",
        message_id: "msg_lifecycle_thread",
        direction: "outbound",
        recipient: "james@x.com",
        stage: "submission",
        outcome: "accepted",
        reason_code: "submission.upstream_accepted",
        retryable: false,
        evidence: {},
        correlation_ids: {},
        occurred_at: "2026-07-22T12:00:00Z",
        reconstructed: false,
      }],
      next_cursor: null,
    });
    const m: MessageSummary = {
      ...msg("msg_lifecycle_thread"),
      direction: "outbound",
      from: "support@acme.dev",
      recipient: "james@x.com",
      status: "sent",
    };

    render(<ThreadBubble message={m} agentEmail="support@acme.dev" />);

    const toggle = screen.getByRole("button", { name: /show lifecycle/i });
    expect(toggle).not.toHaveTextContent(/beta/i);
    expect(mockGetLifecycle).not.toHaveBeenCalled();
    fireEvent.click(toggle);

    expect(await screen.findByText("Handed off to delivery provider")).toBeInTheDocument();
    expect(mockGetLifecycle).toHaveBeenCalledWith(
      "support@acme.dev",
      "msg_lifecycle_thread",
      { limit: 100 },
    );
  });
});

describe("ThreadBubble body precedence", () => {
  it("renders parsed.html in the sandboxed iframe when present", async () => {
    inbound({ parsed: { text: "flat text", html: "<p>rich <b>html</b></p>" }, raw_message: "" });
    render(<ThreadBubble message={msg("msg_html")} agentEmail="support@acme.dev" />);
    await waitFor(() => {
      const frame = screen.getByTitle("Email body") as HTMLIFrameElement;
      expect(frame.getAttribute("srcdoc")).toContain("rich <b>html</b>");
    });
    // The flattened text is not also rendered as escaped page text.
    expect(screen.queryByText("flat text")).not.toBeInTheDocument();
  });

  it("falls back to parsed.text (no iframe) when there is no HTML part", async () => {
    inbound({ parsed: { text: "just the plain body" }, raw_message: "" });
    render(<ThreadBubble message={msg("msg_text")} agentEmail="support@acme.dev" />);
    await waitFor(() => {
      expect(screen.getByText("just the plain body")).toBeInTheDocument();
    });
    expect(screen.queryByTitle("Email body")).not.toBeInTheDocument();
  });
});

describe("ThreadBubble inbound sender identity", () => {
  // In a multi-party thread (e.g. the agent is Cc'd on mail between two other
  // parties), a message's From can differ from the thread's derived
  // counterparty. The bubble must identify each inbound message by its own
  // From — labeling it with the thread counterparty misattributes the mail.
  it("renders the message's own From, not the thread counterparty", async () => {
    inbound({ parsed: { text: "body" }, raw_message: "" });
    const m = { ...msg("msg_multiparty_sender"), from: "casey@sender.example" };

    render(<ThreadBubble message={m} agentEmail="support@acme.dev" />);

    await screen.findByText("body");
    // Header: name derived from the From address, plus the address itself.
    expect(screen.getByText("Casey")).toBeInTheDocument();
    expect(screen.getByText("casey@sender.example")).toBeInTheDocument();
    // Avatar initials come from the sender too.
    expect(screen.getByText("CA")).toBeInTheDocument();
  });
});

describe("ThreadBubble inbound authentication summary", () => {
  it("shows the DMARC-verified domain on an inbound summary", async () => {
    inbound({ parsed: { text: "body" }, raw_message: "" });
    const m = { ...msg("msg_dmarc_pass"), verified_domain: "example.com" };

    render(<ThreadBubble message={m} agentEmail="support@acme.dev" />);

    expect(screen.getByText("DMARC verified · example.com")).toBeInTheDocument();
  });

  it("warns when an inbound summary has no verified domain", async () => {
    inbound({ parsed: { text: "body" }, raw_message: "" });
    const m = { ...msg("msg_dmarc_not_verified"), verified_domain: null };

    render(<ThreadBubble message={m} agentEmail="support@acme.dev" />);

    expect(screen.getByText("DMARC not verified")).toBeInTheDocument();
  });
});

describe("ThreadBubble marks-read cache refresh", () => {
  // Opening a message body flips inbox_status unread → read on the backend.
  // The thread list (bold rows) and the Inboxes unread badge both cache the
  // stale state, so the bubble must revalidate them once the body loads.
  it("invalidates the thread list + unread badge after reading an unread inbound message", async () => {
    inbound({ parsed: { text: "body" }, raw_message: "" });
    const m = { ...msg("msg_unread_inbound"), read_status: "unread" };
    render(<ThreadBubble message={m} agentEmail="support@acme.dev" />);
    await waitFor(() => {
      expect(mockInvalidateMessages).toHaveBeenCalledWith("support@acme.dev");
      expect(mockInvalidateUnread).toHaveBeenCalledWith("support@acme.dev");
    });
  });

  it("does not invalidate when the inbound message was already read", async () => {
    inbound({ parsed: { text: "body" }, raw_message: "" });
    const m = { ...msg("msg_read_inbound"), read_status: "read" };
    render(<ThreadBubble message={m} agentEmail="support@acme.dev" />);
    // Wait for the body fetch to resolve so onSuccess has had its chance.
    await waitFor(() => expect(screen.getByText("body")).toBeInTheDocument());
    expect(mockInvalidateMessages).not.toHaveBeenCalled();
    expect(mockInvalidateUnread).not.toHaveBeenCalled();
  });

  it("does not invalidate for outbound messages", async () => {
    outbound("sent body");
    const m: MessageSummary = { ...msg("msg_outbound"), direction: "outbound", read_status: "" };
    render(<ThreadBubble message={m} agentEmail="support@acme.dev" />);
    await waitFor(() => expect(screen.getByText("sent body")).toBeInTheDocument());
    expect(mockInvalidateMessages).not.toHaveBeenCalled();
    expect(mockInvalidateUnread).not.toHaveBeenCalled();
  });
});

describe("ThreadBubble outbound delivery status", () => {
  it.each([
    ["accepted", "Queued"],
    ["sending", "Sending"],
    ["deferred", "Delayed"],
    ["sent", "Sent"],
    ["delivered", "Delivered"],
    ["failed", "Failed"],
    ["bounced", "Bounced"],
    ["complained", "Complaint"],
  ] as const)("renders %s as %s", async (status, label) => {
    const id = `msg_status_${status}`;
    outbound(`${status} body`);
    const m: MessageSummary = {
      ...msg(id),
      direction: "outbound",
      from: "support@acme.dev",
      to: ["james@x.com"],
      recipient: "james@x.com",
      status,
    };

    render(<ThreadBubble message={m} agentEmail="support@acme.dev" />);

    expect(await screen.findByText(label)).toHaveClass("shrink-0", "whitespace-nowrap");
  });

  it.each([
    ["pending_review", "Pending review"],
    ["review_rejected", "Rejected"],
  ] as const)("renders review status %s as %s", async (review_status, label) => {
    const id = `msg_review_${review_status}`;
    outbound(`${review_status} body`);
    const m: MessageSummary = {
      ...msg(id),
      direction: "outbound",
      from: "support@acme.dev",
      to: ["james@x.com"],
      recipient: "james@x.com",
      status: "",
      review_status,
    };

    render(<ThreadBubble message={m} agentEmail="support@acme.dev" />);

    expect(await screen.findByText(label)).toHaveClass("shrink-0", "whitespace-nowrap");
  });

  it("does not render an outbound delivery chip on an inbound message", async () => {
    inbound({ parsed: { text: "inbound body" }, raw_message: "" });
    const m = { ...msg("msg_status_inbound"), status: "failed" };

    render(<ThreadBubble message={m} agentEmail="support@acme.dev" />);

    await screen.findByText("inbound body");
    expect(screen.queryByText("Failed")).not.toBeInTheDocument();
  });
});
