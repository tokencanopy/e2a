/**
 * Hosted-build branch: NEXT_PUBLIC_BILLING_API is inlined at build time and
 * read at module load, so this file sets the env BEFORE importing the
 * component (mirrors src/app/page.pricing-nav.hosted.test.tsx and
 * billing/page.tsx's own gate). The self-host branch (no paid-plan action
 * at all) is asserted in SendingAccessNotice.test.tsx, where the env var is
 * absent.
 */
process.env.NEXT_PUBLIC_BILLING_API = "https://billing.example.test";

import { render, screen } from "@testing-library/react";
import { SendingAccessNotice } from "./SendingAccessNotice";
import type { SendingAccessStatus } from "../../lib/sendingAccess";

const restricted: SendingAccessStatus = {
  enforcement_applies: true,
  shared_external_approved: false,
  paid_external_sending_entitled: false,
  owner_recipient_verified: true,
};

describe("SendingAccessNotice (hosted, billing gate enabled)", () => {
  it("adds the Choose a paid plan action linking to the billing page", () => {
    render(<SendingAccessNotice status={restricted} />);
    expect(screen.getByRole("link", { name: "Choose a paid plan" })).toHaveAttribute(
      "href",
      "/billing",
    );
  });

  it("appends the paid-plan clause to the body copy", () => {
    render(<SendingAccessNotice status={restricted} />);
    expect(
      screen.getByText(
        "Receive emails from anyone. Send to your verified account email or agent inboxes in this account. To email other recipients, verify your own domain, request approval, or activate a paid base plan.",
      ),
    ).toBeInTheDocument();
  });

  it("all three unlocks: Request approval, Verify a domain, Choose a paid plan", () => {
    render(<SendingAccessNotice status={restricted} />);
    expect(screen.getAllByRole("link").map((l) => l.textContent)).toEqual([
      "Request approval",
      "Verify a domain",
      "Choose a paid plan",
    ]);
  });

  it("hosted approval-only policy: no plan link even with billing enabled", () => {
    render(<SendingAccessNotice status={{ ...restricted, available_unlocks: ["operator_approval"] }} />);
    expect(screen.getAllByRole("link").map((l) => l.textContent)).toEqual(["Request approval"]);
    expect(screen.queryByText(/paid base plan/)).not.toBeInTheDocument();
  });

  it("approval + paid: plan link and clause, no domain link", () => {
    render(
      <SendingAccessNotice
        status={{ ...restricted, available_unlocks: ["operator_approval", "paid_entitlement"] }}
      />,
    );
    expect(screen.getAllByRole("link").map((l) => l.textContent)).toEqual([
      "Request approval",
      "Choose a paid plan",
    ]);
    expect(screen.getByText(/request approval or activate a paid base plan\./)).toBeInTheDocument();
  });
});
