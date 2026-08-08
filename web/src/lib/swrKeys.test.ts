// Invalidation helpers are silent-failure-prone: a predicate that stops
// matching doesn't throw, it just leaves stale data cached with no visible
// error. `invalidateMessageDetail` is the sharpest case — it's what makes
// an approved/rejected message's detail refetch, and its predicate matches
// on a hand-written key SHAPE rather than on `messageDetailKey` itself. The
// per-message key was recently re-shaped from ["pending-message", email,
// id] to ["message-detail", id] (collapsing two colliding per-surface
// entries into one); a predicate left on the old shape would match nothing
// and approve/reject would leave the Review row showing a stale
// "Pending review" forever.
//
// These run against the MODULE-LEVEL SWR cache (no test-utils/swr
// fresh-Map wrapper) because the exported helpers call SWR's global
// `mutate`, which is bound to that cache. jest.setup.ts clears it between
// tests.

import { renderHook, waitFor, act } from "@testing-library/react";
import useSWR from "swr";
import {
  accountUnreadKey,
  agentUnreadKey,
  agentsKey,
  domainsKey,
  invalidateAgentUnread,
  invalidateAgents,
  invalidateDomains,
  invalidateMessageDetail,
  invalidateMessageLifecycle,
  limitsKey,
  messageDetailKey,
  messageLifecycleKey,
} from "./swrKeys";

describe("accountUnreadKey", () => {
  it("uses the canonical account-wide cache key", () => {
    expect(accountUnreadKey).toBe("account-unread");
  });
});

describe("invalidateAgentUnread", () => {
  it("revalidates the specified agent and account total without touching another agent", async () => {
    const targetFetcher = jest.fn().mockResolvedValue({ count: 1, more: false });
    const otherFetcher = jest.fn().mockResolvedValue({ count: 2, more: false });
    const accountFetcher = jest.fn().mockResolvedValue({ count: 3, more: false });

    renderHook(() => useSWR(agentUnreadKey("target@agents.test"), targetFetcher));
    renderHook(() => useSWR(agentUnreadKey("other@agents.test"), otherFetcher));
    renderHook(() => useSWR(accountUnreadKey, accountFetcher));
    await waitFor(() => {
      expect(targetFetcher).toHaveBeenCalledTimes(1);
      expect(otherFetcher).toHaveBeenCalledTimes(1);
      expect(accountFetcher).toHaveBeenCalledTimes(1);
    });

    await act(async () => {
      await invalidateAgentUnread("target@agents.test");
    });

    await waitFor(() => {
      expect(targetFetcher).toHaveBeenCalledTimes(2);
      expect(accountFetcher).toHaveBeenCalledTimes(2);
    });
    expect(otherFetcher).toHaveBeenCalledTimes(1);
  });
});

// Inbox count and domain count are two of the four usage dimensions the
// Billing page meters, so an agent/domain mutation makes the billing
// usage bars stale. Folding the limits invalidation into the agent and
// domain helpers (rather than asking every call site to remember a
// second call) is what keeps that from silently rotting: /billing read a
// key nothing in the app ever invalidated.
describe("limitsKey", () => {
  it("uses the cache key the Billing page reads account limits under", () => {
    expect(limitsKey).toBe("limits");
  });
});

// Both helpers are exercised against a SINGLE limits subscriber in one
// test rather than split in two: limitsKey is a fixed string, and SWR's
// request-dedup window is global and outlives the per-test cache reset in
// jest.setup.ts — so a second test mounting the same key would have its
// mount fetch silently suppressed and assert nothing.
describe("limits invalidation", () => {
  it("revalidates account limits from both agent and domain mutations", async () => {
    const agentsFetcher = jest.fn().mockResolvedValue([]);
    const domainsFetcher = jest.fn().mockResolvedValue([]);
    const limitsFetcher = jest.fn().mockResolvedValue({ plan_code: "free" });
    renderHook(() => useSWR(agentsKey, agentsFetcher));
    renderHook(() => useSWR(domainsKey, domainsFetcher));
    renderHook(() => useSWR(limitsKey, limitsFetcher));
    await waitFor(() => {
      expect(agentsFetcher).toHaveBeenCalledTimes(1);
      expect(domainsFetcher).toHaveBeenCalledTimes(1);
      expect(limitsFetcher).toHaveBeenCalledTimes(1);
    });

    // Creating or trashing an inbox changes usage.agents, so an open
    // Billing tab must not keep rendering the pre-mutation count.
    await act(async () => {
      await invalidateAgents();
    });
    await waitFor(() => {
      expect(agentsFetcher).toHaveBeenCalledTimes(2);
      expect(limitsFetcher).toHaveBeenCalledTimes(2);
    });

    // Same for usage.domains after a domain is registered or removed.
    await act(async () => {
      await invalidateDomains();
    });
    await waitFor(() => {
      expect(domainsFetcher).toHaveBeenCalledTimes(2);
      expect(limitsFetcher).toHaveBeenCalledTimes(3);
    });
  });
});

describe("messageDetailKey", () => {
  it("keys a message by id ALONE — no owning-inbox component", () => {
    // Message ids are globally unique, so the id is the identity. The
    // owning agent's email is a fetch parameter only. Putting the email in
    // the key would re-split the entry the review queue and the mail
    // surfaces are meant to share (they reach the same message through
    // different endpoints and only one of them knows an inbox address).
    expect(messageDetailKey("msg_abc")).toEqual(["message-detail", "msg_abc"]);
  });
});

describe("invalidateMessageDetail", () => {
  it("revalidates the entry written under the real messageDetailKey", async () => {
    // Wired through useSWR with the real key helper rather than poking the
    // cache directly, so the predicate is checked against exactly the key
    // shape a live subscriber holds.
    const fetcher = jest.fn().mockResolvedValue({ id: "msg_a" });
    renderHook(() => useSWR(messageDetailKey("msg_a"), fetcher));
    await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(1));

    await act(async () => {
      await invalidateMessageDetail("msg_a");
    });

    // A second fetch means the entry was dropped and refetched. With a
    // predicate that no longer matches this key shape, the count stays 1
    // and the surface keeps rendering pre-approval data.
    await waitFor(() => expect(fetcher).toHaveBeenCalledTimes(2));
  });

  it("leaves other messages' entries alone", async () => {
    // The predicate scopes to one id: approving message A must not force
    // every other open message detail in the dashboard to refetch.
    // Distinct ids per test: SWR's request-dedup window is keyed globally
    // and outlives the cache reset in jest.setup.ts, so reusing an id from
    // the test above would suppress the first fetch here.
    const fetcherA = jest.fn().mockResolvedValue({ id: "msg_scoped_a" });
    const fetcherB = jest.fn().mockResolvedValue({ id: "msg_scoped_b" });
    renderHook(() => useSWR(messageDetailKey("msg_scoped_a"), fetcherA));
    renderHook(() => useSWR(messageDetailKey("msg_scoped_b"), fetcherB));
    await waitFor(() => {
      expect(fetcherA).toHaveBeenCalledTimes(1);
      expect(fetcherB).toHaveBeenCalledTimes(1);
    });

    await act(async () => {
      await invalidateMessageDetail("msg_scoped_a");
    });

    await waitFor(() => expect(fetcherA).toHaveBeenCalledTimes(2));
    expect(fetcherB).toHaveBeenCalledTimes(1);
  });
});

describe("invalidateMessageLifecycle", () => {
  it("revalidates every paginated key sharing the message's prefix", async () => {
    // MessageLifecycleData pages with useSWRInfinite, so live keys carry
    // trailing page-index/cursor slots beyond messageLifecycleKey. The
    // predicate must match the shared prefix, not the exact tuple.
    const pageOneFetcher = jest.fn().mockResolvedValue({ items: [], next_cursor: "c2" });
    const pageTwoFetcher = jest.fn().mockResolvedValue({ items: [], next_cursor: null });
    renderHook(() =>
      useSWR([...messageLifecycleKey("agent@agents.test", "msg_lc_a"), 0, null], pageOneFetcher),
    );
    renderHook(() =>
      useSWR([...messageLifecycleKey("agent@agents.test", "msg_lc_a"), 1, "c2"], pageTwoFetcher),
    );
    await waitFor(() => {
      expect(pageOneFetcher).toHaveBeenCalledTimes(1);
      expect(pageTwoFetcher).toHaveBeenCalledTimes(1);
    });

    await act(async () => {
      await invalidateMessageLifecycle("agent@agents.test", "msg_lc_a");
    });

    await waitFor(() => {
      expect(pageOneFetcher).toHaveBeenCalledTimes(2);
      expect(pageTwoFetcher).toHaveBeenCalledTimes(2);
    });
  });

  it("leaves other messages' and other agents' panels alone", async () => {
    const targetFetcher = jest.fn().mockResolvedValue({ items: [] });
    const otherMessageFetcher = jest.fn().mockResolvedValue({ items: [] });
    const otherAgentFetcher = jest.fn().mockResolvedValue({ items: [] });
    renderHook(() =>
      useSWR([...messageLifecycleKey("agent@agents.test", "msg_lc_b"), 0, null], targetFetcher),
    );
    renderHook(() =>
      useSWR([...messageLifecycleKey("agent@agents.test", "msg_lc_c"), 0, null], otherMessageFetcher),
    );
    renderHook(() =>
      useSWR([...messageLifecycleKey("other@agents.test", "msg_lc_b"), 0, null], otherAgentFetcher),
    );
    await waitFor(() => {
      expect(targetFetcher).toHaveBeenCalledTimes(1);
      expect(otherMessageFetcher).toHaveBeenCalledTimes(1);
      expect(otherAgentFetcher).toHaveBeenCalledTimes(1);
    });

    await act(async () => {
      await invalidateMessageLifecycle("agent@agents.test", "msg_lc_b");
    });

    await waitFor(() => expect(targetFetcher).toHaveBeenCalledTimes(2));
    expect(otherMessageFetcher).toHaveBeenCalledTimes(1);
    expect(otherAgentFetcher).toHaveBeenCalledTimes(1);
  });
});
