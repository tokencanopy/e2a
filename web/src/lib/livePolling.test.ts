import {
  billingPolling,
  inboxPolling,
  pendingPolling,
  unreadPolling,
} from './livePolling';

describe('live polling configuration', () => {
  it('polls inbox data every 10 seconds only while visible and online', () => {
    expect(inboxPolling).toEqual({
      refreshInterval: 10000,
      refreshWhenHidden: false,
      refreshWhenOffline: false,
    });
  });

  it('uses the inbox polling contract for pending data', () => {
    expect(pendingPolling).toEqual({
      refreshInterval: 10000,
      refreshWhenHidden: false,
      refreshWhenOffline: false,
    });
  });

  it('polls unread data every 15 seconds only while visible and online', () => {
    expect(unreadPolling).toEqual({
      refreshInterval: 15000,
      refreshWhenHidden: false,
      refreshWhenOffline: false,
    });
  });

  // Billing usage counters move whenever the account sends or receives,
  // so the page must refresh itself while it sits open. 30s is slower
  // than the inbox cadence on purpose — nobody watches a storage bar the
  // way they watch a message list, and this fetch also hits the external
  // sidecar.
  it('polls billing data every 30 seconds only while visible and online', () => {
    expect(billingPolling).toEqual({
      refreshInterval: 30000,
      refreshWhenHidden: false,
      refreshWhenOffline: false,
    });
  });
});
