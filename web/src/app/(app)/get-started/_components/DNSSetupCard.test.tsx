import { render, screen } from "../../../../test-utils/swr";
import { DNSSetupCard } from "./DNSSetupCard";
import { track } from "../../../components/onboarding/analytics";
import type { DomainInfo, DNSRecordPurpose } from "../../../components/onboarding/types";

jest.mock("../../../components/onboarding/analytics", () => ({
  track: jest.fn(),
}));
const mockTrack = track as jest.MockedFunction<typeof track>;

const domain: DomainInfo = {
  domain: "mail.example.com",
  verified: false,
  verification_token: "e2a-verify=abc123",
  dns_records: [
    {
      type: "TXT",
      name: "mail.example.com",
      value: "e2a-verify=abc123",
      priority: null,
      purpose: "ownership",
      status: "pending",
    },
    {
      type: "MX",
      name: "mail.example.com",
      value: "mx.e2a.dev",
      priority: 10,
      purpose: "inbound_mx",
      status: "pending",
    },
    {
      type: "MX",
      name: "mail.example.com",
      value: "mx-backup.e2a.dev",
      priority: null,
      purpose: "inbound_mx_wildcard",
      status: "pending",
    },
  ],
  created_at: "2026-01-01T00:00:00Z",
  verified_at: null,
};

beforeEach(() => mockTrack.mockClear());

describe("DNSSetupCard", () => {
  it("renders every record with its human purpose label", () => {
    render(<DNSSetupCard domain={domain} />);
    expect(screen.getByText("Prove domain ownership")).toBeInTheDocument();
    expect(screen.getByText("Route email to e2a")).toBeInTheDocument();
    expect(screen.getByText("Route email for all subdomains")).toBeInTheDocument();
    expect(screen.getByText("e2a-verify=abc123")).toBeInTheDocument();
  });

  it("splits MX records into name / mail server / priority fields", () => {
    render(<DNSSetupCard domain={domain} />);
    expect(screen.getAllByText("Mail server").length).toBe(2);
    expect(screen.getByText("mx.e2a.dev")).toBeInTheDocument();
    // A null priority renders as the 10 default.
    expect(screen.getAllByText("10").length).toBeGreaterThan(0);
  });

  it("falls back to the raw purpose for an unknown record kind", () => {
    render(
      <DNSSetupCard
        domain={{
          ...domain,
          dns_records: [
            {
              type: "TXT",
              name: "mail.example.com",
              value: "v=something",
              priority: null,
              purpose: "future_record" as DNSRecordPurpose,
              status: "pending",
            },
          ],
        }}
      />,
    );
    expect(screen.getByText("future_record")).toBeInTheDocument();
  });

  it("tracks the instructions view once on mount", () => {
    render(<DNSSetupCard domain={domain} />);
    expect(mockTrack).toHaveBeenCalledTimes(1);
    expect(mockTrack).toHaveBeenCalledWith("dns_instructions_viewed", {
      domain: "mail.example.com",
    });
  });

  it("shows the propagation warning", () => {
    render(<DNSSetupCard domain={domain} />);
    expect(
      screen.getByText(/DNS changes can take a few minutes to propagate/),
    ).toBeInTheDocument();
  });
});
