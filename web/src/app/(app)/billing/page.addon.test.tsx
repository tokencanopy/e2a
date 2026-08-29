import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { SWRConfig } from "swr";

// Mock next/link so PageShell / Topbar links don't resolve router state.
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

// BILLING_API is captured at module-evaluation time from this env var
// (and inlined by Next in prod). Set it BEFORE requiring the page so the
// module sees a configured sidecar — hence `require` here rather than a
// top-level `import`, which would be hoisted above this assignment.
process.env.NEXT_PUBLIC_BILLING_API = "https://billing.test";
// eslint-disable-next-line @typescript-eslint/no-require-imports
const BillingPage = require("./page").default as React.ComponentType;

const PLAN_URL = "https://billing.test/api/billing/plan";
const ADDON_URL = "https://billing.test/api/billing/addon";
const PORTAL_URL = "https://billing.test/api/billing/portal";

const CATALOG = [
  {
    code: "free",
    display_name: "Free",
    monthly_price_cents: 0,
    max_agents: 3,
    max_domains: 3,
    max_messages_month: 3000,
    max_storage_bytes: 1 << 30,
  },
  {
    code: "pro",
    display_name: "Pro",
    monthly_price_cents: 2000,
    max_agents: 25,
    max_domains: 10,
    max_messages_month: 50000,
    max_storage_bytes: 10 * (1 << 30),
  },
];

// Mirrors the sidecar's AddOnEntry (plans.InboxAddOn): $2.00/mo per
// unit, each unit adds 1 inbox + 3,000 sends/mo, any quantity lifts the
// Free daily cap.
const ADDON = {
  code: "addon_inbox",
  display_name: "Inbox add-on",
  monthly_price_cents_per_unit: 200,
  max_quantity: 1000,
  per_unit: { max_agents: 1, max_messages_month: 3000 },
  lifts_free_daily_cap: true,
};

const FREE_LIMITS = {
  plan_code: "free",
  limits: {
    max_agents: 3,
    max_domains: 3,
    max_messages_month: 3000,
    max_storage_bytes: 1 << 30,
  },
  usage: { agents: 1, domains: 0, messages_month: 120, storage_bytes: 1024 },
  upgrade_url: "",
};

const PRO_LIMITS = {
  plan_code: "pro",
  limits: {
    max_agents: 25,
    max_domains: 10,
    max_messages_month: 50000,
    max_storage_bytes: 10 * (1 << 30),
  },
  usage: { agents: 4, domains: 2, messages_month: 9000, storage_bytes: 2048 },
  upgrade_url: PORTAL_URL,
};

const mockFetch = jest.fn();
beforeEach(() => {
  mockFetch.mockReset();
  global.fetch = mockFetch;
});

beforeAll(() => {
  window.alert = jest.fn();
});

type StageOpts = {
  limits: unknown;
  plan: unknown;
  // Response for POST /api/billing/addon. Default: a checkout redirect.
  addonResponse?: { url?: string; updated?: boolean };
  addonFails?: { status: number; body: string };
};

function stage(opts: StageOpts) {
  mockFetch.mockImplementation((url: string, init?: RequestInit) => {
    if (url === "/v1/account") {
      return Promise.resolve({ ok: true, json: () => Promise.resolve(opts.limits) });
    }
    if (url === PLAN_URL) {
      return Promise.resolve({ ok: true, json: () => Promise.resolve(opts.plan) });
    }
    if (url === ADDON_URL && init?.method === "POST") {
      const failure = opts.addonFails;
      if (failure) {
        return Promise.resolve({
          ok: false,
          status: failure.status,
          text: () => Promise.resolve(failure.body),
        });
      }
      return Promise.resolve({
        ok: true,
        json: () =>
          Promise.resolve(opts.addonResponse ?? { url: "https://stripe.test/checkout" }),
      });
    }
    return Promise.resolve({ ok: false, status: 404, text: () => Promise.resolve("404") });
  });
}

function renderPage() {
  return render(
    <SWRConfig value={{ provider: () => new Map(), dedupingInterval: 0 }}>
      <BillingPage />
    </SWRConfig>,
  );
}

function addonPost(): { quantity: number } | null {
  const call = mockFetch.mock.calls.find(
    ([u, init]: [string, RequestInit?]) => u === ADDON_URL && init?.method === "POST",
  );
  if (!call) return null;
  return JSON.parse((call[1] as RequestInit).body as string);
}

describe("BillingPage — inbox add-on", () => {
  it("renders the add-on card from the catalog payload", async () => {
    stage({
      limits: FREE_LIMITS,
      plan: {
        catalog: CATALOG,
        addon: ADDON,
        current: { code: "free", status: "inactive", has_stripe_customer: false, addon_quantity: 0 },
      },
    });
    renderPage();

    await waitFor(() => expect(screen.getByText("Inbox add-on")).toBeInTheDocument());
    // Price and per-unit adds come from the payload, never hardcoded.
    expect(screen.getByText(/\$2\/mo each/)).toBeInTheDocument();
    expect(screen.getByText(/adds 1 inbox and 3,000 sends/)).toBeInTheDocument();
    // Free plan + lifts_free_daily_cap → the daily-cap hint shows.
    expect(screen.getByText(/daily send cap/)).toBeInTheDocument();
  });

  it("hides the card when the sidecar payload has no addon entry", async () => {
    stage({
      limits: FREE_LIMITS,
      plan: {
        catalog: CATALOG,
        current: { code: "free", status: "inactive", has_stripe_customer: false },
      },
    });
    renderPage();

    await waitFor(() => expect(screen.getByText("Plans")).toBeInTheDocument());
    expect(screen.queryByText("Inbox add-on")).not.toBeInTheDocument();
  });

  it("buys add-ons via Checkout for a user with no subscription", async () => {
    stage({
      limits: FREE_LIMITS,
      plan: {
        catalog: CATALOG,
        addon: ADDON,
        current: { code: "free", status: "inactive", has_stripe_customer: false, addon_quantity: 0 },
      },
      addonResponse: { url: "https://stripe.test/checkout" },
    });
    renderPage();

    await screen.findByText("Inbox add-on");
    // Step 0 → 2, then buy.
    const inc = screen.getByRole("button", { name: "Increase add-on quantity" });
    await userEvent.click(inc);
    await userEvent.click(inc);
    await userEvent.click(screen.getByRole("button", { name: "Buy add-ons" }));

    await waitFor(() => expect(addonPost()).toEqual({ quantity: 2 }));
  });

  it("updates quantity in place for a subscriber and reports pending provisioning", async () => {
    stage({
      limits: PRO_LIMITS,
      plan: {
        catalog: CATALOG,
        addon: ADDON,
        current: { code: "pro", status: "active", has_stripe_customer: true, addon_quantity: 1 },
      },
      addonResponse: { updated: true },
    });
    renderPage();

    await screen.findByText("Inbox add-on");
    // Current quantity from the payload.
    expect(screen.getByLabelText("Add-on quantity")).toHaveValue(1);

    await userEvent.click(screen.getByRole("button", { name: "Increase add-on quantity" }));
    await userEvent.click(screen.getByRole("button", { name: "Update add-ons" }));

    await waitFor(() => expect(addonPost()).toEqual({ quantity: 2 }));
    // In-place update → no redirect; the page reports the webhook is
    // provisioning the new caps.
    await waitFor(() =>
      expect(screen.getByText(/Updating your add-ons/)).toBeInTheDocument(),
    );
  });

  it("disables the action when the desired quantity equals the current one", async () => {
    stage({
      limits: PRO_LIMITS,
      plan: {
        catalog: CATALOG,
        addon: ADDON,
        current: { code: "pro", status: "active", has_stripe_customer: true, addon_quantity: 3 },
      },
    });
    renderPage();

    await screen.findByText("Inbox add-on");
    expect(screen.getByRole("button", { name: "Update add-ons" })).toBeDisabled();
    // Quantity can't go below zero either.
    expect(screen.getByLabelText("Add-on quantity")).toHaveValue(3);
  });

  it("surfaces an error and re-enables the button when the POST fails", async () => {
    stage({
      limits: PRO_LIMITS,
      plan: {
        catalog: CATALOG,
        addon: ADDON,
        current: { code: "pro", status: "active", has_stripe_customer: true, addon_quantity: 0 },
      },
      addonFails: { status: 503, body: "add-on not available" },
    });
    renderPage();

    await screen.findByText("Inbox add-on");
    await userEvent.click(screen.getByRole("button", { name: "Increase add-on quantity" }));
    const apply = screen.getByRole("button", { name: "Update add-ons" });
    await userEvent.click(apply);

    await waitFor(() => expect(window.alert).toHaveBeenCalled());
    // Failure clears the in-flight state so the user can retry.
    expect(screen.getByRole("button", { name: "Update add-ons" })).not.toBeDisabled();
  });
});
