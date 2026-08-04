export const inboxPolling = {
  refreshInterval: 10000,
  refreshWhenHidden: false,
  refreshWhenOffline: false,
} as const;

export const pendingPolling = inboxPolling;

export const unreadPolling = {
  refreshInterval: 15000,
  refreshWhenHidden: false,
  refreshWhenOffline: false,
} as const;

// Billing usage counters move whenever the account sends or receives, and
// the current plan changes out-of-band when Stripe's webhook provisions an
// upgrade — so the page has to refresh itself while it sits open. Slower
// than the inbox cadence deliberately: nobody watches a storage bar the
// way they watch a message list, and one of these reads leaves the OSS
// server for the external billing sidecar.
export const billingPolling = {
  refreshInterval: 30000,
  refreshWhenHidden: false,
  refreshWhenOffline: false,
} as const;
