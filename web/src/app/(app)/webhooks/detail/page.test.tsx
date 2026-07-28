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
