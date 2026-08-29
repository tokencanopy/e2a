// Freshness behavior of the Billing page.
//
// Two customer-visible defects are pinned here:
//
//  1. The visible "Refresh" button only revalidated the usage read. The
//     plan label, the "Current" badge, and every tier CTA come from the
//     sidecar catalog, which it never refetched — so a user who had just
//     upgraded clicked Refresh, saw their old plan, and concluded the
//     page was broken.
//
//  2. Stripe Checkout returns the user to /billing?status=success via a
//     full page load. That load races the asynchronous
//     checkout.session.completed webhook that provisions the new limits,
//     and the redirect usually wins. Nothing on the page ever re-read
//     afterwards, so the old plan rendered until a manual reload.

import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { SWRConfig } from "swr";

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

// Set before requiring the page: BILLING_API is captured at module
// evaluation, and without it there is no sidecar catalog to reconcile.
process.env.NEXT_PUBLIC_BILLING_API = "https://billing.test";
// eslint-disable-next-line @typescript-eslint/no-require-imports
const BillingPage = require("./page").default as React.ComponentType;

const PLAN_URL = "https://billing.test/api/billing/plan";

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

// Pre-webhook: Stripe has taken the money but the sidecar hasn't
// provisioned yet, so it still reports the free tier as current.
const PENDING_PLAN = {
  catalog: CATALOG,
  current: { code: "free", status: "inactive", has_stripe_customer: true },
};

// Post-webhook: limits provisioned, subscription active.
const ACTIVE_PLAN = {
  catalog: CATALOG,
  current: { code: "pro", status: "active", has_stripe_customer: true },
};

const mockFetch = jest.fn();

// Mutable staging so a test can flip what the sidecar returns
// mid-reconcile, the way the webhook landing does in production.
let limitsPayload: unknown = FREE_LIMITS;
let planPayload: unknown = PENDING_PLAN;

beforeEach(() => {
  limitsPayload = FREE_LIMITS;
  planPayload = PENDING_PLAN;
  mockFetch.mockReset();
  mockFetch.mockImplementation((url: string) => {
    if (url === "/v1/account") {
      return Promise.resolve({ ok: true, json: () => Promise.resolve(limitsPayload) });
    }
    if (url === PLAN_URL) {
      return Promise.resolve({ ok: true, json: () => Promise.resolve(planPayload) });
    }
    return Promise.resolve({ ok: false, text: () => Promise.resolve("404") });
  });
  global.fetch = mockFetch;
  window.history.replaceState({}, "", "/billing");
});

function renderPage() {
  return render(
    <SWRConfig value={{ provider: () => new Map(), dedupingInterval: 0 }}>
      <BillingPage />
    </SWRConfig>,
  );
}

const countCallsTo = (url: string) =>
  mockFetch.mock.calls.filter(([u]) => u === url).length;

describe("BillingPage — manual refresh", () => {
  it("revalidates the plan catalog as well as usage", async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText("Plans")).toBeInTheDocument());
    const planCallsBefore = countCallsTo(PLAN_URL);
    const limitsCallsBefore = countCallsTo("/v1/account");

    fireEvent.click(screen.getByRole("button", { name: "Refresh" }));

    // Both reads must move. Refreshing usage while leaving the plan
    // cached is what made the button look like it did nothing after an
    // upgrade.
    await waitFor(() =>
      expect(countCallsTo(PLAN_URL)).toBeGreaterThan(planCallsBefore),
    );
    expect(countCallsTo("/v1/account")).toBeGreaterThan(limitsCallsBefore);
  });
});

describe("BillingPage — post-checkout reconciliation", () => {
  beforeEach(() => {
    jest.useFakeTimers();
  });
  afterEach(() => {
    jest.useRealTimers();
  });

  it("re-reads the sidecar until the upgrade webhook lands, then shows the new plan", async () => {
    window.history.replaceState({}, "", "/billing?status=success");
    renderPage();

    // Lands on the pre-webhook state: still Free, with a notice telling
    // the user the upgrade is being finalized rather than silently
    // showing them the plan they just paid to leave.
    await waitFor(() =>
      expect(screen.getByText(/Finalizing your purchase/i)).toBeInTheDocument(),
    );
    const callsAfterMount = countCallsTo(PLAN_URL);

    // The webhook hasn't landed yet — the page must keep asking.
    await act(async () => {
      await jest.advanceTimersByTimeAsync(2000);
    });
    expect(countCallsTo(PLAN_URL)).toBeGreaterThan(callsAfterMount);

    // Webhook lands between polls.
    planPayload = ACTIVE_PLAN;
    limitsPayload = { ...FREE_LIMITS, plan_code: "pro" };
    await act(async () => {
      await jest.advanceTimersByTimeAsync(2000);
    });

    await waitFor(() => expect(screen.getByText("Current")).toBeInTheDocument());
    expect(screen.queryByText(/Finalizing your purchase/i)).not.toBeInTheDocument();

    // And it stops once resolved — no unbounded polling of the sidecar.
    const callsAfterResolve = countCallsTo(PLAN_URL);
    await act(async () => {
      await jest.advanceTimersByTimeAsync(6000);
    });
    expect(countCallsTo(PLAN_URL)).toBe(callsAfterResolve);

    // The marker is consumed so a later remount (or a Back navigation)
    // doesn't restart the reconcile loop.
    expect(window.location.search).toBe("");
  });

  // The reconcile deadline is stamped once, into a ref, when the window
  // opens — deliberately not a local recomputed on every run of the effect
  // that owns the interval. That effect depends on SWR's bound mutators; if
  // a future SWR returned a fresh identity per render, a recomputed deadline
  // would be pushed forward on every re-render and the timeout would never
  // be reached, leaving this polling the billing sidecar indefinitely.
  //
  // This pins the wall-clock boundary from both sides: it must still be
  // waiting just before the window elapses, and must have given up just
  // after. A deadline that drifts fails the second assertion.
  it("holds the reconcile window to wall-clock time, then gives up", async () => {
    window.history.replaceState({}, "", "/billing?status=success");
    renderPage();

    await waitFor(() =>
      expect(screen.getByText(/Finalizing your purchase/i)).toBeInTheDocument(),
    );

    // Just inside the window, after many poll ticks and the re-renders
    // each resolved read causes — still waiting.
    await act(async () => {
      await jest.advanceTimersByTimeAsync(19000);
    });
    expect(screen.getByText(/Finalizing your purchase/i)).toBeInTheDocument();
    expect(screen.queryByText(/hasn't appeared yet/i)).not.toBeInTheDocument();

    // Just past it — given up, on schedule rather than whenever the effect
    // last happened to re-run.
    await act(async () => {
      await jest.advanceTimersByTimeAsync(3000);
    });
    await waitFor(() =>
      expect(screen.getByText(/hasn't appeared yet/i)).toBeInTheDocument(),
    );
  });

  it("gives up with an explanation when the webhook never lands", async () => {
    window.history.replaceState({}, "", "/billing?status=success");
    renderPage();

    await waitFor(() =>
      expect(screen.getByText(/Finalizing your purchase/i)).toBeInTheDocument(),
    );

    // Past the reconcile window with the sidecar still reporting free.
    await act(async () => {
      await jest.advanceTimersByTimeAsync(21000);
    });

    await waitFor(() =>
      expect(screen.getByText(/hasn't appeared yet/i)).toBeInTheDocument(),
    );
    expect(screen.queryByText(/Finalizing your purchase/i)).not.toBeInTheDocument();

    // Stopped polling rather than hammering the sidecar forever.
    const callsAfterGiveUp = countCallsTo(PLAN_URL);
    await act(async () => {
      await jest.advanceTimersByTimeAsync(6000);
    });
    expect(countCallsTo(PLAN_URL)).toBe(callsAfterGiveUp);
  });

  it("does not reconcile on an ordinary visit", async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText("Plans")).toBeInTheDocument());
    const callsAfterMount = countCallsTo(PLAN_URL);

    expect(screen.queryByText(/Finalizing your purchase/i)).not.toBeInTheDocument();

    // Well inside the 30s background cadence: nothing extra should fire.
    await act(async () => {
      await jest.advanceTimersByTimeAsync(6000);
    });
    expect(countCallsTo(PLAN_URL)).toBe(callsAfterMount);
  });
});
