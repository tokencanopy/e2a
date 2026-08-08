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
    complained: 0, suppressed: 0, send_failed: 0,
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
    "/inboxes/messages?agent=support%40agents.localhost",
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
