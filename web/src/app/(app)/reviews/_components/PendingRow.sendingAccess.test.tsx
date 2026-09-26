// Composer recipient-preflight + 403 handling for the external sending
// access restriction (beta). Separate from PendingRow.test.tsx to keep that
// file's fixtures untouched; this file adds the extra GET /v1/agents,
// /v1/domains, and /v1/account fetches the preflight needs.

import { render, screen, within } from "../../../../test-utils/swr";
import userEvent from "@testing-library/user-event";
import { PendingRow } from "./PendingRow";
import type { PendingMessageSummary } from "../../../components/types";

const AGENT = "support@acme.dev";
const summary: PendingMessageSummary = {
  id: "msg_1",
  agent_email: AGENT,
  direction: "outbound",
  subject: "Re: refund",
  to: ["customer@bigco.example"],
  status: "pending_review",
  created_at: new Date(Date.now() - 60_000).toISOString(),
};

const detailWire = {
  id: "msg_1",
  from: AGENT,
  to: ["customer@bigco.example"],
  cc: [],
  recipient: "customer@bigco.example",
  subject: "Re: refund",
  conversation_id: "conv_1",
  review_status: "pending_review",
  created_at: summary.created_at,
  body: { text: "Hello, your refund is on the way.", html: "" },
};

const restrictedAccount = {
  user: { id: "u1", email: "owner@acme.dev" },
  scope: "account",
  plan_code: "free",
  limits: { max_agents: 3, max_domains: 1, max_messages_month: 3000, max_storage_bytes: 1_000_000 },
  usage: { agents: 1, domains: 0, messages_month: 0, storage_bytes: 0 },
  upgrade_url: "",
  sending_access: {
    enforcement_applies: true,
    shared_external_approved: false,
    paid_external_sending_entitled: false,
    owner_recipient_verified: true,
  },
};

const notRestrictedAccount = {
  ...restrictedAccount,
  sending_access: { ...restrictedAccount.sending_access, enforcement_applies: false },
};

const mockFetch = jest.fn();
beforeEach(() => {
  mockFetch.mockReset();
  global.fetch = mockFetch as unknown as typeof fetch;
});

const detailURL = "/v1/reviews/msg_1";

type Stage = {
  account?: unknown;
  domains?: unknown[];
  approve?: { status: number; body: unknown };
};

function stage({ account = restrictedAccount, domains = [], approve }: Stage = {}) {
  mockFetch.mockImplementation((url: string, init?: { method?: string }) => {
    if (url === `${detailURL}/approve` && init?.method === "POST") {
      const r = approve ?? { status: 200, body: { status: "sent", message_id: "msg_1" } };
      return Promise.resolve({
        ok: r.status >= 200 && r.status < 300,
        status: r.status,
        text: () => Promise.resolve(JSON.stringify(r.body)),
      });
    }
    if (url === detailURL) {
      return Promise.resolve({
        ok: true,
        status: 200,
        text: () => Promise.resolve(JSON.stringify(detailWire)),
      });
    }
    if (url === "/v1/agents") {
      return Promise.resolve({
        ok: true,
        status: 200,
        text: () => Promise.resolve(JSON.stringify({ items: [{ email: AGENT }] })),
      });
    }
    if (url === "/v1/domains") {
      return Promise.resolve({
        ok: true,
        status: 200,
        text: () => Promise.resolve(JSON.stringify({ items: domains })),
      });
    }
    if (url === "/v1/account") {
      return Promise.resolve({
        ok: true,
        status: 200,
        text: () => Promise.resolve(JSON.stringify(account)),
      });
    }
    return Promise.resolve({ ok: false, status: 404, text: () => Promise.resolve("nf") });
  });
}

describe("PendingRow — external sending access preflight", () => {
  it("flags a recipient outside the account without blocking approve", async () => {
    stage();
    render(<PendingRow summary={summary} expanded onToggle={() => {}} onResolved={() => {}} />);

    const warning = await screen.findByRole("alert");
    expect(warning).toHaveTextContent("External sending is restricted for this account");
    expect(warning).toHaveTextContent("customer@bigco.example");
    expect(screen.getByRole("link", { name: "Request approval" })).toHaveAttribute(
      "href",
      "/sending-access",
    );
    // Guidance only — the approve action stays enabled.
    expect(screen.getByRole("button", { name: "Approve & send" })).not.toBeDisabled();
  });

  it("does not warn once the agent's own domain has a verified sending identity", async () => {
    stage({
      domains: [
        {
          domain: "acme.dev",
          verified: true,
          capabilities: { inbound: "verified", outbound: "verified" },
        },
      ],
    });
    render(<PendingRow summary={summary} expanded onToggle={() => {}} onResolved={() => {}} />);

    await screen.findByText("Hello, your refund is on the way.");
    expect(screen.queryByText(/External sending is restricted/)).not.toBeInTheDocument();
  });

  it("does not warn when the account isn't restricted", async () => {
    stage({ account: notRestrictedAccount });
    render(<PendingRow summary={summary} expanded onToggle={() => {}} onResolved={() => {}} />);

    await screen.findByText("Hello, your refund is on the way.");
    expect(screen.queryByText(/External sending is restricted/)).not.toBeInTheDocument();
  });

  it("renders the server's external_sending_not_enabled 403 with a Request approval link", async () => {
    stage({
      approve: {
        status: 403,
        body: {
          error: {
            code: "external_sending_not_enabled",
            message: "This account may not send to one or more recipients.",
            details: {
              allowed_recipients: ["verified_owner_email", "same_account_agents"],
              recovery_url: "/sending-access?ref=send",
            },
          },
        },
      },
    });
    render(<PendingRow summary={summary} expanded onToggle={() => {}} onResolved={() => {}} />);
    await screen.findByText("Hello, your refund is on the way.");

    await userEvent.click(screen.getByRole("button", { name: "Approve & send" }));

    const errorText = await screen.findByText(
      "This account may not send to one or more recipients.",
    );
    // The error's own Request approval link uses the server's recovery_url;
    // the (still-present) preflight warning above it links to the plain
    // page, so assert on the link inside the error paragraph specifically.
    const errorParagraph = errorText.closest("p");
    expect(errorParagraph).not.toBeNull();
    const { getByRole } = within(errorParagraph as HTMLElement);
    expect(getByRole("link", { name: "Request approval" })).toHaveAttribute(
      "href",
      "/sending-access?ref=send",
    );
  });
});
