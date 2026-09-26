import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { SWRConfig } from "swr";
import { RestoredNotice, RESTORED_NOTICE_STORAGE_KEY } from "./RestoredNotice";

jest.mock("next/link", () => {
  return function MockLink({ href, children, ...rest }: { href: string; children: React.ReactNode; [k: string]: unknown }) {
    return <a href={href} {...rest}>{children}</a>;
  };
});

const DAY = 24 * 60 * 60 * 1000;

function mockAccount(extra: Record<string, unknown>) {
  const body = {
    user: { id: "usr_test", email: "owner@example.test" },
    scope: "account",
    plan_code: "free",
    limits: { max_agents: 3, max_domains: 1, max_messages_month: 3000, max_storage_bytes: 1 },
    usage: { agents: 0, domains: 0, messages_month: 0, storage_bytes: 0 },
    upgrade_url: "",
    ...extra,
  };
  global.fetch = jest.fn(async () => ({
    ok: true,
    status: 200,
    json: async () => body,
    text: async () => JSON.stringify(body),
  })) as unknown as typeof fetch;
}

function renderNotice() {
  // Fresh cache per render so one test's account data can't leak into the next.
  return render(
    <SWRConfig value={{ provider: () => new Map(), dedupingInterval: 0 }}>
      <RestoredNotice />
    </SWRConfig>,
  );
}

const recentRestore = () => new Date(Date.now() - 2 * 60 * 60 * 1000).toISOString();

beforeEach(() => {
  window.localStorage.clear();
  jest.restoreAllMocks();
});

describe("RestoredNotice", () => {
  it("shows when the account carries a recent restored_at", async () => {
    mockAccount({ restored_at: recentRestore() });
    renderNotice();
    const notice = await screen.findByTestId("account-restored-notice");
    expect(notice).toHaveTextContent(/your account was restored/i);
    expect(notice).toHaveTextContent(/API keys were revoked/i);
    expect(notice).toHaveTextContent(/re-verified/i);
    expect(screen.getByRole("link", { name: /create an api key/i })).toHaveAttribute("href", "/api-keys");
    expect(screen.getByRole("link", { name: /re-verify domains/i })).toHaveAttribute("href", "/domains");
  });

  it("stays hidden for an account that was never restored", async () => {
    mockAccount({});
    renderNotice();
    await waitFor(() => expect(global.fetch).toHaveBeenCalledWith("/v1/account", expect.anything()));
    expect(screen.queryByTestId("account-restored-notice")).not.toBeInTheDocument();
  });

  it("stays hidden for a restore older than the notice window", async () => {
    mockAccount({ restored_at: new Date(Date.now() - 45 * DAY).toISOString() });
    renderNotice();
    await waitFor(() => expect(global.fetch).toHaveBeenCalled());
    expect(screen.queryByTestId("account-restored-notice")).not.toBeInTheDocument();
  });

  it("remembers dismissal for that restored_at across mounts", async () => {
    const restoredAt = recentRestore();
    mockAccount({ restored_at: restoredAt });
    const { unmount } = renderNotice();
    fireEvent.click(await screen.findByRole("button", { name: /dismiss restored account notice/i }));
    expect(screen.queryByTestId("account-restored-notice")).not.toBeInTheDocument();
    expect(window.localStorage.getItem(RESTORED_NOTICE_STORAGE_KEY)).toBe(restoredAt);
    unmount();

    renderNotice();
    await waitFor(() => expect(global.fetch).toHaveBeenCalledTimes(2));
    expect(screen.queryByTestId("account-restored-notice")).not.toBeInTheDocument();
  });

  it("shows again for a newer restore after an older one was dismissed", async () => {
    window.localStorage.setItem(RESTORED_NOTICE_STORAGE_KEY, new Date(Date.now() - 10 * DAY).toISOString());
    mockAccount({ restored_at: recentRestore() });
    renderNotice();
    expect(await screen.findByTestId("account-restored-notice")).toBeInTheDocument();
  });

  it("tolerates storage that throws on read and write", async () => {
    jest.spyOn(Storage.prototype, "getItem").mockImplementation(() => {
      throw new Error("SecurityError");
    });
    jest.spyOn(Storage.prototype, "setItem").mockImplementation(() => {
      throw new Error("QuotaExceededError");
    });
    mockAccount({ restored_at: recentRestore() });
    renderNotice();
    fireEvent.click(await screen.findByRole("button", { name: /dismiss restored account notice/i }));
    // Dismissal still holds for this page session.
    expect(screen.queryByTestId("account-restored-notice")).not.toBeInTheDocument();
  });

  it("renders nothing when the account read fails", async () => {
    global.fetch = jest.fn(async () => ({
      ok: false,
      status: 500,
      text: async () => "boom",
    })) as unknown as typeof fetch;
    renderNotice();
    await waitFor(() => expect(global.fetch).toHaveBeenCalled());
    expect(screen.queryByTestId("account-restored-notice")).not.toBeInTheDocument();
  });
});
