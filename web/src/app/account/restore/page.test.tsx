import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import RestoreAccountPage from "./page";

jest.mock("next/link", () => {
  return function MockLink({ href, children, ...rest }: { href: string; children: React.ReactNode; [k: string]: unknown }) {
    return <a href={href} {...rest}>{children}</a>;
  };
});

// The sign-in links pull in in-app-browser detection; a plain anchor is enough.
jest.mock("../../components/SignInLink", () => ({
  SignInLink: ({ children, href }: { children: React.ReactNode; href?: string }) => (
    <a href={href}>{children}</a>
  ),
}));

const mockHardNavigate = jest.fn();
jest.mock("../../../lib/navigation", () => ({
  hardNavigate: (url: string) => mockHardNavigate(url),
}));

type FakeResponse = { ok: boolean; status: number; json: () => Promise<unknown>; text: () => Promise<string> };

function jsonResponse(status: number, body: unknown): FakeResponse {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: async () => body,
    text: async () => JSON.stringify(body),
  };
}

function envelope(status: number, code: string, message = "server message") {
  return jsonResponse(status, { error: { code, message, request_id: "req_test" } });
}

const DAY = 24 * 60 * 60 * 1000;
const deletion = {
  email: "owner@example.test",
  deleted_at: new Date(Date.now() - 6 * DAY).toISOString(),
  purge_after: new Date(Date.now() + 24 * DAY - 60_000).toISOString(),
  purge_in_progress: false,
};

// Routes the page's three endpoints to per-test responses. `restore`/`erase`
// may be a response or a pending promise (to observe in-flight state).
function installFetch(routes: {
  deletion?: FakeResponse | Promise<FakeResponse>;
  restore?: FakeResponse | Promise<FakeResponse>;
  erase?: FakeResponse | Promise<FakeResponse>;
}) {
  const fetchMock = jest.fn(async (url: string) => {
    if (url === "/api/account/deletion") return routes.deletion ?? jsonResponse(200, deletion);
    if (url === "/api/account/restore" && routes.restore) return routes.restore;
    if (url === "/api/account/erase" && routes.erase) return routes.erase;
    throw new Error(`unexpected fetch ${url}`);
  });
  global.fetch = fetchMock as unknown as typeof fetch;
  return fetchMock;
}

beforeEach(() => {
  mockHardNavigate.mockClear();
});

async function renderReady() {
  render(<RestoreAccountPage />);
  await screen.findByRole("heading", { name: /your account is in the trash/i });
}

describe("/account/restore", () => {
  it("shows a loading state while the deletion status loads", () => {
    installFetch({ deletion: new Promise(() => {}) });
    render(<RestoreAccountPage />);
    expect(screen.getByRole("status")).toHaveTextContent(/checking your account/i);
  });

  it("reads the deletion status with cookie credentials and shows both dates and the restore window", async () => {
    const fetchMock = installFetch({});
    await renderReady();
    expect(fetchMock).toHaveBeenCalledWith(
      "/api/account/deletion",
      expect.objectContaining({ credentials: "include" }),
    );
    expect(screen.getByText("owner@example.test")).toBeInTheDocument();
    expect(screen.getByText(/was scheduled for deletion on/i)).toBeInTheDocument();
    expect(screen.getByText(/will be permanently deleted after/i)).toBeInTheDocument();
    expect(screen.getByText("24 days left to restore")).toBeInTheDocument();
    expect(screen.getByTestId("trash-window-track")).toHaveAttribute("aria-hidden", "true");
    // What a restore does not bring back is stated before restoring.
    expect(screen.getByText(/They stay revoked, so create new ones/i)).toBeInTheDocument();
    expect(screen.getByText(/Re-verify each domain/i)).toBeInTheDocument();
  });

  it("offers sign-in again on 401 (no restricted session)", async () => {
    installFetch({ deletion: envelope(401, "unauthorized") });
    render(<RestoreAccountPage />);
    expect(await screen.findByRole("heading", { name: /nothing to restore/i })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /sign in/i })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /restore account/i })).not.toBeInTheDocument();
  });

  it("offers a retry when the status fails to load, and retries", async () => {
    const fetchMock = installFetch({ deletion: envelope(503, "auth_unavailable") });
    render(<RestoreAccountPage />);
    const retry = await screen.findByRole("button", { name: /try again/i });
    installFetchRoutesOnto(fetchMock, jsonResponse(200, deletion));
    fireEvent.click(retry);
    expect(await screen.findByRole("heading", { name: /your account is in the trash/i })).toBeInTheDocument();
  });

  it("shows no restore action when a purge is already in progress", async () => {
    installFetch({ deletion: jsonResponse(200, { ...deletion, purge_in_progress: true }) });
    render(<RestoreAccountPage />);
    expect(await screen.findByRole("heading", { name: /being erased/i })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /restore account/i })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /erase/i })).not.toBeInTheDocument();
  });

  it("restores and navigates to the dashboard with a full page load", async () => {
    const fetchMock = installFetch({
      restore: jsonResponse(200, { restored: true, restored_at: new Date().toISOString() }),
    });
    await renderReady();
    fireEvent.click(screen.getByRole("button", { name: /restore account/i }));
    await waitFor(() => expect(mockHardNavigate).toHaveBeenCalledWith("/inboxes"));
    expect(fetchMock).toHaveBeenCalledWith(
      "/api/account/restore",
      expect.objectContaining({ method: "POST", credentials: "include" }),
    );
    // Buttons stay disabled through the navigation.
    expect(screen.getByRole("button", { name: /restoring/i })).toBeDisabled();
    expect(screen.getByRole("button", { name: /erase now/i })).toBeDisabled();
  });

  it("disables both actions while the restore is in flight", async () => {
    installFetch({ restore: new Promise(() => {}) });
    await renderReady();
    fireEvent.click(screen.getByRole("button", { name: /restore account/i }));
    expect(await screen.findByRole("button", { name: /restoring…/i })).toBeDisabled();
    expect(screen.getByRole("button", { name: /erase now/i })).toBeDisabled();
  });

  it("removes the restore action on registration_refused and explains why", async () => {
    installFetch({ restore: envelope(403, "registration_refused") });
    await renderReady();
    fireEvent.click(screen.getByRole("button", { name: /restore account/i }));
    expect(await screen.findByRole("alert")).toHaveTextContent(/can.t be restored/i);
    expect(screen.queryByRole("button", { name: /restore account/i })).not.toBeInTheDocument();
    // No restore countdown for an account that can't be restored.
    expect(screen.queryByText(/left to restore/i)).not.toBeInTheDocument();
    // Erasing is still possible.
    expect(screen.getByRole("button", { name: /erase now/i })).toBeEnabled();
    expect(mockHardNavigate).not.toHaveBeenCalled();
  });

  it("switches to the being-erased state on purge_in_progress", async () => {
    installFetch({ restore: envelope(409, "purge_in_progress") });
    await renderReady();
    fireEvent.click(screen.getByRole("button", { name: /restore account/i }));
    expect(await screen.findByRole("heading", { name: /being erased/i })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /restore account/i })).not.toBeInTheDocument();
  });

  it("points to the dashboard on not_in_trash (restored elsewhere)", async () => {
    installFetch({ restore: envelope(409, "not_in_trash") });
    await renderReady();
    fireEvent.click(screen.getByRole("button", { name: /restore account/i }));
    expect(await screen.findByRole("heading", { name: /already active/i })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /open the dashboard/i })).toHaveAttribute("href", "/inboxes");
  });

  it("falls back to sign-in when the restricted session expired mid-flow", async () => {
    installFetch({ restore: envelope(401, "unauthorized") });
    await renderReady();
    fireEvent.click(screen.getByRole("button", { name: /restore account/i }));
    expect(await screen.findByRole("heading", { name: /nothing to restore/i })).toBeInTheDocument();
  });

  it.each([
    [503, /temporarily unavailable/i],
    [500, /didn.t finish/i],
  ])("keeps the restore action available to retry after a %i", async (status, message) => {
    installFetch({ restore: envelope(status, "internal_error") });
    await renderReady();
    fireEvent.click(screen.getByRole("button", { name: /restore account/i }));
    expect(await screen.findByRole("alert")).toHaveTextContent(message);
    expect(screen.getByRole("alert")).toHaveTextContent(/still in the trash/i);
    expect(screen.getByRole("button", { name: /restore account/i })).toBeEnabled();
  });

  it("reports a network failure on restore without leaving the page", async () => {
    global.fetch = jest.fn(async (url: string) => {
      if (url === "/api/account/deletion") return jsonResponse(200, deletion);
      throw new TypeError("Failed to fetch");
    }) as unknown as typeof fetch;
    await renderReady();
    fireEvent.click(screen.getByRole("button", { name: /restore account/i }));
    expect(await screen.findByRole("alert")).toHaveTextContent(/couldn.t reach e2a/i);
    expect(screen.getByRole("button", { name: /restore account/i })).toBeEnabled();
  });

  describe("erase now", () => {
    it("asks for confirmation that states it is irreversible and the identity may be held", async () => {
      const fetchMock = installFetch({});
      await renderReady();
      fireEvent.click(screen.getByRole("button", { name: /erase now/i }));
      const dialog = screen.getByRole("region", { name: /erase this account permanently/i });
      expect(dialog).toHaveTextContent(/can.t be undone/i);
      expect(dialog).toHaveTextContent(/may be held for a while/i);
      expect(screen.getByRole("heading", { name: /erase this account permanently/i })).toHaveFocus();
      // Nothing is erased until the explicit confirm.
      expect(fetchMock).not.toHaveBeenCalledWith("/api/account/erase", expect.anything());
    });

    it("cancel returns to the choice and focus to the trigger", async () => {
      installFetch({});
      await renderReady();
      fireEvent.click(screen.getByRole("button", { name: /erase now/i }));
      fireEvent.click(screen.getByRole("button", { name: /cancel/i }));
      expect(screen.getByRole("button", { name: /erase now/i })).toHaveFocus();
      expect(screen.getByRole("button", { name: /restore account/i })).toBeInTheDocument();
    });

    it("erases and shows the final state with a link home", async () => {
      const fetchMock = installFetch({
        erase: jsonResponse(200, { deleted: true, mode: "permanent", user_deleted: true }),
      });
      await renderReady();
      fireEvent.click(screen.getByRole("button", { name: /erase now/i }));
      fireEvent.click(screen.getByRole("button", { name: /erase permanently/i }));
      expect(await screen.findByRole("heading", { name: /your account and its data were erased/i })).toBeInTheDocument();
      expect(screen.getByRole("link", { name: /back to e2a home/i })).toHaveAttribute("href", "/");
      expect(fetchMock).toHaveBeenCalledWith(
        "/api/account/erase",
        expect.objectContaining({ method: "POST", credentials: "include" }),
      );
      expect(mockHardNavigate).not.toHaveBeenCalled();
    });

    it("disables the confirm while erasing", async () => {
      installFetch({ erase: new Promise(() => {}) });
      await renderReady();
      fireEvent.click(screen.getByRole("button", { name: /erase now/i }));
      fireEvent.click(screen.getByRole("button", { name: /erase permanently/i }));
      expect(await screen.findByRole("button", { name: /erasing…/i })).toBeDisabled();
      expect(screen.getByRole("button", { name: /cancel/i })).toBeDisabled();
    });

    it("says the account stays in the trash when erasure is temporarily unavailable", async () => {
      installFetch({ erase: envelope(503, "internal_error") });
      await renderReady();
      fireEvent.click(screen.getByRole("button", { name: /erase now/i }));
      fireEvent.click(screen.getByRole("button", { name: /erase permanently/i }));
      expect(await screen.findByRole("alert")).toHaveTextContent(/temporarily unavailable.*stays in the trash/i);
      expect(screen.getByRole("button", { name: /erase permanently/i })).toBeEnabled();
    });

    it("explains erase_held (paused account) instead of asking to retry", async () => {
      installFetch({ erase: envelope(409, "erase_held") });
      await renderReady();
      fireEvent.click(screen.getByRole("button", { name: /erase now/i }));
      fireEvent.click(screen.getByRole("button", { name: /erase permanently/i }));
      const alert = await screen.findByRole("alert");
      expect(alert).toHaveTextContent(/sending is paused/i);
      expect(alert).toHaveTextContent(/deleted permanently when the trash window ends/i);
      expect(alert).not.toHaveTextContent(/didn.t finish/i);
    });
  });

  it("explains a restore cooldown (429 rate_limited) instead of asking to retry", async () => {
    installFetch({ restore: envelope(429, "rate_limited") });
    await renderReady();
    fireEvent.click(screen.getByRole("button", { name: /restore account/i }));
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent(/restored moments ago/i);
    expect(alert).not.toHaveTextContent(/didn.t finish/i);
  });
});

// Swap the deletion route's response on an installed mock (used by the retry test).
function installFetchRoutesOnto(fetchMock: jest.Mock, deletionResponse: FakeResponse) {
  fetchMock.mockImplementation(async (url: string) => {
    if (url === "/api/account/deletion") return deletionResponse;
    throw new Error(`unexpected fetch ${url}`);
  });
}
