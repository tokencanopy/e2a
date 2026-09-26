import { render, screen, waitFor } from "../../../test-utils/swr";
import userEvent from "@testing-library/user-event";
import SendingAccessPage from "./page";

const mockFetch = jest.fn();
beforeEach(() => {
  mockFetch.mockReset();
  global.fetch = mockFetch as unknown as typeof fetch;
});

const restrictedAccount = {
  user: { id: "u1", email: "owner@example.test" },
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

const eligibleAccount = {
  ...restrictedAccount,
  sending_access: { ...restrictedAccount.sending_access, shared_external_approved: true },
};

const notFoundRequest = { status: 404, body: { error: { code: "not_found", message: "no request on file" } } };

function requestView(state: string) {
  return {
    status: 200,
    body: {
      id: "sar_1",
      state,
      use_case: "Sending order confirmations to our customers.",
      recipients: "Our own customers who signed up",
      expected_daily_volume: 250,
      created_at: "2026-01-01T00:00:00Z",
      ...(state !== "pending" ? { decided_at: "2026-01-02T00:00:00Z" } : {}),
    },
  };
}

type Staged = {
  account: unknown;
  requestGet: { status: number; body: unknown };
  requestPost?: { status: number; body: unknown };
};

function stage({ account, requestGet, requestPost }: Staged) {
  mockFetch.mockImplementation((url: string, init?: RequestInit) => {
    if (url === "/v1/account") {
      return Promise.resolve({ ok: true, status: 200, text: async () => JSON.stringify(account) });
    }
    if (url === "/v1/account/sending-access/request") {
      const r = init?.method === "POST" ? requestPost ?? requestGet : requestGet;
      return Promise.resolve({
        ok: r.status >= 200 && r.status < 300,
        status: r.status,
        text: async () => JSON.stringify(r.body),
      });
    }
    return Promise.resolve({ ok: false, status: 404, text: async () => "not found" });
  });
}

async function fillAndSubmit() {
  await userEvent.type(
    screen.getByLabelText("What are you building?"),
    "Sending order confirmations to our customers.",
  );
  await userEvent.type(
    screen.getByLabelText("Who will you email?"),
    "Our own customers who signed up",
  );
  await userEvent.type(screen.getByLabelText("Expected daily volume"), "250");
  await userEvent.click(screen.getByRole("button", { name: "Submit request" }));
}

describe("/sending-access", () => {
  it("shows the request form when restricted with no prior request", async () => {
    stage({ account: restrictedAccount, requestGet: notFoundRequest });
    render(<SendingAccessPage />);

    expect(await screen.findByRole("button", { name: "Submit request" })).toBeInTheDocument();
    expect(screen.queryByText(/under review/)).not.toBeInTheDocument();
    expect(screen.queryByText("No approval needed")).not.toBeInTheDocument();
  });

  it("shows the under-review state and hides the form for a pending request", async () => {
    stage({ account: restrictedAccount, requestGet: requestView("pending") });
    render(<SendingAccessPage />);

    expect(await screen.findByText("Your request is under review")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Submit request" })).not.toBeInTheDocument();
  });

  it("shows eligibility and the approved request, keeping history visible with no form", async () => {
    stage({ account: eligibleAccount, requestGet: requestView("approved") });
    render(<SendingAccessPage />);

    expect(await screen.findByText("No approval needed")).toBeInTheDocument();
    expect(screen.getByText("Operator-approved external sending")).toBeInTheDocument();
    expect(screen.getByText("Request approved")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Submit request" })).not.toBeInTheDocument();
  });

  it("offers a support appeal and still allows filing a new request when declined", async () => {
    stage({ account: restrictedAccount, requestGet: requestView("declined") });
    render(<SendingAccessPage />);

    expect(await screen.findByText("Request declined")).toBeInTheDocument();
    const appeal = screen.getByRole("link", { name: "Contact support to appeal" });
    expect(appeal).toHaveAttribute("href", "/feedback");
    expect(screen.getByRole("button", { name: "Submit request" })).toBeInTheDocument();
  });

  it("submits a new request (201) and shows the under-review state", async () => {
    stage({
      account: restrictedAccount,
      requestGet: notFoundRequest,
      requestPost: requestView("pending"),
    });
    render(<SendingAccessPage />);
    await screen.findByRole("button", { name: "Submit request" });

    await fillAndSubmit();

    expect(await screen.findByText("Your request is under review")).toBeInTheDocument();
  });

  it("treats the idempotent 200 (existing pending request) the same as a fresh 201", async () => {
    stage({
      account: restrictedAccount,
      requestGet: notFoundRequest,
      requestPost: { status: 200, body: requestView("pending").body },
    });
    render(<SendingAccessPage />);
    await screen.findByRole("button", { name: "Submit request" });

    await fillAndSubmit();

    expect(await screen.findByText("Your request is under review")).toBeInTheDocument();
  });

  it("surfaces a 429 rate-limit response distinctly", async () => {
    stage({
      account: restrictedAccount,
      requestGet: notFoundRequest,
      requestPost: {
        status: 429,
        body: { error: { code: "rate_limited", message: "too many requests" } },
      },
    });
    render(<SendingAccessPage />);
    await screen.findByRole("button", { name: "Submit request" });

    await fillAndSubmit();

    await waitFor(() =>
      expect(screen.getByText(/reached the limit of 3 requests per 30 days/)).toBeInTheDocument(),
    );
  });

  it("surfaces a 422 validation error from the server", async () => {
    stage({
      account: restrictedAccount,
      requestGet: notFoundRequest,
      requestPost: {
        status: 422,
        body: {
          error: {
            code: "invalid_request",
            message: "expected_daily_volume must be between 1 and 1000000",
          },
        },
      },
    });
    render(<SendingAccessPage />);
    await screen.findByRole("button", { name: "Submit request" });

    await fillAndSubmit();

    expect(
      await screen.findByText("expected_daily_volume must be between 1 and 1000000"),
    ).toBeInTheDocument();
  });
});
