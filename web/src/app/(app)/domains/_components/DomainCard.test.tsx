import { render, screen, waitFor } from "../../../../test-utils/swr";
import userEvent from "@testing-library/user-event";
import { DomainCard } from "./DomainCard";
import { verifyDomain, deleteDomain } from "../../../components/onboarding/api";
import type {
  DomainInfo,
  DNSRecord,
  DNSRecordStatus,
} from "../../../components/onboarding/types";

jest.mock("../../../components/onboarding/api", () => ({
  ...jest.requireActual("../../../components/onboarding/api"),
  verifyDomain: jest.fn(),
  deleteDomain: jest.fn(),
}));
const mockVerify = verifyDomain as jest.MockedFunction<typeof verifyDomain>;
const mockDelete = deleteDomain as jest.MockedFunction<typeof deleteDomain>;

function makeRecord(overrides: Partial<DNSRecord> = {}): DNSRecord {
  return {
    type: "TXT",
    name: "mail.example.com",
    value: "e2a-verify=abc123",
    priority: null,
    purpose: "ownership",
    status: "pending",
    ...overrides,
  };
}

function makeDomain(overrides: Partial<DomainInfo> = {}): DomainInfo {
  return {
    domain: "mail.example.com",
    verified: false,
    verification_token: "e2a-verify=abc123",
    dns_records: [
      makeRecord(),
      makeRecord({
        type: "MX",
        name: "mail.example.com",
        value: "mx.e2a.dev",
        priority: 10,
        purpose: "inbound_mx",
      }),
    ],
    created_at: "2026-01-01T00:00:00Z",
    verified_at: null,
    ...overrides,
  };
}

function renderCard(domain: DomainInfo, agentCount = 0) {
  return render(
    <DomainCard
      domain={domain}
      agentCount={agentCount}
      onVerified={jest.fn()}
      onDeleted={jest.fn()}
    />,
  );
}

beforeEach(() => {
  mockVerify.mockReset();
  mockDelete.mockReset();
});

describe("DomainCard — header", () => {
  it("shows the domain, an Unverified chip, and the inbox count", () => {
    renderCard(makeDomain());
    expect(screen.getByText("mail.example.com")).toBeInTheDocument();
    expect(screen.getByText("Unverified")).toBeInTheDocument();
    expect(screen.getByText("No inboxes")).toBeInTheDocument();
  });

  it("pluralizes the inbox count", () => {
    const { unmount } = renderCard(makeDomain(), 1);
    expect(screen.getByText("1 inbox")).toBeInTheDocument();
    unmount();
    renderCard(makeDomain(), 3);
    expect(screen.getByText("3 inboxes")).toBeInTheDocument();
  });

  it("shows a Verify domain button for unverified domains", () => {
    renderCard(makeDomain());
    expect(screen.getByRole("button", { name: "Verify domain" })).toBeInTheDocument();
  });

  it("shows a Create inbox link for verified domains", () => {
    renderCard(makeDomain({ verified: true, verified_at: "2026-01-15T00:00:00Z" }));
    const link = screen.getByRole("link", { name: "Create inbox" });
    expect(link).toHaveAttribute("href", "/get-started?domain=mail.example.com");
    expect(screen.getByText("Verified")).toBeInTheDocument();
    expect(screen.getByText(/verified/)).toBeInTheDocument();
  });
});

describe("DomainCard — DNS records section", () => {
  it("keeps records hidden until the toggle is clicked, and hides them again", async () => {
    renderCard(makeDomain());
    expect(screen.queryByText("Prove domain ownership (also drives SPF check)")).not.toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: /View DNS records/ }));
    expect(screen.getByText("Prove domain ownership (also drives SPF check)")).toBeInTheDocument();
    expect(screen.getByText("Route email to e2a")).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: /Hide DNS records/ }));
    expect(screen.queryByText("Prove domain ownership (also drives SPF check)")).not.toBeInTheDocument();
  });

  it("shows the Cloudflare MCP reminder when DNS records are expanded", async () => {
    renderCard(makeDomain());
    await userEvent.click(screen.getByRole("button", { name: /View DNS records/ }));
    expect(screen.getByText("Cloudflare managed?")).toBeInTheDocument();
  });

  it("splits MX priority into its own field instead of embedding it in the value", async () => {
    renderCard(makeDomain());
    await userEvent.click(screen.getByRole("button", { name: /View DNS records/ }));
    expect(screen.getByText("mx.e2a.dev")).toBeInTheDocument();
    expect(screen.getByText("Mail server")).toBeInTheDocument();
    expect(screen.getByText("Priority")).toBeInTheDocument();
    expect(screen.queryByText("10 mx.e2a.dev")).not.toBeInTheDocument();
  });

  it("falls back to the raw purpose label for an unknown record purpose", async () => {
    const weird = makeRecord({ purpose: "future_kind" as DNSRecord["purpose"] });
    renderCard(makeDomain({ dns_records: [weird] }));
    await userEvent.click(screen.getByRole("button", { name: /View DNS records/ }));
    expect(screen.getByText("future_kind")).toBeInTheDocument();
  });

  it("renders an unknown record status as a raw neutral chip", async () => {
    const weird = makeRecord({ status: "quarantined" as DNSRecordStatus });
    renderCard(makeDomain({ dns_records: [weird] }));
    await userEvent.click(screen.getByRole("button", { name: /View DNS records/ }));
    expect(screen.getByText("quarantined")).toBeInTheDocument();
  });

  it("shows the last-checked stamp in the section header when available", async () => {
    renderCard(makeDomain({ last_checked_at: "2026-02-01T12:00:00Z" }));
    await userEvent.click(screen.getByRole("button", { name: /View DNS records/ }));
    // The stamp renders twice: once in the card header metadata line and
    // once in the expanded DNS section header.
    expect(screen.getAllByText(/last checked/).length).toBeGreaterThanOrEqual(2);
  });
});

describe("DomainCard — outbound sending section", () => {
  const sendingRecords: DNSRecord[] = [
    makeRecord(),
    makeRecord({
      type: "MX",
      name: "bounce.mail.example.com",
      value: "feedback-smtp.us-east-1.amazonses.com",
      priority: 10,
      purpose: "mail_from_mx",
    }),
    makeRecord({
      type: "TXT",
      name: "bounce.mail.example.com",
      value: "v=spf1 include:amazonses.com ~all",
      purpose: "mail_from_spf",
    }),
  ];

  it("renders no sending section when no mail_from records exist", async () => {
    renderCard(makeDomain());
    await userEvent.click(screen.getByRole("button", { name: /View DNS records/ }));
    expect(screen.queryByText("Outbound sending")).not.toBeInTheDocument();
  });

  it("shows a Verifying… rollup chip while the sending identity is pending", async () => {
    renderCard(makeDomain({ dns_records: sendingRecords, sending_status: "pending" }));
    await userEvent.click(screen.getByRole("button", { name: /View DNS records/ }));
    expect(screen.getByText("Outbound sending")).toBeInTheDocument();
    expect(screen.getByText("Verifying…")).toBeInTheDocument();
    expect(screen.getByText("Return path for bounces (MAIL FROM)")).toBeInTheDocument();
    expect(screen.getByText("Authorize sending (SPF)")).toBeInTheDocument();
  });

  it("shows Sending enabled when the sending identity is verified", async () => {
    renderCard(makeDomain({ dns_records: sendingRecords, sending_status: "verified" }));
    await userEvent.click(screen.getByRole("button", { name: /View DNS records/ }));
    expect(screen.getByText("Sending enabled")).toBeInTheDocument();
  });

  it("surfaces sending_error when sending verification failed", async () => {
    renderCard(
      makeDomain({
        dns_records: sendingRecords,
        sending_status: "failed",
        sending_error: "MAIL FROM MX not found",
      }),
    );
    await userEvent.click(screen.getByRole("button", { name: /View DNS records/ }));
    expect(screen.getByText("MAIL FROM MX not found")).toBeInTheDocument();
  });
});

// The header chips are the only place both capability axes are visible without
// expanding the DNS section. The axes are independent, so the card must be able
// to show a domain that can receive but not send — and vice versa.
describe("DomainCard — capability header chips", () => {
  const sendingRecords: DNSRecord[] = [
    makeRecord(),
    makeRecord({
      type: "MX",
      name: "bounce.mail.example.com",
      value: "feedback-smtp.us-east-1.amazonses.com",
      priority: 10,
      purpose: "mail_from_mx",
    }),
  ];

  it("omits the outbound chip when the sending feature is off server-side", () => {
    // No mail_from records ⇒ sending not configured on this deployment. The card
    // must look exactly as it did before the outbound chip existed.
    renderCard(makeDomain());
    expect(screen.queryByText(/^Outbound/)).not.toBeInTheDocument();
  });

  it("shows Outbound pending while the sending identity is provisioning", () => {
    renderCard(
      makeDomain({
        dns_records: sendingRecords,
        capabilities: { inbound: "verified", outbound: "pending" },
      }),
    );
    expect(screen.getByText("Outbound pending")).toBeInTheDocument();
  });

  it("shows Outbound ready once the sending identity is verified", () => {
    renderCard(
      makeDomain({
        dns_records: sendingRecords,
        capabilities: { inbound: "verified", outbound: "verified" },
      }),
    );
    expect(screen.getByText("Outbound ready")).toBeInTheDocument();
  });

  it("shows Outbound failed when the sending identity failed", () => {
    renderCard(
      makeDomain({
        dns_records: sendingRecords,
        capabilities: { inbound: "verified", outbound: "failed" },
      }),
    );
    expect(screen.getByText("Outbound failed")).toBeInTheDocument();
  });

  // The point of reading `capabilities` at all: when the axes diverge the card
  // reports each one honestly instead of inferring both from one boolean.
  it("reports the axes independently — can send but not receive", () => {
    renderCard(
      makeDomain({
        verified: false,
        dns_records: sendingRecords,
        capabilities: { inbound: "pending", outbound: "verified" },
      }),
    );
    expect(screen.getByText("Unverified")).toBeInTheDocument();
    expect(screen.getByText("Outbound ready")).toBeInTheDocument();
    // Inbound is not ready, so creating an inbox is still gated behind verify.
    expect(
      screen.getByRole("button", { name: /Verify domain/ }),
    ).toBeInTheDocument();
  });

  it("prefers capabilities over a disagreeing legacy verified flag", () => {
    renderCard(
      makeDomain({
        verified: false,
        capabilities: { inbound: "verified", outbound: "none" },
      }),
    );
    expect(screen.getByText("Verified")).toBeInTheDocument();
    // Inbound-ready ⇒ the create-inbox affordance replaces the verify button.
    expect(screen.getByRole("link", { name: /Create inbox/ })).toBeInTheDocument();
  });

  it("falls back to the legacy fields when capabilities is absent", () => {
    // A server predating `capabilities` omits it entirely; the card must still
    // derive both axes rather than rendering an empty/none state.
    renderCard(
      makeDomain({
        verified: true,
        dns_records: sendingRecords,
        sending_status: "verified",
      }),
    );
    expect(screen.getByText("Verified")).toBeInTheDocument();
    expect(screen.getByText("Outbound ready")).toBeInTheDocument();
  });
});

describe("DomainCard — verify flow", () => {
  it("calls the verify endpoint, notifies the parent, and overlays the probe result", async () => {
    const onVerified = jest.fn();
    mockVerify.mockResolvedValue({
      domain: "mail.example.com",
      verified: true,
      mx: "found",
      dkim: "missing",
    });
    render(
      <DomainCard
        domain={makeDomain({
          dns_records: [
            makeRecord(),
            makeRecord({ type: "MX", value: "mx.e2a.dev", priority: 10, purpose: "inbound_mx" }),
            makeRecord({ purpose: "dkim", name: "e2a202606._domainkey.mail.example.com", value: "v=DKIM1; k=rsa; p=PUBKEY" }),
          ],
        })}
        agentCount={0}
        onVerified={onVerified}
        onDeleted={jest.fn()}
      />,
    );

    await userEvent.click(screen.getByRole("button", { name: "Verify domain" }));

    await waitFor(() => expect(onVerified).toHaveBeenCalledTimes(1));
    expect(mockVerify).toHaveBeenCalledWith("mail.example.com");

    // The probe overlay replaces the server-derived chips on the mapped rows.
    await userEvent.click(screen.getByRole("button", { name: /View DNS records/ }));
    expect(screen.getByText("Found")).toBeInTheDocument();
    expect(screen.getByText("Missing")).toBeInTheDocument();
  });

  it("shows the error and a propagation hint when verification fails", async () => {
    mockVerify.mockRejectedValue(new Error("DNS records not found"));
    renderCard(makeDomain());

    await userEvent.click(screen.getByRole("button", { name: "Verify domain" }));

    expect(await screen.findByText("DNS records not found")).toBeInTheDocument();
    expect(screen.getByText(/DNS changes can take a few minutes to propagate/)).toBeInTheDocument();
  });

  it("disables the verify button while the probe is in flight", async () => {
    let resolveVerify: (v: unknown) => void = () => {};
    mockVerify.mockImplementation(
      () => new Promise((r) => { resolveVerify = r; }) as never,
    );
    renderCard(makeDomain());

    await userEvent.click(screen.getByRole("button", { name: "Verify domain" }));
    const pending = await screen.findByRole("button", { name: "Verifying..." });
    expect(pending).toBeDisabled();

    resolveVerify({ domain: "mail.example.com", verified: true });
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Verify domain" })).toBeEnabled(),
    );
  });
});

describe("DomainCard — delete flow", () => {
  it("does nothing when the confirm dialog is cancelled", async () => {
    jest.spyOn(window, "confirm").mockReturnValue(false);
    renderCard(makeDomain());

    await userEvent.click(screen.getByRole("button", { name: "Delete" }));
    expect(mockDelete).not.toHaveBeenCalled();
  });

  it("deletes the domain and notifies the parent on confirm", async () => {
    jest.spyOn(window, "confirm").mockReturnValue(true);
    mockDelete.mockResolvedValue(undefined);
    const onDeleted = jest.fn();
    render(
      <DomainCard
        domain={makeDomain()}
        agentCount={0}
        onVerified={jest.fn()}
        onDeleted={onDeleted}
      />,
    );

    await userEvent.click(screen.getByRole("button", { name: "Delete" }));

    await waitFor(() => expect(onDeleted).toHaveBeenCalledTimes(1));
    expect(mockDelete).toHaveBeenCalledWith("mail.example.com");
  });

  it("shows an inline alert when deletion fails", async () => {
    jest.spyOn(window, "confirm").mockReturnValue(true);
    mockDelete.mockRejectedValue(new Error("Domain has active inboxes"));
    renderCard(makeDomain());

    await userEvent.click(screen.getByRole("button", { name: "Delete" }));

    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("Domain has active inboxes");
  });
});
