"use client";

// Shared SWR hook for the account's external sending access status
// (GET /v1/account → sending_access, beta). Reads through `limitsKey` — the
// same cache entry the Billing page's usage/plan read uses — so mounting
// this hook alongside Billing dedupes onto one request rather than firing a
// second GET /v1/account.
//
// Consumers: the dashboard restriction notice, the onboarding success
// panel, and the review-queue composer's recipient preflight.

import useSWR from "swr";
import { getAccountInfo } from "../onboarding/api";
import { limitsKey } from "../../../lib/swrKeys";
import type { SendingAccessStatus } from "../../../lib/sendingAccess";

export function useSendingAccess(): {
  status: SendingAccessStatus | undefined;
  ownerEmail: string | undefined;
  isLoading: boolean;
} {
  const { data, isLoading } = useSWR(limitsKey, getAccountInfo);
  return {
    status: data?.sending_access,
    ownerEmail: data?.user?.email,
    isLoading,
  };
}
