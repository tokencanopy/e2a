import { act, render, screen, waitFor, fireEvent } from "@testing-library/react";
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
// Free daily cap. max_quantity is deliberately SMALL so the ceiling is
// reachable in one or two clicks (prod's is 1000).
const ADDON = {
  code: "addon_inbox",
  display_name: "Inbox add-on",
  monthly_price_cents_per_unit: 200,
  max_quantity: 3,
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
  // upgrade_url present == active subscription; it is also the portal POST target.
  upgrade_url: PORTAL_URL,
};

function proPlan(addonQuantity: number) {
  return {
    catalog: CATALOG,
    addon: ADDON,
    current: {
      code: "pro",
      status: "active",
      has_stripe_customer: true,
      addon_quantity: addonQuantity,
    },
  };
}

const mockFetch = jest.fn();
// The plan payload is a mutable `let` so sync tests can flip it
// mid-flight and watch the page's polling pick the change up — the
// same pattern page.refresh.test.tsx uses.
let planPayload: unknown;
let limitsPayload: unknown;
let addonResponse: { url?: string; updated?: boolean };
let addonFails: { status: number; body: string } | null;

beforeEach(() => {
  mockFetch.mockReset();
  addonResponse = { url: "https://stripe.test/checkout" };
  addonFails = null;
  mockFetch.mockImplementation((url: string, init?: RequestInit) => {
    if (url === "/v1/account") {
      return Promise.resolve({ ok: true, json: () => Promise.resolve(limitsPayload) });
    }
    if (url === PLAN_URL) {
      return Promise.resolve({ ok: true, json: () => Promise.resolve(planPayload) });
    }
    if (url === ADDON_URL && init?.method === "POST") {
      const failure = addonFails;
      if (failure) {
        return Promise.resolve({
          ok: false,
          status: failure.status,
          text: () => Promise.resolve(failure.body),
        });
      }
      return Promise.resolve({ ok: true, json: () => Promise.resolve(addonResponse) });
    }
    return Promise.resolve({ ok: false, status: 404, text: () => Promise.resolve("404") });
  });
  global.fetch = mockFetch;
});

beforeAll(() => {
  window.alert = jest.fn();
  // In-place increases ask for confirmation before charging; default to
  // accepting so most tests exercise the post path. The decline test
  // overrides per-call.
  window.confirm = jest.fn(() => true);
});

beforeEach(() => {
  (window.alert as jest.Mock).mockClear();
  (window.confirm as jest.Mock).mockClear();
  (window.confirm as jest.Mock).mockImplementation(() => true);
});

function renderPage() {
  return render(
    <SWRConfig value={{ provider: () => new Map(), dedupingInterval: 0 }}>
      <BillingPage />
    </SWRConfig>,
  );
}

function addonPosts(): { quantity: number }[] {
  return mockFetch.mock.calls
    .filter(([u, init]: [string, RequestInit?]) => u === ADDON_URL && init?.method === "POST")
    .map(([, init]: [string, RequestInit]) => JSON.parse(init.body as string));
}

describe("BillingPage — inbox add-on", () => {
  it("renders the add-on card from the catalog payload", async () => {
    limitsPayload = FREE_LIMITS;
    planPayload = {
      catalog: CATALOG,
      addon: ADDON,
      current: { code: "free", status: "inactive", has_stripe_customer: false, addon_quantity: 0 },
    };
    renderPage();

    await waitFor(() => expect(screen.getByText("Inbox add-on")).toBeInTheDocument());
    // Price and per-unit adds come from the payload, never hardcoded.
    expect(screen.getByText(/\$2\/mo each/)).toBeInTheDocument();
    expect(screen.getByText(/adds 1 inbox and 3,000 sends/)).toBeInTheDocument();
    // Free plan + lifts_free_daily_cap → the daily-cap hint shows.
    expect(screen.getByText(/daily send cap/)).toBeInTheDocument();
  });

  it("omits the daily-cap hint on a paid plan", async () => {
    limitsPayload = PRO_LIMITS;
    planPayload = proPlan(0);
    renderPage();

    await screen.findByText("Inbox add-on");
    expect(screen.queryByText(/daily send cap/)).not.toBeInTheDocument();
  });

  it("hides the card when the sidecar payload has no addon entry", async () => {
    limitsPayload = FREE_LIMITS;
    planPayload = {
      catalog: CATALOG,
      current: { code: "free", status: "inactive", has_stripe_customer: false },
    };
    renderPage();

    await waitFor(() => expect(screen.getByText("Plans")).toBeInTheDocument());
    expect(screen.queryByText("Inbox add-on")).not.toBeInTheDocument();
  });

  it("buys add-ons via Checkout for a user with no subscription", async () => {
    limitsPayload = FREE_LIMITS;
    planPayload = {
      catalog: CATALOG,
      addon: ADDON,
      current: { code: "free", status: "inactive", has_stripe_customer: false, addon_quantity: 0 },
    };
    renderPage();

    await screen.findByText("Inbox add-on");
    // Step 0 → 2, then buy. The proposed total shows before commit.
    const inc = screen.getByRole("button", { name: "Increase add-on quantity" });
    await userEvent.click(inc);
    await userEvent.click(inc);
    expect(screen.getByText(/New total:/)).toHaveTextContent("$4/mo");
    await userEvent.click(screen.getByRole("button", { name: "Buy add-ons" }));

    await waitFor(() => expect(addonPosts()).toEqual([{ quantity: 2 }]));
    // Took the redirect branch: no in-place provisioning notice, no
    // confirm dialog (Stripe's page shows the price), no error.
    expect(screen.queryByText(/Updating your add-ons/)).not.toBeInTheDocument();
    expect(window.confirm).not.toHaveBeenCalled();
    expect(window.alert).not.toHaveBeenCalled();
  });

  it("updates quantity in place for a subscriber after confirmation", async () => {
    limitsPayload = PRO_LIMITS;
    planPayload = proPlan(1);
    addonResponse = { updated: true };
    renderPage();

    await screen.findByText("Inbox add-on");
    // Current quantity from the payload.
    expect(screen.getByLabelText("Add-on quantity")).toHaveValue(1);

    await userEvent.click(screen.getByRole("button", { name: "Increase add-on quantity" }));
    await userEvent.click(screen.getByRole("button", { name: "Update add-ons" }));

    // An in-place increase charges immediately → the confirm carries the
    // new total.
    expect(window.confirm).toHaveBeenCalledWith(expect.stringContaining("$4/mo"));
    await waitFor(() => expect(addonPosts()).toEqual([{ quantity: 2 }]));
    // No redirect; the page reports the webhook is provisioning and the
    // action is locked for the whole pending window — a live button here
    // is how duplicate charges happen.
    await waitFor(() =>
      expect(screen.getByText(/Updating your add-ons/)).toBeInTheDocument(),
    );
    expect(screen.getByRole("button", { name: "Updating…" })).toBeDisabled();
  });

  it("declining the confirmation sends nothing", async () => {
    limitsPayload = PRO_LIMITS;
    planPayload = proPlan(1);
    (window.confirm as jest.Mock).mockImplementation(() => false);
    renderPage();

    await screen.findByText("Inbox add-on");
    await userEvent.click(screen.getByRole("button", { name: "Increase add-on quantity" }));
    await userEvent.click(screen.getByRole("button", { name: "Update add-ons" }));

    expect(window.confirm).toHaveBeenCalled();
    expect(addonPosts()).toEqual([]);
    // Declining leaves the page interactive.
    expect(screen.getByRole("button", { name: "Update add-ons" })).not.toBeDisabled();
  });

  it("disables the action when the desired quantity equals the current one", async () => {
    limitsPayload = PRO_LIMITS;
    planPayload = proPlan(3);
    renderPage();

    await screen.findByText("Inbox add-on");
    expect(screen.getByRole("button", { name: "Update add-ons" })).toBeDisabled();
    expect(screen.getByLabelText("Add-on quantity")).toHaveValue(3);
  });

  it("clamps the stepper to the catalog ceiling and the zero floor", async () => {
    limitsPayload = PRO_LIMITS;
    planPayload = proPlan(2);
    renderPage();

    await screen.findByText("Inbox add-on");
    const inc = screen.getByRole("button", { name: "Increase add-on quantity" });
    const dec = screen.getByRole("button", { name: "Decrease add-on quantity" });

    // 2 → 3 = max_quantity → + disables.
    await userEvent.click(inc);
    expect(screen.getByLabelText("Add-on quantity")).toHaveValue(3);
    expect(inc).toBeDisabled();

    // Down to the floor: 3 → 0 → − disables.
    await userEvent.click(dec);
    await userEvent.click(dec);
    await userEvent.click(dec);
    expect(screen.getByLabelText("Add-on quantity")).toHaveValue(0);
    expect(dec).toBeDisabled();

    // Typing past the ceiling clamps too.
    const input = screen.getByLabelText("Add-on quantity");
    fireEvent.change(input, { target: { value: "999" } });
    expect(input).toHaveValue(3);
  });

  it("does not stage a cancel-everything when the input is cleared mid-retype", async () => {
    limitsPayload = PRO_LIMITS;
    planPayload = proPlan(2);
    renderPage();

    await screen.findByText("Inbox add-on");
    const input = screen.getByLabelText("Add-on quantity");
    // Backspacing to empty must hold the previous value rather than
    // arming a one-click "set to 0".
    fireEvent.change(input, { target: { value: "" } });
    expect(input).toHaveValue(2);
    expect(screen.getByRole("button", { name: "Update add-ons" })).toBeDisabled();
    expect(addonPosts()).toEqual([]);
  });

  it("surfaces an error and re-enables the button when the POST fails", async () => {
    limitsPayload = PRO_LIMITS;
    planPayload = proPlan(1);
    addonFails = { status: 502, body: "add-on change failed" };
    renderPage();

    await screen.findByText("Inbox add-on");
    await userEvent.click(screen.getByRole("button", { name: "Decrease add-on quantity" }));
    await userEvent.click(screen.getByRole("button", { name: "Update add-ons" }));

    await waitFor(() => expect(window.alert).toHaveBeenCalled());
    // Failure clears the in-flight state so the user can retry.
    expect(screen.getByRole("button", { name: "Update add-ons" })).not.toBeDisabled();
  });
});

describe("BillingPage — add-on provisioning sync", () => {
  beforeEach(() => {
    jest.useFakeTimers();
  });
  afterEach(() => {
    jest.useRealTimers();
  });

  async function stageAndApply(user: ReturnType<typeof userEvent.setup>) {
    await screen.findByText("Inbox add-on");
    await user.click(screen.getByRole("button", { name: "Increase add-on quantity" }));
    await user.click(screen.getByRole("button", { name: "Update add-ons" }));
    await waitFor(() =>
      expect(screen.getByText(/Updating your add-ons/)).toBeInTheDocument(),
    );
  }

  it("polls until the webhook lands, then unlocks and tracks the server", async () => {
    const user = userEvent.setup({ advanceTimers: jest.advanceTimersByTime });
    limitsPayload = PRO_LIMITS;
    planPayload = proPlan(1);
    addonResponse = { updated: true };
    renderPage();

    await stageAndApply(user);

    // Webhook lands between polls: the stored quantity reaches the target.
    planPayload = proPlan(2);
    await act(async () => {
      await jest.advanceTimersByTimeAsync(2000);
    });

    await waitFor(() =>
      expect(screen.queryByText(/Updating your add-ons/)).not.toBeInTheDocument(),
    );
    // Input tracks the server again and the action re-locks as a no-op.
    expect(screen.getByLabelText("Add-on quantity")).toHaveValue(2);
    expect(screen.getByRole("button", { name: "Update add-ons" })).toBeDisabled();
  });

  it("shows the timeout notice when provisioning outlasts the window", async () => {
    const user = userEvent.setup({ advanceTimers: jest.advanceTimersByTime });
    limitsPayload = PRO_LIMITS;
    planPayload = proPlan(1);
    addonResponse = { updated: true };
    renderPage();

    await stageAndApply(user);

    // The webhook never lands; the deadline passes.
    await act(async () => {
      await jest.advanceTimersByTimeAsync(21000);
    });

    await waitFor(() =>
      expect(screen.getByText(/haven't appeared yet|hasn't|keeps checking/)).toBeInTheDocument(),
    );
    expect(screen.queryByText(/Updating your add-ons/)).not.toBeInTheDocument();
  });
});
