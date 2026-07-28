// Imported from test-utils/swr, not raw RTL: this page is SWR-backed, and the
// wrapper gives each render a fresh cache with dedupingInterval 0. Without it
// consecutive tests reusing the same webhook id fall inside SWR's dedup window
// and silently reuse the previous test's cache entry instead of refetching.
import { render, screen, waitFor } from "../../../../test-utils/swr";
import WebhookDetailPage from "./page";

// The detail page addresses its resource by query param, not a dynamic route
// segment: the dashboard is a static export (next.config `output: "export"`)
// and the app contains zero dynamic segments. /webhooks/[id] would not build.
let searchParams = new URLSearchParams();
jest.mock("next/navigation", () => ({
  useSearchParams: () => searchParams,
  useRouter: () => ({ push: jest.fn(), back: jest.fn() }),
}));

const mockFetch = jest.fn();
beforeEach(() => {
  mockFetch.mockReset();
  global.fetch = mockFetch as unknown as typeof fetch;
  searchParams = new URLSearchParams();
});

function okJson(obj: unknown) {
  return Promise.resolve({
    ok: true,
    status: 200,
    text: () => Promise.resolve(JSON.stringify(obj)),
  });
}
function notFound() {
  return Promise.resolve({
    ok: false,
    status: 404,
    text: () => Promise.resolve("not found"),
  });
}

const webhook = {
  id: "wh_1",
  url: "https://app.example.com/inbox",
  description: "",
  events: ["email.received"],
  enabled: true,
  created_at: "2026-07-01T10:00:00Z",
};

function respond(handlers: { webhook?: () => unknown; deliveries?: () => unknown }) {
  mockFetch.mockImplementation((url: string) => {
    if (url.includes("/deliveries")) {
      return (handlers.deliveries ?? (() => okJson({ items: [], next_cursor: null })))();
    }
    return (handlers.webhook ?? (() => okJson(webhook)))();
  });
}

describe("webhook detail page", () => {
  it("renders the endpoint identity for the id in the query string", async () => {
    searchParams = new URLSearchParams("id=wh_1");
    respond({});

    render(<WebhookDetailPage />);
    await waitFor(() => {
      expect(screen.getByText(webhook.url)).toBeInTheDocument();
    });
    expect(screen.getByText("email.received")).toBeInTheDocument();
  });

  // An unscoped subscription receives every agent's events. This page is
  // where someone lands to audit one endpoint, so the state has to be as
  // loud here as it is on the list.
  it("calls out an unscoped subscription", async () => {
    searchParams = new URLSearchParams("id=wh_1");
    respond({ webhook: () => okJson({ ...webhook, filters: {} }) });

    render(<WebhookDetailPage />);
    await waitFor(() => {
      expect(screen.getByText(webhook.url)).toBeInTheDocument();
    });
    expect(screen.getByText("all agents")).toBeInTheDocument();
  });

  it("summarizes a scoped subscription", async () => {
    searchParams = new URLSearchParams("id=wh_1");
    respond({
      webhook: () =>
        okJson({ ...webhook, filters: { agent_emails: ["a@example.com"] } }),
    });

    render(<WebhookDetailPage />);
    await waitFor(() => {
      expect(screen.getByText("a@example.com")).toBeInTheDocument();
    });
    expect(screen.queryByText("all agents")).not.toBeInTheDocument();
  });

  // The id comes from a user-editable query string, and delivery rows outlive
  // the subscription they belong to — so "webhook is gone" is a reachable
  // state that must render cleanly rather than throw or half-render.
  it("renders a clean not-found state for an unknown id", async () => {
    searchParams = new URLSearchParams("id=wh_missing");
    respond({ webhook: () => notFound() });

    render(<WebhookDetailPage />);
    await waitFor(() => {
      expect(screen.getByText(/couldn't find that webhook/i)).toBeInTheDocument();
    });
    // No skeleton of a page around a missing resource.
    expect(screen.queryByText("Deliveries")).not.toBeInTheDocument();
  });

  describe("deliveries feed", () => {
    const delivery = {
      id: "whd_1",
      type: "email.received",
      status: "delivered",
      attempts: 1,
      next_retry_at: "2026-07-27T12:00:00Z",
      created_at: "2026-07-27T11:00:00Z",
      last_attempt_at: "2026-07-27T11:00:05Z",
      last_status_code: 200,
    };

    it("lists delivery rows with their event type and state", async () => {
      searchParams = new URLSearchParams("id=wh_1");
      respond({
        deliveries: () => okJson({ items: [delivery], next_cursor: null }),
      });

      render(<WebhookDetailPage />);
      await waitFor(() => {
        expect(screen.getByText("email.received")).toBeInTheDocument();
      });
      expect(screen.getByText("delivered")).toBeInTheDocument();
    });

    // "Failed" alone sends people to their own logs. The status code and the
    // error body are what make the row actionable.
    it("shows the HTTP status code and error for a failed delivery", async () => {
      searchParams = new URLSearchParams("id=wh_1");
      respond({
        deliveries: () =>
          okJson({
            items: [
              {
                ...delivery,
                status: "failed",
                attempts: 10,
                last_status_code: 503,
                last_error: "upstream connect error",
              },
            ],
            next_cursor: null,
          }),
      });

      render(<WebhookDetailPage />);
      await waitFor(() => {
        expect(screen.getByText("failed")).toBeInTheDocument();
      });
      expect(screen.getByText(/503/)).toBeInTheDocument();
      expect(screen.getByText(/upstream connect error/)).toBeInTheDocument();
    });

    // A pending delivery with a future retry is in flight, not broken.
    it("distinguishes a retrying delivery from a failed one", async () => {
      searchParams = new URLSearchParams("id=wh_1");
      respond({
        deliveries: () =>
          okJson({
            items: [
              {
                ...delivery,
                status: "pending",
                attempts: 3,
                // Far future so the row is 'retrying' regardless of when the
                // suite runs.
                next_retry_at: "2099-01-01T00:00:00Z",
              },
            ],
            next_cursor: null,
          }),
      });

      render(<WebhookDetailPage />);
      await waitFor(() => {
        expect(screen.getByText(/retrying/i)).toBeInTheDocument();
      });
      expect(screen.queryByText(/^failed$/)).not.toBeInTheDocument();
    });

    // Worker lag or clock skew. Must not render a negative countdown.
    it("renders an overdue retry without a countdown", async () => {
      searchParams = new URLSearchParams("id=wh_1");
      respond({
        deliveries: () =>
          okJson({
            items: [
              {
                ...delivery,
                status: "pending",
                next_retry_at: "2000-01-01T00:00:00Z",
              },
            ],
            next_cursor: null,
          }),
      });

      render(<WebhookDetailPage />);
      await waitFor(() => {
        expect(screen.getByText(/retry due/i)).toBeInTheDocument();
      });
      expect(document.body.textContent).not.toMatch(/-\d+/);
    });

    // Both are open sets. An unrecognized status must not read as success,
    // and an unrecognized event type must not be dropped.
    it("surfaces an unrecognized status verbatim instead of as success", async () => {
      searchParams = new URLSearchParams("id=wh_1");
      respond({
        deliveries: () =>
          okJson({
            items: [{ ...delivery, status: "deferred", type: "some.future.event" }],
            next_cursor: null,
          }),
      });

      render(<WebhookDetailPage />);
      await waitFor(() => {
        expect(screen.getByText("some.future.event")).toBeInTheDocument();
      });
      expect(screen.getByText("deferred")).toBeInTheDocument();
      expect(screen.queryByText("delivered")).not.toBeInTheDocument();
    });

    // last_error is arbitrary bytes from a customer's endpoint.
    it("renders a hostile error body as inert text", async () => {
      searchParams = new URLSearchParams("id=wh_1");
      respond({
        deliveries: () =>
          okJson({
            items: [
              {
                ...delivery,
                status: "failed",
                last_error: "<img src=x onerror=alert(1)>",
              },
            ],
            next_cursor: null,
          }),
      });

      render(<WebhookDetailPage />);
      await waitFor(() => {
        expect(screen.getByText("failed")).toBeInTheDocument();
      });
      expect(document.querySelector("img")).toBeNull();
      expect(
        screen.getByText(/<img src=x onerror=alert\(1\)>/),
      ).toBeInTheDocument();
    });

    it("truncates a long error and reveals the rest on demand", async () => {
      const long = "E".repeat(400);
      searchParams = new URLSearchParams("id=wh_1");
      respond({
        deliveries: () =>
          okJson({
            items: [{ ...delivery, status: "failed", last_error: long }],
            next_cursor: null,
          }),
      });

      render(<WebhookDetailPage />);
      await waitFor(() => {
        expect(screen.getByText("failed")).toBeInTheDocument();
      });
      expect(screen.queryByText(long)).not.toBeInTheDocument();

      screen.getByRole("button", { name: /show full error/i }).click();
      await waitFor(() => {
        expect(screen.getByText(long)).toBeInTheDocument();
      });
    });

    // "This endpoint has never received anything" and "nothing matched the
    // filter you picked" are different facts and must not share a string.
    it("distinguishes never-delivered from nothing-matching-the-filter", async () => {
      searchParams = new URLSearchParams("id=wh_1");
      respond({ deliveries: () => okJson({ items: [], next_cursor: null }) });

      render(<WebhookDetailPage />);
      await waitFor(() => {
        expect(screen.getByText(/no deliveries yet/i)).toBeInTheDocument();
      });
    });

    it("requests the server-side status filter when one is selected", async () => {
      searchParams = new URLSearchParams("id=wh_1");
      respond({ deliveries: () => okJson({ items: [], next_cursor: null }) });

      render(<WebhookDetailPage />);
      await waitFor(() => {
        expect(screen.getByText(/no deliveries yet/i)).toBeInTheDocument();
      });

      screen.getByRole("button", { name: "Failed" }).click();
      await waitFor(() => {
        const urls = mockFetch.mock.calls.map((c) => c[0] as string);
        expect(urls.some((u) => u.includes("status=failed"))).toBe(true);
      });
    });
  });

  // A 500 is not a 404. Telling someone their id is wrong when the server is
  // broken sends them to debug the address bar instead of the outage.
  it("distinguishes a server error from a missing webhook", async () => {
    searchParams = new URLSearchParams("id=wh_1");
    respond({
      webhook: () =>
        Promise.resolve({
          ok: false,
          status: 500,
          text: () => Promise.resolve("boom"),
        }),
    });

    render(<WebhookDetailPage />);
    await waitFor(() => {
      expect(screen.getByText(/couldn't load that webhook/i)).toBeInTheDocument();
    });
    expect(
      screen.queryByText(/couldn't find that webhook/i),
    ).not.toBeInTheDocument();
  });

  it("explains a missing id parameter rather than fetching", async () => {
    searchParams = new URLSearchParams();
    respond({});

    render(<WebhookDetailPage />);
    await waitFor(() => {
      expect(screen.getByText(/no webhook selected/i)).toBeInTheDocument();
    });
    expect(mockFetch).not.toHaveBeenCalled();
  });
});
