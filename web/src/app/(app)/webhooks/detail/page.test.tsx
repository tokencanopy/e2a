// Imported from test-utils/swr, not raw RTL: this page is SWR-backed, and the
// wrapper gives each render a fresh cache with dedupingInterval 0. Without it
// consecutive tests reusing the same webhook id fall inside SWR's dedup window
// and silently reuse the previous test's cache entry instead of refetching.
import { fireEvent, render, screen, waitFor } from "../../../../test-utils/swr";
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

  it("renders auto-disabled as danger with recovery guidance", async () => {
    searchParams = new URLSearchParams("id=wh_1");
    respond({
      webhook: () =>
        okJson({
          ...webhook,
          enabled: false,
          auto_disabled_at: "2026-07-27T11:00:00Z",
        }),
    });

    render(<WebhookDetailPage />);
    await waitFor(() => {
      expect(screen.getByText("auto-disabled")).toBeInTheDocument();
    });

    expect(screen.getByText("auto-disabled").closest(".loft-chip")).toHaveClass(
      "loft-chip--danger",
    );
    expect(
      screen.getByText(/disabled this webhook after repeated delivery failures/i),
    ).toBeInTheDocument();
    expect(
      screen
        .getByText(/disabled this webhook after repeated delivery failures/i)
        .closest("div"),
    ).toHaveStyle({ background: "var(--danger-bg)" });
    expect(screen.getByText(/five-minute cooldown/i)).toBeInTheDocument();
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

    // A pending delivery with prior attempts is retrying, regardless of the
    // legacy next_retry_at field: River owns the real schedule and does not
    // advance that column between attempts.
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
                next_retry_at: "2000-01-01T00:00:00Z",
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

    it("renders a not-yet-attempted pending delivery as pending", async () => {
      searchParams = new URLSearchParams("id=wh_1");
      respond({
        deliveries: () =>
          okJson({
            items: [
              {
                ...delivery,
                status: "pending",
                attempts: 0,
                next_retry_at: "2000-01-01T00:00:00Z",
              },
            ],
            next_cursor: null,
          }),
      });

      render(<WebhookDetailPage />);
      await waitFor(() => {
        expect(screen.getByText(/^pending$/i)).toBeInTheDocument();
      });
      expect(screen.queryByText(/retry due/i)).not.toBeInTheDocument();
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

      fireEvent.click(screen.getByRole("button", { name: /show full error/i }));
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

      fireEvent.click(screen.getByRole("button", { name: "Failed" }));
      await waitFor(() => {
        const urls = mockFetch.mock.calls.map((c) => c[0] as string);
        expect(urls.some((u) => u.includes("status=failed"))).toBe(true);
      });
    });

    it("loads older delivery pages with the server cursor and keeps existing rows", async () => {
      searchParams = new URLSearchParams("id=wh_1");
      mockFetch.mockImplementation((url: string) => {
        if (!url.includes("/deliveries")) return okJson(webhook);
        if (url.includes("cursor=next-page")) {
          return okJson({
            items: [
              {
                ...delivery,
                id: "whd_older",
                type: "email.sent",
                created_at: "2026-07-26T11:00:00Z",
              },
            ],
            next_cursor: null,
          });
        }
        return okJson({ items: [delivery], next_cursor: "next-page" });
      });

      render(<WebhookDetailPage />);
      await waitFor(() => {
        expect(screen.getByText("email.received")).toBeInTheDocument();
      });

      fireEvent.click(screen.getByRole("button", { name: /load older/i }));

      await waitFor(() => {
        expect(screen.getByText("email.sent")).toBeInTheDocument();
      });
      expect(screen.getAllByText("email.received")).toHaveLength(2);
      const deliveryUrls = mockFetch.mock.calls
        .map(([url]) => String(url))
        .filter((url) => url.includes("/deliveries"));
      expect(deliveryUrls).toHaveLength(2);
      expect(
        deliveryUrls.filter((url) => !url.includes("cursor=")),
      ).toHaveLength(1);
      expect(
        deliveryUrls.filter((url) => url.includes("cursor=next-page")),
      ).toHaveLength(1);
      expect(
        screen.queryByRole("button", { name: /load older/i }),
      ).not.toBeInTheDocument();
    });

    it("retries a failed continuation without advancing to another cursor", async () => {
      searchParams = new URLSearchParams("id=wh_1");
      let nextPageAttempts = 0;
      mockFetch.mockImplementation((url: string) => {
        if (!url.includes("/deliveries")) return okJson(webhook);
        if (url.includes("cursor=next-page")) {
          nextPageAttempts += 1;
          if (nextPageAttempts === 1) {
            return Promise.resolve({
              ok: false,
              status: 500,
              text: () => Promise.resolve("temporary failure"),
            });
          }
          return okJson({
            items: [{ ...delivery, id: "whd_older", type: "email.sent" }],
            next_cursor: "third-page",
          });
        }
        if (url.includes("cursor=third-page")) {
          return okJson({
            items: [{ ...delivery, id: "whd_oldest", type: "email.failed" }],
            next_cursor: null,
          });
        }
        return okJson({ items: [delivery], next_cursor: "next-page" });
      });

      render(<WebhookDetailPage />);
      await waitFor(() => {
        expect(
          screen.getByRole("button", { name: "Load older" }),
        ).toBeInTheDocument();
      });

      fireEvent.click(screen.getByRole("button", { name: "Load older" }));
      await waitFor(() => {
        expect(
          screen.getByRole("button", { name: "Retry loading older" }),
        ).toBeInTheDocument();
      });

      fireEvent.click(
        screen.getByRole("button", { name: "Retry loading older" }),
      );
      await waitFor(() => {
        expect(screen.getByText("email.sent")).toBeInTheDocument();
      });

      const urls = mockFetch.mock.calls.map(([url]) => String(url));
      expect(
        urls.filter(
          (url) => url.includes("/deliveries") && !url.includes("cursor="),
        ),
      ).toHaveLength(1);
      expect(urls.filter((url) => url.includes("cursor=next-page"))).toHaveLength(
        2,
      );
      expect(urls.some((url) => url.includes("cursor=third-page"))).toBe(false);
      expect(
        screen.getByRole("button", { name: "Load older" }),
      ).toBeInTheDocument();
    });

    it("refreshes every loaded page so later pending rows can terminalize", async () => {
      searchParams = new URLSearchParams("id=wh_1");
      let olderFetches = 0;
      mockFetch.mockImplementation((url: string) => {
        if (!url.includes("/deliveries")) return okJson(webhook);
        if (url.includes("cursor=next-page")) {
          olderFetches += 1;
          return okJson({
            items: [
              {
                ...delivery,
                id: "whd_older",
                type: "email.sent",
                status: olderFetches === 1 ? "pending" : "failed",
                attempts: olderFetches === 1 ? 2 : 8,
              },
            ],
            next_cursor: null,
          });
        }
        return okJson({ items: [delivery], next_cursor: "next-page" });
      });

      render(<WebhookDetailPage />);
      await waitFor(() => {
        expect(
          screen.getByRole("button", { name: "Load older" }),
        ).toBeInTheDocument();
      });
      fireEvent.click(screen.getByRole("button", { name: "Load older" }));
      await waitFor(() => {
        expect(screen.getByText("retrying")).toBeInTheDocument();
      });

      fireEvent.click(screen.getByRole("button", { name: "Refresh deliveries" }));
      await waitFor(() => {
        expect(screen.getByText("failed")).toBeInTheDocument();
      });
      expect(olderFetches).toBe(2);
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
