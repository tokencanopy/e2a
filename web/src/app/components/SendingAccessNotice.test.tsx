// Billing-disabled variant (default Jest env — NEXT_PUBLIC_BILLING_API
// unset). The billing-enabled branch is covered by
// SendingAccessNotice.hosted.test.tsx, which sets the env var before
// importing the module (mirrors billing/page.test.tsx / the
// *.hosted.test.tsx convention elsewhere in this suite).

import { render, screen } from "@testing-library/react";
import { SendingAccessNotice } from "./SendingAccessNotice";
import type { SendingAccessStatus } from "../../lib/sendingAccess";

const restricted: SendingAccessStatus = {
  enforcement_applies: true,
  shared_external_approved: false,
  paid_external_sending_entitled: false,
  owner_recipient_verified: true,
};

describe("SendingAccessNotice", () => {
  it("renders nothing for a disabled deployment (status omitted)", () => {
    const { container } = render(<SendingAccessNotice status={undefined} />);
    expect(container).toBeEmptyDOMElement();
  });

  it("renders nothing in shadow mode / out of cohort (enforcement_applies false)", () => {
    const { container } = render(
      <SendingAccessNotice status={{ ...restricted, enforcement_applies: false }} />,
    );
    expect(container).toBeEmptyDOMElement();
  });

  it("shows the full banner with recovery actions when restricted", () => {
    render(<SendingAccessNotice status={restricted} />);
    expect(screen.getByTestId("sending-access-restricted-notice")).toHaveTextContent(
      "Your inbox is ready. External sending is restricted.",
    );
    expect(screen.getByRole("link", { name: "Verify a domain" })).toHaveAttribute(
      "href",
      "/domains",
    );
    expect(screen.getByRole("link", { name: "Request approval" })).toHaveAttribute(
      "href",
      "/sending-access",
    );
    expect(screen.queryByRole("link", { name: "Choose a paid plan" })).not.toBeInTheDocument();
  });

  it("offers the verified-email destination when owner proof exists", () => {
    render(<SendingAccessNotice status={restricted} />);
    expect(screen.getByText(/your verified account email or agent inboxes/)).toBeInTheDocument();
  });

  it("omits the verified-email destination and mentions testing without owner proof", () => {
    render(
      <SendingAccessNotice status={{ ...restricted, owner_recipient_verified: false }} />,
    );
    expect(screen.queryByText(/your verified account email/)).not.toBeInTheDocument();
    expect(screen.getByText(/testing/)).toBeInTheDocument();
  });

  it("shows a small neutral line instead of the banner once operator-approved", () => {
    render(<SendingAccessNotice status={{ ...restricted, shared_external_approved: true }} />);
    expect(screen.queryByTestId("sending-access-restricted-notice")).not.toBeInTheDocument();
    expect(screen.getByTestId("sending-access-eligible")).toHaveTextContent(
      "Operator-approved external sending",
    );
  });

  it("shows a small neutral line instead of the banner once paid-entitled", () => {
    render(
      <SendingAccessNotice status={{ ...restricted, paid_external_sending_entitled: true }} />,
    );
    expect(screen.queryByTestId("sending-access-restricted-notice")).not.toBeInTheDocument();
    expect(screen.getByTestId("sending-access-eligible")).toHaveTextContent(
      "Paid plan: external sending enabled",
    );
  });
});
