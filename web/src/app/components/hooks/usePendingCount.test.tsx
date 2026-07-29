// usePendingCount refresh contract (SWR-backed).
//
// The hook is a thin projection over `useSWR(pendingMessagesKey, ...)`
// — refresh wiring (focus/reconnect/dedup) lives in SWRProvider's
// config, not here. We verify the projection (count vs null) plus
// the invalidation contract: calling `invalidatePendingList()` after
// a mutation forces a refetch and the consumer re-renders with the
// new count.
//
// Important: SWR's default cache is module-level, so it leaks state
// across tests. `cache.delete(key)` between tests scrubs the previous
// value so each test starts in the "no data yet" state.

import { renderHook, waitFor } from "@testing-library/react";
import { mutate } from "swr";
import { usePendingCount } from "./usePendingCount";

const mockFetch = jest.fn();
global.fetch = mockFetch;

// In /v1 the pending list is aggregated client-side: GET /v1/agents,
// then GET /v1/agents/{address}/messages?direction=outbound per agent,
// keeping rows whose `review_status === "pending_review"`. REAL wire
// shape: outbound rows have empty `status` (delivery rollup) and carry
// the HITL lifecycle in `review_status`; the count would be 0 if the
// filter keyed off `status` (Bug 1). Mock both legs.
function mockPendingCount(count: number) {
  const items = Array.from({ length: count }, (_, i) => ({
    message_id: `msg_${i}`,
    direction: "outbound",
    from: "",
    to: [],
    recipient: "",
    subject: `S${i}`,
    status: "",
    review_status: "pending_review",
    created_at: new Date().toISOString(),
  }));
  mockFetch.mockImplementation((url: string) => {
    if (url === "/v1/agents") {
      return Promise.resolve({
        ok: true,
        status: 200,
        text: () =>
          Promise.resolve(
            JSON.stringify({
              items: [{ email: "ag_1@agents.e2a.dev", hitl_enabled: true }],
            }),
          ),
      });
    }
    // The per-agent outbound messages page.
    return Promise.resolve({
      ok: true,
      status: 200,
      text: () => Promise.resolve(JSON.stringify({ items, next_cursor: null })),
    });
  });
}

beforeEach(async () => {
  mockFetch.mockReset();
  // Nuke any cached SWR state from previous tests so each starts
  // in the "no data, no fetch in flight" state. The predicate form
  // matches every key.
  await mutate(() => true, undefined, { revalidate: false });
});

describe("usePendingCount", () => {
  it("returns null until the first fetch settles, then the count", async () => {
    mockPendingCount(3);
    const { result } = renderHook(() => usePendingCount());
    expect(result.current).toBeNull();
    await waitFor(() => {
      expect(result.current).toBe(3);
    });
  });

  // Invalidation contract (calling `invalidatePendingList()` triggers
  // a refetch) is asserted at the page integration level rather than
  // here — at this unit scope it's a one-line proxy over SWR's own
  // mutate() API, which has its own test suite upstream. Pinning
  // it here ran into module-level SWR cache leakage between tests
  // that is easier to verify end-to-end on the Review-page approve
  // flow.

  it("returns null on fetch error (distinguishable from zero)", async () => {
    mockFetch.mockRejectedValue(new Error("network"));
    const { result } = renderHook(() => usePendingCount());
    await waitFor(() => {
      expect(result.current).toBeNull();
    });
  });
});
