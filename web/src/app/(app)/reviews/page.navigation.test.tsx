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

it("keeps the row open when the superseded collapse commit lands after it", async () => {
  const user = userEvent.setup();
  // Start from the settled post-resolution URL: the collapse to /reviews is
  // what handleResolved issued, and it has not committed yet.
  mockRouteSelectedId = "msg_resolved";
  const { rerender } = render(<PendingPage />);

  const row = await screen.findByRole("button", {
    name: /Remaining pending review/,
  });
  await user.click(row);
  await waitFor(() =>
    expect(
      screen.getByText("Open without requiring a refresh."),
    ).toBeInTheDocument(),
  );

  // Next commits the queued navigations in order, so the earlier
  // replace("/reviews") lands FIRST — a superseded echo, not a user action.
  // Syncing from it would collapse the row and tear down its detail fetch.
  mockRouteSelectedId = "";
  rerender(<PendingPage />);
  expect(
    screen.getByText("Open without requiring a refresh."),
  ).toBeInTheDocument();

  // Our real target lands and the guard releases.
  mockRouteSelectedId = "msg_remaining";
  rerender(<PendingPage />);
  expect(
    screen.getByText("Open without requiring a refresh."),
  ).toBeInTheDocument();

  // Once caught up, an external navigation collapses the accordion again.
  mockRouteSelectedId = "";
  rerender(<PendingPage />);
  await waitFor(() =>
    expect(
      screen.queryByText("Open without requiring a refresh."),
    ).not.toBeInTheDocument(),
  );
});
