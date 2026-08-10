import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { SWRConfig } from "swr";
import MetricsPage from "./page";

// SWR's cache is module-global, so without an isolated provider per test the
// second test would render the first test's payload and never call fetch.
function renderPage() {
  return render(
    <SWRConfig value={{ provider: () => new Map(), dedupingInterval: 0 }}>
      <MetricsPage />
    </SWRConfig>,
  );
}

const fetchMock = jest.fn();
beforeEach(() => {
  fetchMock.mockReset();
  global.fetch = fetchMock as unknown as typeof fetch;
});

function summary(over: Record<string, number> = {}) {
  return {
    accepted: 0, submitted: 0, delivered: 0,
    bounced_hard: 0, bounced_soft: 0, bounced_undetermined: 0,
    complained: 0, suppressed: 0, send_failed: 0, loopback: 0,
    received: 0, dmarc_pass: 0, dmarc_fail: 0, dmarc_none: 0, dmarc_error: 0,
    review_held: 0, review_approved: 0, review_rejected: 0,
    review_expired_approved: 0, review_expired_rejected: 0,
    ...over,
  };
}

function rates(over: Record<string, number | null> = {}) {
  return {
    delivered_rate: null, bounce_rate: null, complaint_rate: null,
    suppression_block_rate: null,
    ...over,
  };
}

function body(over: Record<string, unknown> = {}) {
  return {
    start: "2026-07-09T00:00:00Z",
    end: "2026-08-08T00:00:00Z",
    messages_in_window: 12480,
    messages_with_lifecycle: 12480,
    reconstructed_observations: 0,
    summary: summary({
      accepted: 12480, submitted: 12290, delivered: 12201,
      bounced_hard: 71, bounced_soft: 18, complained: 3, suppressed: 41,
      received: 3204, dmarc_pass: 3101, dmarc_fail: 12, dmarc_none: 91,
      review_held: 170, review_approved: 158, review_rejected: 12,
    }),
    rates: rates({
      delivered_rate: 0.9776, bounce_rate: 0.00724,
      complaint_rate: 0.000246, suppression_block_rate: 0.00328,
    }),
    counters: [
      { reason_code: "delivery.permanent_bounce", stage: "delivery", outcome: "bounced", retryable: false, observations: 71, messages: 71 },
    ],
    agents: [
      {
        agent_email: "support@agents.localhost",
        messages_in_window: 4102,
        messages_with_lifecycle: 4102,
        reconstructed_observations: 0,
        summary: summary({ accepted: 2051, delivered: 2001, bounced_hard: 48 }),
        rates: rates({ delivered_rate: 0.9756 }),
        counters: [],
      },
    ],
    agents_truncated: false,
    webhooks: webhooks(),
    buckets: [],
    ...over,
  };
}

function webhooks(over: Record<string, unknown> = {}) {
  return {
    deliveries: 0, delivered: 0, pending: 0,
    endpoint_rejected: 0, no_response: 0,
    success_rate: null, window_exceeds_retention: false,
    endpoints_auto_disabled: 0, endpoints: [], endpoints_truncated: false,
    ...over,
  };
}

function respond(payload: unknown) {
  fetchMock.mockResolvedValue({ ok: true, status: 200, json: async () => payload });
}

it("renders headline rates with their denominators spelled out", async () => {
  respond(body());
  renderPage();

  expect(await screen.findByText("97.8%")).toBeInTheDocument();
  // The denominator is printed because delivered_rate is over accepted while
  // bounce_rate is over submitted — the page has to show why they differ.
  expect(screen.getByText("12,201 of 12,480 accepted")).toBeInTheDocument();
  expect(screen.getByText("89 of 12,290 submitted")).toBeInTheDocument();
  expect(screen.getByText("3 of 12,201 delivered")).toBeInTheDocument();
});

// A null rate means the denominator was zero. Rendered as 0% it reads as
// total delivery failure, which is the opposite of "nothing was sent".
it("renders a null rate as an em dash, never as 0%", async () => {
  respond(
    body({
      messages_in_window: 5,
      summary: summary({ accepted: 5 }),
      rates: rates({ delivered_rate: 0, bounce_rate: null }),
      agents: [],
      counters: [],
    }),
  );
  renderPage();

  await screen.findByText("0.0%"); // a real zero still shows as a percentage
  expect(screen.getAllByText("—").length).toBeGreaterThan(0);
});

// Regression: with accepted=0 the funnel had no baseline, so shortfall was
// computed as 100% and the whole track painted red — zero traffic rendered as
// total delivery failure, contradicting the tiles' em dash. Hit whenever an
// account's messages predate the lifecycle ledger.
it("draws no loss bar when there is no outbound baseline to lose against", async () => {
  respond(
    body({
      messages_in_window: 24,
      messages_with_lifecycle: 2,
      summary: summary(),
      rates: rates(),
      agents: [],
      counters: [],
    }),
  );
  const { container } = renderPage();

  await screen.findByText(/22 of 24 messages have no lifecycle record/);
  expect(screen.getByText("No outbound mail with a lifecycle record in this window.")).toBeInTheDocument();

  const danger = Array.from(container.querySelectorAll<HTMLElement>("div[style*='--danger']"));
  // No loss segment may be drawn without a baseline.
  expect(danger).toHaveLength(0);
});

it("still draws the loss bar when a real baseline exists", async () => {
  respond(
    body({
      summary: summary({ accepted: 100, submitted: 60, delivered: 50 }),
      rates: rates({ delivered_rate: 0.5 }),
      agents: [],
      counters: [],
    }),
  );
  const { container } = renderPage();

  await screen.findByText("50.0%");
  const danger = Array.from(container.querySelectorAll<HTMLElement>("div[style*='--danger']"));
  expect(danger.length).toBeGreaterThan(0);
});

it("shows an empty state instead of a wall of dashes for a new account", async () => {
  respond(body({ messages_in_window: 0, summary: summary(), rates: rates(), agents: [], counters: [] }));
  renderPage();

  expect(await screen.findByText("No mail in this window yet.")).toBeInTheDocument();
  expect(screen.getByRole("link", { name: /Get started/ })).toBeInTheDocument();
});

it("warns that a ledger gap is a recording gap, not lost mail", async () => {
  respond(body({ messages_in_window: 12480, messages_with_lifecycle: 12000 }));
  renderPage();

  expect(
    await screen.findByText(/480 of 12,480 messages have no lifecycle record/),
  ).toBeInTheDocument();
  expect(screen.getByText(/recording gap, not lost mail/)).toBeInTheDocument();
});

it("lists inboxes busiest first and links each to its messages", async () => {
  respond(body());
  renderPage();

  const link = await screen.findByRole("link", { name: "support@agents.localhost" });
  expect(link).toHaveAttribute(
    "href",
    "/inboxes/messages?email=support%40agents.localhost",
  );
});

it("says so when the per-inbox breakdown is truncated", async () => {
  respond(body({ agents_truncated: true }));
  renderPage();

  await screen.findByText("97.8%");
  expect(
    screen.getByText(/More inboxes have traffic than are listed/),
  ).toBeInTheDocument();
});

it("keeps the reason-code detail collapsed until asked for", async () => {
  respond(body());
  renderPage();

  await screen.findByText("97.8%");
  expect(screen.queryByText("delivery.permanent_bounce")).not.toBeInTheDocument();

  await userEvent.click(screen.getByRole("button", { name: /Show reason-code detail/ }));
  expect(screen.getByText("delivery.permanent_bounce")).toBeInTheDocument();
});

it("requests the per-agent breakdown so one call feeds the whole page", async () => {
  respond(body());
  renderPage();

  await screen.findByText("97.8%");
  const urls = fetchMock.mock.calls.map((c) => String(c[0]));
  expect(urls.some((u) => u.includes("/v1/metrics") && u.includes("group_by=agent"))).toBe(true);
});

it("labels every authentication segment so colour is never the only cue", async () => {
  respond(body());
  renderPage();

  await screen.findByText("97.8%");
  expect(screen.getByText(/Pass 3,101/)).toBeInTheDocument();
  expect(screen.getByText(/No policy 91/)).toBeInTheDocument();
  expect(screen.getByText(/Fail 12/)).toBeInTheDocument();
});

describe("metric tooltips", () => {
  it("explains a metric on demand and names its denominator", async () => {
    respond(body());
    renderPage();
    await screen.findByText("97.8%");

    const trigger = screen.getByRole("button", { name: "What is the bounce rate?" });
    expect(screen.queryByRole("tooltip")).not.toBeInTheDocument();

    await userEvent.click(trigger);
    const tip = screen.getByRole("tooltip");
    expect(within(tip).getByText(/share of what was actually submitted/)).toBeInTheDocument();
  });

  it("is reachable by keyboard, not hover only", async () => {
    respond(body());
    renderPage();
    await screen.findByText("97.8%");

    const trigger = screen.getByRole("button", { name: "What is the delivered rate?" });
    trigger.focus();
    await waitFor(() => expect(screen.getByRole("tooltip")).toBeInTheDocument());
    expect(trigger).toHaveAttribute("aria-describedby");
  });

  it("closes on Escape", async () => {
    respond(body());
    renderPage();
    await screen.findByText("97.8%");

    await userEvent.click(screen.getByRole("button", { name: "What is the complaint rate?" }));
    expect(screen.getByRole("tooltip")).toBeInTheDocument();
    await userEvent.keyboard("{Escape}");
    await waitFor(() => expect(screen.queryByRole("tooltip")).not.toBeInTheDocument());
  });

  it("offers a definition for every headline tile", async () => {
    respond(body());
    renderPage();
    await screen.findByText("97.8%");

    for (const label of [
      "the delivered rate", "the bounce rate", "the complaint rate", "the suppression rate",
    ]) {
      expect(screen.getByRole("button", { name: `What is ${label}?` })).toBeInTheDocument();
    }
  });
});

describe("webhook delivery panel", () => {
  it("stays hidden for an account with no webhooks", async () => {
    respond(body());
    renderPage();
    await screen.findByText("97.8%");
    expect(screen.queryByText("Webhook delivery")).not.toBeInTheDocument();
  });

  it("separates an endpoint that answered from one that never did", async () => {
    respond(
      body({
        webhooks: webhooks({
          deliveries: 102, delivered: 96, pending: 2,
          endpoint_rejected: 3, no_response: 1, success_rate: 0.96,
          endpoints: [{
            webhook_id: "wh_1", url_host: "hooks.example.test",
            deliveries: 102, delivered: 96, pending: 2,
            endpoint_rejected: 3, no_response: 1, success_rate: 0.96,
            last_status_code: 405, enabled: true,
            auto_disabled_at: null, auto_disable_reason: "",
          }],
        }),
      }),
    );
    renderPage();
    await screen.findByText("Webhook delivery");
    expect(screen.getByText(/endpoint answered non-2xx 3 · no response 1/)).toBeInTheDocument();
    // Pending is excluded from the denominator.
    expect(screen.getByText(/96 of 100 settled/)).toBeInTheDocument();
    expect(screen.getByText("405")).toBeInTheDocument();
    expect(screen.getByText("hooks.example.test")).toBeInTheDocument();
  });

  // An auto-disabled endpoint is dropping events, not retrying them — the most
  // urgent state the page can show, so it must be stated, not just tinted.
  it("calls out auto-disabled endpoints in words", async () => {
    respond(
      body({
        webhooks: webhooks({
          deliveries: 10, delivered: 2, endpoint_rejected: 8, success_rate: 0.2,
          endpoints_auto_disabled: 1,
          endpoints: [{
            webhook_id: "wh_dead", url_host: "dead.example.test",
            deliveries: 10, delivered: 2, pending: 0,
            endpoint_rejected: 8, no_response: 0, success_rate: 0.2,
            last_status_code: 500, enabled: false,
            auto_disabled_at: "2026-08-01T00:00:00Z",
            auto_disable_reason: "sustained_failure",
          }],
        }),
      }),
    );
    renderPage();
    await screen.findByText("Webhook delivery");
    expect(screen.getByText(/1 endpoint is auto-disabled/)).toBeInTheDocument();
    expect(screen.getByText(/events are being\s+dropped, not retried/)).toBeInTheDocument();
    expect(screen.getByText("auto-disabled")).toBeInTheDocument();
  });

  it("shows the panel for a disabled endpoint even with no recent deliveries", async () => {
    respond(body({ webhooks: webhooks({ endpoints_auto_disabled: 1 }) }));
    renderPage();
    await screen.findByText("Webhook delivery");
  });

  it("explains the 30-day delivery retention horizon", async () => {
    respond(
      body({ webhooks: webhooks({ deliveries: 5, delivered: 5, success_rate: 1, window_exceeds_retention: true }) }),
    );
    renderPage();
    await screen.findByText("Webhook delivery");
    expect(screen.getByText(/Delivery history is kept 30 days/)).toBeInTheDocument();
  });
});

// The API, CLI, and MCP tools all announce beta; the dashboard must too, or a
// reader has no signal these definitions can still change under them.
it("marks the page as beta", async () => {
  respond(body());
  renderPage();
  await screen.findByText("97.8%");
  expect(screen.getByText("Beta")).toBeInTheDocument();
  expect(
    screen.getByText(/definitions may change while this is in beta/),
  ).toBeInTheDocument();
});

describe("delivery-rate trend", () => {
  const bucket = (day: string, rate: number | null, accepted: number) => ({
    day,
    summary: summary({ accepted, delivered: rate === null ? 0 : Math.round(accepted * rate) }),
    rates: rates({ delivered_rate: rate }),
  });

  it("requests daily buckets so the chart has something to draw", async () => {
    respond(body());
    renderPage();
    await screen.findByText("97.8%");
    const urls = fetchMock.mock.calls.map((c) => String(c[0]));
    expect(urls.some((u) => u.includes("bucket=day"))).toBe(true);
  });

  it("stays hidden until there are at least two days", async () => {
    respond(body({ buckets: [bucket("2026-08-01T00:00:00Z", 0.97, 100)] }));
    renderPage();
    await screen.findByText("97.8%");
    expect(screen.queryByText("Delivery rate over time")).not.toBeInTheDocument();
  });

  // A null rate means no sends that day. Plotting it as 0% would draw a crash
  // that never happened, so the line must break instead.
  it("breaks the line on a day with no sends rather than plotting zero", async () => {
    respond(
      body({
        buckets: [
          bucket("2026-08-01T00:00:00Z", 0.98, 100),
          bucket("2026-08-02T00:00:00Z", 0.97, 110),
          bucket("2026-08-03T00:00:00Z", null, 0), // silent day
          bucket("2026-08-04T00:00:00Z", 0.96, 90),
          bucket("2026-08-05T00:00:00Z", 0.99, 95),
        ],
      }),
    );
    const { container } = renderPage();
    await screen.findByText("Delivery rate over time");

    // Two separate runs — never one path interpolated across the gap, which
    // would imply a rate on a day that had no sends.
    const paths = Array.from(container.querySelectorAll("path[stroke*='--accent']"));
    expect(paths).toHaveLength(2);
    for (const p of paths) {
      expect(p.getAttribute("d") ?? "").not.toMatch(/M.*M/);
    }
  });

  // A day whose neighbours are both silent has no line to sit on, so it must
  // render as a point rather than disappearing from the chart.
  it("draws an isolated day as a dot", async () => {
    respond(
      body({
        buckets: [
          bucket("2026-08-01T00:00:00Z", null, 0),
          bucket("2026-08-02T00:00:00Z", 0.97, 110),
          bucket("2026-08-03T00:00:00Z", null, 0),
        ],
      }),
    );
    const { container } = renderPage();
    await screen.findByText("Delivery rate over time");
    expect(container.querySelectorAll("circle[fill*='--accent']").length).toBeGreaterThanOrEqual(1);
    expect(container.querySelectorAll("path[stroke*='--accent']")).toHaveLength(0);
  });

  it("labels the axis range so the chart is readable without hovering", async () => {
    respond(
      body({
        buckets: [
          bucket("2026-08-01T00:00:00Z", 0.98, 100),
          bucket("2026-08-02T00:00:00Z", 0.96, 120),
        ],
      }),
    );
    renderPage();
    await screen.findByText("Delivery rate over time");
    expect(screen.getByText("100%")).toBeInTheDocument();
    expect(screen.getByText("1 Aug")).toBeInTheDocument();
    expect(screen.getByText("2 Aug")).toBeInTheDocument();
  });
});
