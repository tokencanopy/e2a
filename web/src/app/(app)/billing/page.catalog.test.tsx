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
// This is a separate test file from page.test.tsx precisely so the two
// don't fight over the value (Jest gives each test file its own env).
process.env.NEXT_PUBLIC_BILLING_API = "https://billing.test";
// eslint-disable-next-line @typescript-eslint/no-require-imports
const BillingPage = require("./page").default as React.ComponentType;

const PLAN_URL = "https://billing.test/api/billing/plan";
const CHECKOUT_URL = "https://billing.test/api/billing/checkout";
const PORTAL_URL = "https://billing.test/api/billing/portal";

// The catalog the sidecar's plans package advertises (Free/Pro/Scale).
const CATALOG = [
  {
    code: "free",
    display_name: "Free",
    monthly_price_cents: 0,
    max_agents: 3,
    max_domains: 1,
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
  {
    code: "scale",
    display_name: "Scale",
    monthly_price_cents: 9900,
    max_agents: 250,
    max_domains: 50,
    max_messages_month: 500000,
    max_storage_bytes: 100 * (1 << 30),
  },
];

const FREE_LIMITS = {
  plan_code: "free",
  limits: {
    max_agents: 3,
    max_domains: 1,
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
  // upgrade_url present == active subscription; it is also the portal POST target.
  upgrade_url: PORTAL_URL,
};

const mockFetch = jest.fn();
beforeEach(() => {
  mockFetch.mockReset();
  global.fetch = mockFetch;
});

// jsdom's window.location is non-configurable and doesn't implement
// navigation, so we can't observe the final `window.location.href = url`
// redirect directly. The meaningful behavior is which billing endpoint
// gets POSTed (and with what body) — that's what the tests assert. The
// page's redirect assignment is a no-op in jsdom (logged, not thrown),
// so postBilling still completes its happy path. Mock alert so an
// unexpected error path doesn't surface a jsdom dialog.
beforeAll(() => {
  window.alert = jest.fn();
});

type StageOpts = {
  limits: unknown;
  plan?: unknown; // catalog/current payload; omit to 500 the plan fetch
  planFails?: boolean;
  checkoutUrl?: string;
  portalUrl?: string;
};

function stage(opts: StageOpts) {
  mockFetch.mockImplementation((url: string, init?: RequestInit) => {
    if (url === "/v1/account") {
      return Promise.resolve({ ok: true, json: () => Promise.resolve(opts.limits) });
    }
    if (url === PLAN_URL) {
      if (opts.planFails) {
        return Promise.resolve({
          ok: false,
          status: 500,
          text: () => Promise.resolve("boom"),
        });
      }
      return Promise.resolve({ ok: true, json: () => Promise.resolve(opts.plan) });
    }
    if (url === CHECKOUT_URL && init?.method === "POST") {
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({ url: opts.checkoutUrl ?? "https://stripe.test/checkout" }),
      });
    }
    if (url === PORTAL_URL && init?.method === "POST") {
      return Promise.resolve({
        ok: true,
        json: () => Promise.resolve({ url: opts.portalUrl ?? "https://stripe.test/portal" }),
      });
    }
    return Promise.resolve({ ok: false, text: () => Promise.resolve("404") });
  });
}

function renderPage() {
  return render(
    <SWRConfig value={{ provider: () => new Map(), dedupingInterval: 0 }}>
      <BillingPage />
    </SWRConfig>,
  );
}

describe("BillingPage — tier comparison", () => {
  it("renders every catalog tier with its quota and prices", async () => {
    stage({ limits: FREE_LIMITS, plan: { catalog: CATALOG, current: { code: "free", status: "inactive", has_stripe_customer: false } } });
    renderPage();

    await waitFor(() => expect(screen.getByText("Plans")).toBeInTheDocument());
    // All three tiers present. "Free" also appears in the current-plan
    // banner for this user, so it legitimately matches twice.
    expect(screen.getAllByText("Free").length).toBeGreaterThan(0);
    expect(screen.getByText("Pro")).toBeInTheDocument();
    expect(screen.getByText("Scale")).toBeInTheDocument();
    // Prices from cents.
    expect(screen.getByText("$20/mo")).toBeInTheDocument();
    expect(screen.getByText("$99/mo")).toBeInTheDocument();
    // Quota numbers from the catalog (Scale's caps).
    expect(screen.getByText("250")).toBeInTheDocument();
    expect(screen.getByText("500,000")).toBeInTheDocument();
    expect(screen.getByText("100.00 GB")).toBeInTheDocument();
  });

  it("marks the current tier and offers Upgrade CTAs to a free user", async () => {
    stage({ limits: FREE_LIMITS, plan: { catalog: CATALOG, current: { code: "free", status: "inactive", has_stripe_customer: false } } });
    renderPage();

    await waitFor(() => expect(screen.getByText("Current")).toBeInTheDocument());
    // Free is current → upgrade CTAs only for the paid tiers.
    expect(screen.getByRole("button", { name: "Upgrade to Pro" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Upgrade to Scale" })).toBeInTheDocument();
    // Free tier shows no action button.
    expect(screen.queryByRole("button", { name: /Upgrade to Free|Downgrade|Switch to Free/ })).not.toBeInTheDocument();
  });

  it("upgrade routes a free user to Checkout with the chosen plan", async () => {
    stage({ limits: FREE_LIMITS, plan: { catalog: CATALOG, current: { code: "free", status: "inactive", has_stripe_customer: false } } });
    renderPage();

    const btn = await screen.findByRole("button", { name: "Upgrade to Scale" });
    await userEvent.click(btn);

    // Routes to Checkout (not portal) carrying the selected plan code.
    await waitFor(() =>
      expect(mockFetch.mock.calls.some(([u]) => u === CHECKOUT_URL)).toBe(true),
    );
    const call = mockFetch.mock.calls.find(([u]) => u === CHECKOUT_URL);
    expect(call![1].method).toBe("POST");
    expect(JSON.parse(call![1].body)).toEqual({ plan: "scale" });
    expect(mockFetch.mock.calls.some(([u]) => u === PORTAL_URL)).toBe(false);
  });

  it("offers Switch/Downgrade via the portal for an active subscriber", async () => {
    stage({ limits: PRO_LIMITS, plan: { catalog: CATALOG, current: { code: "pro", status: "active", has_stripe_customer: true } } });
    renderPage();

    await waitFor(() => expect(screen.getByText("Current")).toBeInTheDocument());
    // Pro is current (no button); Scale = switch up, Free = downgrade.
    const switchBtn = screen.getByRole("button", { name: "Switch to Scale" });
    expect(switchBtn).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Downgrade" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Switch to Pro|Upgrade/ })).not.toBeInTheDocument();

    // A switch routes to the Stripe portal (upgrade_url), not checkout.
    await userEvent.click(switchBtn);
    await waitFor(() =>
      expect(
        mockFetch.mock.calls.some(([u, i]) => u === PORTAL_URL && i?.method === "POST"),
      ).toBe(true),
    );
    expect(mockFetch.mock.calls.some(([u]) => u === CHECKOUT_URL)).toBe(false);
  });

  it("offers no plan-change actions when the current plan can't be determined", async () => {
    // Fail-safe path: both the sidecar's current.code and the OSS
    // plan_code are empty, so currentCode resolves to "". No tier should
    // be marked current and no Upgrade/Switch/Downgrade button should
    // render — we don't act on an unknown current plan.
    stage({
      limits: { ...FREE_LIMITS, plan_code: "" },
      plan: { catalog: CATALOG, current: { code: "", status: "inactive", has_stripe_customer: false } },
    });
    renderPage();

    await waitFor(() => expect(screen.getByText("Plans")).toBeInTheDocument());
    expect(screen.queryByText("Current")).not.toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: /Upgrade|Switch|Downgrade/ }),
    ).not.toBeInTheDocument();
  });

  it("degrades gracefully when the plan catalog fails to load", async () => {
    stage({ limits: FREE_LIMITS, planFails: true });
    renderPage();

    // Usage still renders (it comes from /v1/account, a separate fetch).
    await waitFor(() => expect(screen.getByText("Inboxes")).toBeInTheDocument());
    // The Plans section shows a retry notice rather than tier cards.
    expect(screen.getByText(/Couldn't load plans/i)).toBeInTheDocument();
    expect(screen.queryByText("Scale")).not.toBeInTheDocument();
  });
});
