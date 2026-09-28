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
      "External sending is restricted for this account.",
    );
    // The headline never assumes an inbox exists (an account may have none).
    expect(screen.getByTestId("sending-access-restricted-notice")).not.toHaveTextContent(/inbox is ready/i);
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

  it("approval-only deployment: leads with the request, no domain or plan links", () => {
    render(<SendingAccessNotice status={{ ...restricted, available_unlocks: ["operator_approval"] }} />);
    const links = screen.getAllByRole("link").map((l) => l.textContent);
    expect(links).toEqual(["Request approval"]);
    // The notice links to the form; it is not "below" here.
    expect(screen.getByText(/To email other recipients, request approval\.$/)).toBeInTheDocument();
    expect(screen.queryByText(/below/)).not.toBeInTheDocument();
  });

  it("verified_domain listed: offers Verify a domain after Request approval", () => {
    render(
      <SendingAccessNotice
        status={{ ...restricted, available_unlocks: ["operator_approval", "verified_domain"] }}
      />,
    );
    const links = screen.getAllByRole("link").map((l) => l.textContent);
    expect(links).toEqual(["Request approval", "Verify a domain"]);
    expect(screen.getByText(/verify your own domain or request approval\./)).toBeInTheDocument();
  });

  it("paid_entitlement listed but billing disabled (self-host): no plan link or clause", () => {
    render(
      <SendingAccessNotice
        status={{ ...restricted, available_unlocks: ["operator_approval", "paid_entitlement"] }}
      />,
    );
    expect(screen.queryByRole("link", { name: "Choose a paid plan" })).not.toBeInTheDocument();
    expect(screen.queryByText(/paid base plan/)).not.toBeInTheDocument();
  });

  it("a paid entitlement does not lift the banner where paid_entitlement is not an unlock", () => {
    render(
      <SendingAccessNotice
        status={{ ...restricted, paid_external_sending_entitled: true, available_unlocks: ["operator_approval"] }}
      />,
    );
    expect(screen.getByTestId("sending-access-restricted-notice")).toBeInTheDocument();
    expect(screen.queryByTestId("sending-access-eligible")).not.toBeInTheDocument();
  });
});
