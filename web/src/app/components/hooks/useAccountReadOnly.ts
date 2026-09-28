"use client";

// Whether the signed-in account is read-only (GET /v1/account → read_only):
// sending is paused for an abuse review and every write is refused. Reads
// through `limitsKey`, the same SWR entry as the Billing page,
// useSendingAccess and RestoredNotice, so it adds no request. Unknown (still
// loading, or a server that does not report it) reads as writable: the API's
// 403 account_read_only is the backstop, the UI state is a courtesy.

import useSWR from "swr";
import { getAccountInfo } from "../onboarding/api";
import { limitsKey } from "../../../lib/swrKeys";

export function useAccountReadOnly(): boolean {
  const { data } = useSWR(limitsKey, getAccountInfo);
  return data?.read_only === true;
}
