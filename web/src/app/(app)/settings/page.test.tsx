import { render, screen, fireEvent, waitFor } from "../../../test-utils/swr";
import SettingsPage from "./page";

// jsdom doesn't provide navigator.clipboard. The signing-secret Copy
// button calls writeText, so we install a jest mock once at module
// level so individual tests can assert on it.
const writeText = jest.fn(async () => {});
Object.assign(navigator, { clipboard: { writeText } });
beforeEach(() => writeText.mockClear());

// Mock next/link to plain anchors so we don't need a router in jsdom.
jest.mock("next/link", () => {
  return function MockLink({ href, children, ...rest }: { href: string; children: React.ReactNode; [k: string]: unknown }) {
    return <a href={href} {...rest}>{children}</a>;
  };
});

const signOut = jest.fn();
let mockAuth: {
  user: { id: string; email: string; name: string; created_at: string } | null;
  loading: boolean;
  signOut: jest.Mock;
};

jest.mock("../../components/AuthProvider", () => ({
  useAuth: () => mockAuth,
}));

beforeEach(() => {
  mockAuth = {
    user: {
      id: "usr_abc123",
      email: "alice@example.com",
      name: "Alice",
      created_at: "2026-04-01T10:00:00Z",
    },
    loading: false,
    signOut,
  };
  // Generic fetch stub for GET /v1/account and GET
  // /v1/account/sending-access/request (the sending-access status row's
  // two background reads on every mount) and anything else this suite
  // doesn't care about: an empty 200 body has no `sending_access` field,
  // so the row renders nothing and the rest of these tests are unaffected.
  // The delete-account tests and the dedicated sending-access-row tests
  // below override this with their own mock.
  global.fetch = jest.fn(async () => ({
    ok: true,
    status: 200,
    json: async () => ({}),
    text: async () => "{}",
  })) as unknown as typeof fetch;
});

describe("Settings — Profile section", () => {
  it("shows the user's name, email, ID, and member-since date", () => {
    render(<SettingsPage />);
    expect(screen.getByText("Alice")).toBeInTheDocument();
    expect(screen.getByText("alice@example.com")).toBeInTheDocument();
    expect(screen.getByText("usr_abc123")).toBeInTheDocument();
    // The exact format depends on locale but the year should always render.
    expect(screen.getByText(/2026/)).toBeInTheDocument();
  });

  it("renders nothing when user is null (defensive)", () => {
    mockAuth.user = null;
    const { container } = render(<SettingsPage />);
    expect(container.firstChild).toBeNull();
  });
});

describe("Settings — Export section", () => {
  it("links the Download export button at the API endpoint", () => {
    render(<SettingsPage />);
    const link = screen.getByRole("link", { name: /download export/i });
    expect(link).toHaveAttribute("href", "/v1/account/export");
  });
});

// Mirrors the mocking pattern in app/(app)/sending-access/page.test.tsx:
// route GET /v1/account and GET /v1/account/sending-access/request off the
// same fetch mock so the row's two background reads settle independently.
const restrictedSendingAccess = {
  enforcement_applies: true,
  shared_external_approved: false,
  paid_external_sending_entitled: false,
  owner_recipient_verified: true,
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

function stageSendingAccess(
  sendingAccess: Record<string, unknown> | undefined,
  requestGet: { status: number; body: unknown } = notFoundRequest,
) {
  global.fetch = jest.fn((url: string) => {
    if (url === "/v1/account") {
      const body = {
        user: { id: "usr_abc123", email: "alice@example.com" },
        scope: "account",
        plan_code: "free",
        limits: { max_agents: 3, max_domains: 1, max_messages_month: 3000, max_storage_bytes: 1_000_000 },
        usage: { agents: 1, domains: 0, messages_month: 0, storage_bytes: 0 },
        upgrade_url: "",
        ...(sendingAccess ? { sending_access: sendingAccess } : {}),
      };
      return Promise.resolve({ ok: true, status: 200, text: async () => JSON.stringify(body) });
    }
    if (url === "/v1/account/sending-access/request") {
      return Promise.resolve({
        ok: requestGet.status >= 200 && requestGet.status < 300,
        status: requestGet.status,
        text: async () => JSON.stringify(requestGet.body),
      });
    }
    return Promise.resolve({ ok: true, status: 200, text: async () => "{}" });
  }) as unknown as typeof fetch;
}

describe("Settings — Sending access row", () => {
  it("renders no row at all when the deployment has no sending_access object", async () => {
    stageSendingAccess(undefined);
    render(<SettingsPage />);

    // Let both background reads settle before asserting absence.
    await screen.findByText("Alice");
    await waitFor(() => expect(screen.queryByText(/External sending/)).not.toBeInTheDocument());
  });

  it("shows Restricted with no request on file", async () => {
    stageSendingAccess(restrictedSendingAccess, notFoundRequest);
    render(<SettingsPage />);

    const value = await screen.findByRole("link", { name: "Restricted" });
    expect(value).toHaveAttribute("href", "/sending-access");
    expect(screen.getByText("External sending")).toBeInTheDocument();
  });

  it("shows Restricted — request under review for a pending request", async () => {
    stageSendingAccess(restrictedSendingAccess, requestView("pending"));
    render(<SettingsPage />);

    expect(
      await screen.findByRole("link", { name: "Restricted — request under review" }),
    ).toHaveAttribute("href", "/sending-access");
  });

  it("shows Restricted — request declined for the latest declined request", async () => {
    stageSendingAccess(restrictedSendingAccess, requestView("declined"));
    render(<SettingsPage />);

    expect(
      await screen.findByRole("link", { name: "Restricted — request declined" }),
    ).toHaveAttribute("href", "/sending-access");
  });

  it("shows Enabled — operator approved once an operator grants access", async () => {
    stageSendingAccess(
      { ...restrictedSendingAccess, shared_external_approved: true },
      requestView("approved"),
    );
    render(<SettingsPage />);

    expect(
      await screen.findByRole("link", { name: "Enabled — operator approved" }),
    ).toHaveAttribute("href", "/sending-access");
  });

  it("shows Enabled — paid plan when the paid entitlement lifts the restriction", async () => {
    stageSendingAccess(
      { ...restrictedSendingAccess, paid_external_sending_entitled: true },
      notFoundRequest,
    );
    render(<SettingsPage />);

    expect(
      await screen.findByRole("link", { name: "Enabled — paid plan" }),
    ).toHaveAttribute("href", "/sending-access");
  });
});

const mockHardNavigate = jest.fn();
jest.mock("../../../lib/navigation", () => ({
  hardNavigate: (url: string) => mockHardNavigate(url),
}));
beforeEach(() => mockHardNavigate.mockClear());

function openDeleteFlow() {
  fireEvent.click(screen.getByRole("button", { name: /delete account/i }));
}

describe("Settings — Danger zone (delete account)", () => {
  it("describes the trash, not an immediate irreversible deletion", () => {
    render(<SettingsPage />);
    expect(screen.getByText(/moves your account to the trash/i)).toBeInTheDocument();
    expect(screen.getByText(/Restoring doesn.t bring them back/i)).toBeInTheDocument();
    expect(screen.getByText(/Custom domains are unverified/i)).toBeInTheDocument();
    expect(screen.getByText(/may then be held for a while/i)).toBeInTheDocument();
    expect(screen.queryByText(/irreversible/i)).not.toBeInTheDocument();
  });

  it("hides the confirm input until the user opens the flow", () => {
    render(<SettingsPage />);
    expect(screen.queryByPlaceholderText("DELETE")).not.toBeInTheDocument();
    openDeleteFlow();
    expect(screen.getByPlaceholderText("DELETE")).toBeInTheDocument();
    // Trash is the default choice.
    expect(screen.getByRole("radio", { name: /move to trash/i })).toBeChecked();
    expect(screen.getByRole("radio", { name: /erase permanently now/i })).not.toBeChecked();
  });

  it("disables the final delete button until confirmation matches", () => {
    render(<SettingsPage />);
    openDeleteFlow();
    const finalBtn = screen.getByRole("button", { name: /delete my account/i });
    expect(finalBtn).toBeDisabled();

    const input = screen.getByPlaceholderText("DELETE") as HTMLInputElement;
    fireEvent.change(input, { target: { value: "delete" } }); // wrong case
    expect(finalBtn).toBeDisabled();

    fireEvent.change(input, { target: { value: "DELETE" } });
    expect(finalBtn).toBeEnabled();
  });

  it("moves the account to the trash by default (no permanent flag) and redirects home", async () => {
    const fetchMock = jest.fn(async (_url: string) => ({
      ok: true,
      status: 200,
      text: async () => '{"deleted":true,"mode":"trash"}',
      json: async () => ({ deleted: true, mode: "trash" }),
    }));
    global.fetch = fetchMock as unknown as typeof fetch;

    render(<SettingsPage />);
    openDeleteFlow();
    fireEvent.change(screen.getByPlaceholderText("DELETE"), { target: { value: "DELETE" } });
    fireEvent.click(screen.getByRole("button", { name: /delete my account/i }));

    await waitFor(() => expect(mockHardNavigate).toHaveBeenCalledWith("/?account_deleted=1"));
    // Exactly one DELETE call — the sending-access status row's own
    // background reads (GET /v1/account, GET .../sending-access/request)
    // share this same mock and must not be confused with it.
    const deleteCalls = fetchMock.mock.calls.filter(([url]) => url === "/v1/account?confirm=DELETE");
    expect(deleteCalls).toHaveLength(1);
    expect(fetchMock).toHaveBeenCalledWith(
      "/v1/account?confirm=DELETE",
      expect.objectContaining({ method: "DELETE", credentials: "include" }),
    );
  });

  it("disables the buttons while the delete is in flight", async () => {
    global.fetch = jest.fn(() => new Promise(() => {})) as unknown as typeof fetch;
    render(<SettingsPage />);
    openDeleteFlow();
    fireEvent.change(screen.getByPlaceholderText("DELETE"), { target: { value: "DELETE" } });
    fireEvent.click(screen.getByRole("button", { name: /delete my account/i }));

    expect(await screen.findByRole("button", { name: /deleting…/i })).toBeDisabled();
    expect(screen.getByRole("button", { name: /cancel/i })).toBeDisabled();
  });

  it("erases permanently only after the separate acknowledgement, sending permanent=true", async () => {
    const fetchMock = jest.fn(async () => ({
      ok: true,
      status: 200,
      text: async () => '{"deleted":true,"mode":"permanent"}',
      json: async () => ({ deleted: true, mode: "permanent" }),
    }));
    global.fetch = fetchMock as unknown as typeof fetch;

    render(<SettingsPage />);
    openDeleteFlow();
    fireEvent.click(screen.getByRole("radio", { name: /erase permanently now/i }));
    fireEvent.change(screen.getByPlaceholderText("DELETE"), { target: { value: "DELETE" } });

    const eraseBtn = screen.getByRole("button", { name: /erase my account permanently/i });
    // Typed DELETE alone is not enough for the permanent path.
    expect(eraseBtn).toBeDisabled();
    fireEvent.click(screen.getByRole("checkbox", { name: /can.t be recovered/i }));
    expect(eraseBtn).toBeEnabled();
    fireEvent.click(eraseBtn);

    await waitFor(() => expect(mockHardNavigate).toHaveBeenCalledWith("/?account_deleted=1"));
    expect(fetchMock).toHaveBeenCalledWith(
      "/v1/account?confirm=DELETE&permanent=true",
      expect.objectContaining({ method: "DELETE", credentials: "include" }),
    );
  });

  it("tells the user a deferred erase left the account in the trash until purge_after", async () => {
    const purgeAfter = "2026-10-26T12:00:00Z";
    global.fetch = jest.fn(async () => ({
      ok: true,
      status: 200,
      text: async () => "",
      json: async () => ({ deleted: true, mode: "trash", erase_deferred: true, purge_after: purgeAfter }),
    })) as unknown as typeof fetch;

    render(<SettingsPage />);
    openDeleteFlow();
    fireEvent.click(screen.getByRole("radio", { name: /erase permanently now/i }));
    fireEvent.change(screen.getByPlaceholderText("DELETE"), { target: { value: "DELETE" } });
    fireEvent.click(screen.getByRole("checkbox", { name: /can.t be recovered/i }));
    fireEvent.click(screen.getByRole("button", { name: /erase my account permanently/i }));

    const heading = await screen.findByText(/was deleted and moved to the trash/i);
    const status = heading.closest('[role="status"]') as HTMLElement;
    expect(status).toHaveTextContent(/emailed people outside e2a recently/i);
    expect(status).toHaveTextContent(/stays in the trash/i);
    expect(status).toHaveTextContent(/2026/);
    expect(status).toHaveTextContent(/sign in again/i);
    expect(mockHardNavigate).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", { name: /continue/i }));
    expect(mockHardNavigate).toHaveBeenCalledWith("/?account_deleted=1");
  });

  it("switching back to trash clears the permanent acknowledgement", () => {
    render(<SettingsPage />);
    openDeleteFlow();
    fireEvent.click(screen.getByRole("radio", { name: /erase permanently now/i }));
    fireEvent.click(screen.getByRole("checkbox", { name: /can.t be recovered/i }));
    fireEvent.click(screen.getByRole("radio", { name: /move to trash/i }));
    expect(screen.queryByRole("checkbox")).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("radio", { name: /erase permanently now/i }));
    expect(screen.getByRole("checkbox", { name: /can.t be recovered/i })).not.toBeChecked();
  });

  it.each([
    [409, "erase_held", /sending is paused/i],
    [429, "rate_limited", /wait a few minutes/i],
  ])("maps a %i %s to specific copy", async (status, code, message) => {
    global.fetch = jest.fn(async () => ({
      ok: false,
      status,
      text: async () => JSON.stringify({ error: { code, message: "raw server text", request_id: "req_t" } }),
    })) as unknown as typeof fetch;
    render(<SettingsPage />);
    openDeleteFlow();
    fireEvent.click(screen.getByRole("radio", { name: /erase permanently now/i }));
    fireEvent.change(screen.getByPlaceholderText("DELETE"), { target: { value: "DELETE" } });
    fireEvent.click(screen.getByRole("checkbox", { name: /can.t be recovered/i }));
    fireEvent.click(screen.getByRole("button", { name: /erase my account permanently/i }));
    expect(await screen.findByText(message)).toBeInTheDocument();
    expect(screen.queryByText(/raw server text/)).not.toBeInTheDocument();
  });

  it("shows an error message when the server rejects the delete", async () => {
    global.fetch = jest.fn(async () => ({ ok: false, status: 400, text: async () => "nope" })) as unknown as typeof fetch;
    render(<SettingsPage />);
    openDeleteFlow();
    fireEvent.change(screen.getByPlaceholderText("DELETE"), { target: { value: "DELETE" } });
    fireEvent.click(screen.getByRole("button", { name: /delete my account/i }));

    await waitFor(() => {
      expect(screen.getByRole("alert")).toHaveTextContent(/Deleting didn.t finish:\s*nope/);
    });
    expect(mockHardNavigate).not.toHaveBeenCalled();
  });

  it("shows the error envelope's message, not the raw JSON", async () => {
    global.fetch = jest.fn(async () => ({
      ok: false,
      status: 409,
      text: async () =>
        JSON.stringify({
          error: { code: "send_in_progress", message: "a send is in progress; retry shortly", request_id: "req_test" },
        }),
    })) as unknown as typeof fetch;
    render(<SettingsPage />);
    openDeleteFlow();
    fireEvent.change(screen.getByPlaceholderText("DELETE"), { target: { value: "DELETE" } });
    fireEvent.click(screen.getByRole("button", { name: /delete my account/i }));

    await waitFor(() => {
      expect(screen.getByRole("alert")).toHaveTextContent("a send is in progress; retry shortly");
    });
    expect(screen.getByRole("alert")).not.toHaveTextContent("request_id");
  });
});
