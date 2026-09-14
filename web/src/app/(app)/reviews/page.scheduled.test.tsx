// Scheduled tab contract: the Pending page's second tab renders the
// account-scoped GET /v1/scheduled queue (outbound messages accepted and
// awaiting a future send_at), read-only, with a "Sends …" label. These are
// NOT holds — no approve/reject affordance appears on a scheduled row.

import { render, screen, waitFor, fireEvent } from "@testing-library/react";
import { mutate, SWRConfig } from "swr";
import PendingPage from "./page";

// Each test gets a fresh SWR cache with dedup disabled so cross-test key reuse
// (both tabs fetch on mount) can't leak a prior test's cached page into the next.
function renderPage() {
  return render(
    <SWRConfig value={{ provider: () => new Map(), dedupingInterval: 0 }}>
      <PendingPage />
    </SWRConfig>,
  );
}

// Land directly on the Scheduled tab (?tab=scheduled), no deep-linked hold.
let mockTabParam: string | null = "scheduled";
jest.mock("next/navigation", () => ({
  useSearchParams: () => ({
    get: (k: string) => (k === "tab" ? mockTabParam : null),
  }),
  useRouter: () => ({ push: jest.fn(), replace: jest.fn() }),
}));

const mockFetch = jest.fn();
global.fetch = mockFetch;

const AGENT_EMAIL = "ag_1@agents.e2a.dev";

// A ScheduledMessageView row from GET /v1/scheduled.
const SCHEDULED_ROW = {
  id: "msg_sched_1",
  agent_email: AGENT_EMAIL,
  direction: "outbound",
  header_from: AGENT_EMAIL,
  to: ["alice@example.com"],
  subject: "Quarterly update",
  delivery_status: "accepted",
  created_at: "2026-05-23T00:00:00Z",
  scheduled_at: "2099-01-01T09:00:00Z",
};

function stageFetch(scheduledItems: unknown[]) {
  mockFetch.mockImplementation((url: string) => {
    if (url === "/v1/scheduled-messages") {
      return Promise.resolve({
        ok: true,
        status: 200,
        text: () =>
          Promise.resolve(JSON.stringify({ items: scheduledItems, next_cursor: null })),
      });
    }
    // The holds tab's /v1/reviews fetch also fires on mount; keep it empty.
    if (url === "/v1/reviews") {
      return Promise.resolve({
        ok: true,
        status: 200,
        text: () => Promise.resolve(JSON.stringify({ items: [], next_cursor: null })),
      });
    }
    return Promise.resolve({ ok: false, status: 404, text: () => Promise.resolve("nf") });
  });
}

beforeEach(async () => {
  mockFetch.mockReset();
  mockTabParam = "scheduled";
  await mutate(() => true, undefined, { revalidate: false });
});

describe("Pending page — Scheduled tab", () => {
  it("renders scheduled sends from /v1/scheduled with a Sends label", async () => {
    stageFetch([SCHEDULED_ROW]);

    renderPage();

    await waitFor(() => {
      expect(screen.getByText("Quarterly update")).toBeInTheDocument();
    });
    // Read-only: the row shows when it sends, not an approve/reject bar.
    expect(screen.getByText(/^Sends /)).toBeInTheDocument();
    expect(screen.queryByText(/Approve/)).not.toBeInTheDocument();
    expect(screen.getByTestId("scheduled-row")).toBeInTheDocument();
  });

  it("expands a scheduled row to show the message body read-only (no approve/reject)", async () => {
    mockFetch.mockImplementation((url: string) => {
      if (url === "/v1/scheduled-messages") {
        return Promise.resolve({
          ok: true,
          status: 200,
          text: () =>
            Promise.resolve(JSON.stringify({ items: [SCHEDULED_ROW], next_cursor: null })),
        });
      }
      if (url === "/v1/reviews") {
        return Promise.resolve({
          ok: true,
          status: 200,
          text: () => Promise.resolve(JSON.stringify({ items: [], next_cursor: null })),
        });
      }
      // The lazy detail fetch fired on row expand.
      if (url.includes("/messages/msg_sched_1")) {
        return Promise.resolve({
          ok: true,
          status: 200,
          text: () =>
            Promise.resolve(
              JSON.stringify({
                id: "msg_sched_1",
                direction: "outbound",
                to: ["alice@example.com"],
                subject: "Quarterly update",
                delivery_status: "accepted",
                created_at: "2026-05-23T00:00:00Z",
                scheduled_at: "2099-01-01T09:00:00Z",
                body: { text: "The full scheduled body text." },
              }),
            ),
        });
      }
      return Promise.resolve({ ok: false, status: 404, text: () => Promise.resolve("nf") });
    });

    renderPage();

    await waitFor(() => {
      expect(screen.getByText("Quarterly update")).toBeInTheDocument();
    });
    // Body is not fetched/shown until the row is opened.
    expect(screen.queryByText(/The full scheduled body text/)).not.toBeInTheDocument();

    fireEvent.click(screen.getByTestId("scheduled-row"));

    await waitFor(() => {
      expect(screen.getByText(/The full scheduled body text/)).toBeInTheDocument();
    });
    // Read-only: still no approve/reject affordance.
    expect(screen.queryByText(/Approve/)).not.toBeInTheDocument();
    expect(screen.queryByText(/Reject/)).not.toBeInTheDocument();
  });

  it("flags an overdue-but-pending scheduled send instead of a Sends label", async () => {
    // A send whose fire time has passed but is still accepted (e.g. deferred by
    // the daily send cap). GET /v1/scheduled now surfaces it; the row frames it
    // as overdue rather than hiding it until it fires.
    stageFetch([
      { ...SCHEDULED_ROW, id: "msg_overdue_1", scheduled_at: "2020-01-01T09:00:00Z" },
    ]);

    renderPage();

    await waitFor(() => {
      expect(screen.getByText("Quarterly update")).toBeInTheDocument();
    });
    expect(screen.getByText(/^Overdue · was due /)).toBeInTheDocument();
    expect(screen.queryByText(/^Sends /)).not.toBeInTheDocument();
    // Still read-only — overdue is not a hold.
    expect(screen.queryByText(/Approve/)).not.toBeInTheDocument();
  });

  it("shows the empty state when nothing is scheduled", async () => {
    stageFetch([]);

    renderPage();

    await waitFor(() => {
      expect(screen.getByText(/No scheduled messages/)).toBeInTheDocument();
    });
  });
});
