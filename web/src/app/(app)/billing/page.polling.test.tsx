// The Billing page has to refresh itself while it sits open: usage
// counters move whenever the account sends or receives mail, and the
// current plan changes out-of-band when Stripe's webhook provisions an
// upgrade. Both of its reads were focus-revalidate-only, so a user
// watching the page saw numbers frozen at whatever they were when the
// tab mounted.
//
// Mirrors the *.polling.test.tsx convention used for the pending and
// unread hooks: assert the page hands the shared preset to useSWR, and
// let livePolling.test.ts own the preset's actual cadence.

import { render } from "@testing-library/react";
import useSWR from "swr";
import { billingPolling } from "../../../lib/livePolling";

jest.mock("swr", () => ({
  __esModule: true,
  default: jest.fn(() => ({
    data: undefined,
    error: undefined,
    isLoading: true,
    mutate: jest.fn(),
  })),
}));

jest.mock("next/link", () => {
  return function MockLink({
    href,
    children,
    ...rest
  }: {
    href: string;
    children: React.ReactNode;
    [k: string]: unknown;
  }) {
    return (
      <a href={href} {...rest}>
        {children}
      </a>
    );
  };
});

// BILLING_API is read at module-evaluation time, so it must be set before
// the page module is required — otherwise the plan fetch is skipped
// entirely (null key) and there's no second call to assert on.
process.env.NEXT_PUBLIC_BILLING_API = "https://billing.test";
// eslint-disable-next-line @typescript-eslint/no-require-imports
const BillingPage = require("./page").default as React.ComponentType;

const mockUseSWR = useSWR as jest.MockedFunction<typeof useSWR>;

beforeEach(() => {
  mockUseSWR.mockClear();
});

describe("BillingPage polling", () => {
  it("polls account limits on the shared billing cadence", () => {
    render(<BillingPage />);

    const call = mockUseSWR.mock.calls.find(([key]) => key === "limits");
    expect(call).toBeDefined();
    expect(call![2]).toEqual(expect.objectContaining(billingPolling));
  });

  it("polls the sidecar plan catalog on the shared billing cadence", () => {
    render(<BillingPage />);

    const call = mockUseSWR.mock.calls.find(
      ([key]) => key === "https://billing.test/api/billing/plan",
    );
    expect(call).toBeDefined();
    expect(call![2]).toEqual(expect.objectContaining(billingPolling));
  });
});
