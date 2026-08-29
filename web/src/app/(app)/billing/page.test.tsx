import { render, screen, waitFor } from "@testing-library/react";
import { SWRConfig } from "swr";
import BillingPage from "./page";

// Mock next/link so the PageShell / Topbar links don't try to resolve
// router state.
jest.mock("next/link", () => {
  return function MockLink({
    href,
    children,
    ...rest
  }: {
    href: string;
    children: React.ReactNode;
    [k: string]: unknown;
  }) {
    return (
      <a href={href} {...rest}>
        {children}
      </a>
    );
  };
});

const mockFetch = jest.fn();
beforeEach(() => {
  mockFetch.mockReset();
  global.fetch = mockFetch;
  window.history.replaceState({}, "", "/billing");
});

// Wraps the page in a fresh SWR provider per test so cached responses
// from a previous test don't leak into this one and silently mask a
// missing mock.
function renderPage() {
  return render(
    <SWRConfig value={{ provider: () => new Map(), dedupingInterval: 0 }}>
      <BillingPage />
    </SWRConfig>,
  );
}

function stageLimits(payload: unknown) {
  mockFetch.mockImplementation((url: string) => {
    if (url === "/v1/account") {
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve(payload),
      });
    }
    return Promise.resolve({ ok: false, text: () => Promise.resolve("404") });
  });
}

describe("BillingPage", () => {
  it("renders plan name and all four usage rows", async () => {
    stageLimits({
      plan_code: "default",
      limits: {
        max_agents: 100,
        max_domains: 10,
        max_messages_month: 50000,
        max_storage_bytes: 10737418240,
      },
      usage: {
        agents: 3,
        domains: 1,
        messages_month: 1247,
        storage_bytes: 425600,
      },
      upgrade_url: "",
    });
    renderPage();

    await waitFor(() =>
      expect(screen.getByText(/Default \(operator-configured\)/i)).toBeInTheDocument(),
    );
    expect(screen.getByText("Inboxes")).toBeInTheDocument();
    expect(screen.getByText("Domains")).toBeInTheDocument();
    expect(screen.getByText("Sends this month")).toBeInTheDocument();
    expect(screen.getByText("Storage")).toBeInTheDocument();

    // Spot-check formatted numbers — message count uses thousands
    // separator, storage uses unit suffix.
    expect(screen.getByText(/1,247/)).toBeInTheDocument();
    expect(screen.getByText(/415\.6 KB|425600/)).toBeInTheDocument();
  });

  it("hides Upgrade and Manage Billing buttons when NEXT_PUBLIC_BILLING_API is unset", async () => {
    // Default Jest env has no NEXT_PUBLIC_BILLING_API set — the module
    // already captured an empty string at import time, so neither
    // button should render.
    stageLimits({
      plan_code: "default",
      limits: { max_agents: 1, max_domains: 1, max_messages_month: 1, max_storage_bytes: 1 },
      usage: { agents: 0, domains: 0, messages_month: 0, storage_bytes: 0 },
      upgrade_url: "",
    });
    renderPage();

    await waitFor(() => screen.getByText(/Default/i));
    expect(screen.queryByText(/Upgrade/i)).not.toBeInTheDocument();
    expect(screen.queryByText(/Manage billing/i)).not.toBeInTheDocument();
    // The plan comparison is also a sidecar-only surface — with no
    // BILLING_API there's no catalog to fetch, so the Plans section and
    // its tier cards must not render.
    expect(screen.queryByText("Plans")).not.toBeInTheDocument();
    expect(screen.queryByText("Scale")).not.toBeInTheDocument();
  });

  it("ignores a checkout marker when no billing sidecar is configured", async () => {
    // Post-checkout reconciliation waits on the sidecar's plan read to
    // report an active subscription. With no sidecar there is no such
    // read, so a stray ?status=success must not park the page on a
    // "Finalizing your purchase" notice that can never resolve — a
    // self-host install has no checkout to come back from.
    window.history.replaceState({}, "", "/billing?status=success");
    stageLimits({
      plan_code: "default",
      limits: { max_agents: 1, max_domains: 1, max_messages_month: 1, max_storage_bytes: 1 },
      usage: { agents: 0, domains: 0, messages_month: 0, storage_bytes: 0 },
      upgrade_url: "",
    });
    renderPage();

    await waitFor(() => screen.getByText(/Default/i));
    expect(screen.queryByText(/Finalizing your purchase/i)).not.toBeInTheDocument();
  });

  it("renders error state when the API returns non-2xx", async () => {
    mockFetch.mockImplementation(() =>
      Promise.resolve({
        ok: false,
        status: 503,
        text: () => Promise.resolve("limits subsystem not configured"),
      }),
    );
    renderPage();

    await waitFor(() =>
      expect(
        screen.getByText(/Couldn't load your limits/i),
      ).toBeInTheDocument(),
    );
  });
});
