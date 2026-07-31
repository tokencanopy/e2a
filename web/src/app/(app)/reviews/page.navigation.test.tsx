import { render, screen, waitFor } from "../../../test-utils/swr";
import userEvent from "@testing-library/user-event";
import PendingPage from "./page";

const replace = jest.fn();
let mockRouteSelectedId = "msg_resolved";

// Model the real post-resolution window: the resolved row has disappeared
// from the SWR queue, but Next has not committed the replace("/reviews")
// transition yet, so useSearchParams still exposes the old selected id.
jest.mock("next/navigation", () => ({
  useSearchParams: () => ({ get: () => mockRouteSelectedId }),
  useRouter: () => ({ replace }),
}));

const remaining = {
  id: "msg_remaining",
  agent_email: "support@acme.dev",
  direction: "outbound",
  header_from: "support@acme.dev",
  envelope_from: null,
  verified_domain: null,
  to: ["customer@example.com"],
  subject: "Remaining pending review",
  review_status: "pending_review",
  created_at: "2026-07-27T00:00:00Z",
};

const detail = {
  message_id: remaining.id,
  from: remaining.agent_email,
  to: remaining.to,
  cc: [],
  recipient: remaining.to[0],
  subject: remaining.subject,
  conversation_id: "conv_remaining",
  review_status: "pending_review",
  created_at: remaining.created_at,
  body: { text: "Open without requiring a refresh.", html: "" },
};

const mockFetch = jest.fn();

beforeEach(() => {
  replace.mockReset();
  mockRouteSelectedId = "msg_resolved";
  mockFetch.mockReset();
  global.fetch = mockFetch as unknown as typeof fetch;

  mockFetch.mockImplementation((url: string) => {
    if (url === "/v1/reviews") {
      return Promise.resolve({
        ok: true,
        status: 200,
        text: () =>
          Promise.resolve(
            JSON.stringify({ items: [remaining], next_cursor: null }),
          ),
      });
    }
    if (url === `/v1/reviews/${remaining.id}/approve`) {
      return Promise.resolve({
        ok: true,
        status: 200,
        text: () =>
          Promise.resolve(
            JSON.stringify({ status: "sent", message_id: remaining.id }),
          ),
      });
    }
    if (url === `/v1/reviews/${remaining.id}`) {
      return Promise.resolve({
        ok: true,
        status: 200,
        text: () => Promise.resolve(JSON.stringify(detail)),
      });
    }
    return Promise.resolve({
      ok: false,
      status: 404,
      text: () => Promise.resolve("nf"),
    });
  });
});

it("opens the remaining review while the prior URL replacement is still stale", async () => {
  const user = userEvent.setup();
  render(<PendingPage />);

  const row = await screen.findByRole("button", {
    name: /Remaining pending review/,
  });
  await user.click(row);

  expect(replace).toHaveBeenCalledWith(
    "/reviews?id=msg_remaining",
    { scroll: false },
  );
  await waitFor(() =>
    expect(
      screen.getByText("Open without requiring a refresh."),
    ).toBeInTheDocument(),
  );
});

it("still follows URL selection changes for deep links and browser navigation", async () => {
  const { rerender } = render(<PendingPage />);

  await screen.findByRole("button", {
    name: /Remaining pending review/,
  });
  mockRouteSelectedId = "msg_remaining";
  rerender(<PendingPage />);

  expect(
    await screen.findByText("Open without requiring a refresh."),
  ).toBeInTheDocument();
});

it("recovers a deep-linked review from a later queue page", async () => {
  const deep = {
    ...remaining,
    id: "msg_deep",
    subject: "Review from page two",
  };
  mockRouteSelectedId = deep.id;
  mockFetch.mockImplementation((url: string) => {
    if (url === "/v1/reviews") {
      return Promise.resolve({
        ok: true,
        status: 200,
        text: () =>
          Promise.resolve(
            JSON.stringify({ items: [remaining], next_cursor: "page-2" }),
          ),
      });
    }
    if (url === "/v1/reviews?limit=100") {
      return Promise.resolve({
        ok: true,
        status: 200,
        text: () =>
          Promise.resolve(
            JSON.stringify({ items: [remaining], next_cursor: "page-2" }),
          ),
      });
    }
    if (url === "/v1/reviews?limit=100&cursor=page-2") {
      return Promise.resolve({
        ok: true,
        status: 200,
        text: () =>
          Promise.resolve(JSON.stringify({ items: [deep], next_cursor: null })),
      });
    }
    if (url === `/v1/reviews/${deep.id}`) {
      return Promise.resolve({
        ok: true,
        status: 200,
        text: () =>
          Promise.resolve(
            JSON.stringify({
              ...detail,
              id: deep.id,
              message_id: deep.id,
              subject: deep.subject,
              body: { text: "Recovered from a later page.", html: "" },
            }),
          ),
      });
    }
    return Promise.resolve({
      ok: false,
      status: 404,
      text: () => Promise.resolve("nf"),
    });
  });

  render(<PendingPage />);

  expect(await screen.findByText(deep.subject)).toBeInTheDocument();
  expect(
    await screen.findByText("Recovered from a later page."),
  ).toBeInTheDocument();
});


it("collapses optimistically when a hold is resolved, before the URL catches up", async () => {
  const user = userEvent.setup();
  // Deep-linked straight onto the remaining row, URL already settled there.
  mockRouteSelectedId = "msg_remaining";
  render(<PendingPage />);

  await screen.findByText("Open without requiring a refresh.");
  await user.click(screen.getByRole("button", { name: /^Approve & send/ }));

  // handleResolved clears the selection locally and asks the router for
  // /reviews. useSearchParams still reports the old id for the length of the
  // RSC round trip, so the panel must close off local state alone.
  await waitFor(() =>
    expect(replace).toHaveBeenCalledWith("/reviews", { scroll: false }),
  );
  await waitFor(() =>
    expect(
      screen.queryByText("Open without requiring a refresh."),
    ).not.toBeInTheDocument(),
  );
});
