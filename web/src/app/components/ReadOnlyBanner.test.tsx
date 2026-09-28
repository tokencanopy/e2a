import { render, screen, waitFor } from "@testing-library/react";
import { SWRConfig } from "swr";
import { ReadOnlyBanner } from "./ReadOnlyBanner";

jest.mock("next/link", () => {
  return function MockLink({ href, children, ...rest }: { href: string; children: React.ReactNode; [k: string]: unknown }) {
    return <a href={href} {...rest}>{children}</a>;
  };
});

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

function renderBanner() {
  return render(
    <SWRConfig value={{ provider: () => new Map(), dedupingInterval: 0 }}>
      <ReadOnlyBanner />
    </SWRConfig>,
  );
}

beforeEach(() => {
  jest.restoreAllMocks();
});

describe("ReadOnlyBanner", () => {
  it("shows the read-only banner with a support link when read_only is true", async () => {
    mockAccount({ read_only: true });
    renderBanner();
    const banner = await screen.findByTestId("account-read-only-banner");
    expect(banner).toHaveTextContent(
      "Your account is read-only while sending is paused for abuse review. Contact support.",
    );
    expect(banner).toHaveTextContent(/still read your inboxes/i);
    expect(screen.getByRole("link", { name: /contact support/i })).toHaveAttribute("href", "/feedback");
    // Not dismissible: there is no close control while the state lasts.
    expect(screen.queryByRole("button")).toBeNull();
  });

  it("stays hidden for a writable account", async () => {
    mockAccount({ read_only: false });
    renderBanner();
    await waitFor(() => expect(global.fetch).toHaveBeenCalled());
    expect(screen.queryByTestId("account-read-only-banner")).toBeNull();
  });

  it("stays hidden when the server does not report read_only", async () => {
    mockAccount({});
    renderBanner();
    await waitFor(() => expect(global.fetch).toHaveBeenCalled());
    expect(screen.queryByTestId("account-read-only-banner")).toBeNull();
  });
});
