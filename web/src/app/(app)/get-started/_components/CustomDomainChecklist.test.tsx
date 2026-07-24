import { render, screen, waitFor } from "../../../../test-utils/swr";
import userEvent from "@testing-library/user-event";
import { CustomDomainChecklist } from "./CustomDomainChecklist";
import { listDomains, verifyDomain, createAgent } from "../../../components/onboarding/api";
import type { DomainInfo } from "../../../components/onboarding/types";

jest.mock("../../../components/onboarding/api", () => ({
  ...jest.requireActual("../../../components/onboarding/api"),
  listDomains: jest.fn(),
  verifyDomain: jest.fn(),
  createAgent: jest.fn(),
}));
jest.mock("../../../../lib/swrKeys", () => ({
  invalidateAgents: jest.fn().mockResolvedValue(undefined),
}));
const mockListDomains = listDomains as jest.MockedFunction<typeof listDomains>;
const mockVerify = verifyDomain as jest.MockedFunction<typeof verifyDomain>;

function makeDomain(overrides: Partial<DomainInfo> = {}): DomainInfo {
  return {
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
    ],
    created_at: "2026-01-01T00:00:00Z",
    verified_at: null,
    ...overrides,
  };
}

beforeEach(() => {
  mockListDomains.mockReset();
  mockVerify.mockReset();
});

describe("CustomDomainChecklist — phase derivation", () => {
  it("resumes at DNS + verify for an unverified initial domain, without fetching domains", () => {
    render(
      <CustomDomainChecklist initialDomain={makeDomain()} onComplete={jest.fn()} />,
    );
    expect(screen.getByText("Configure DNS records")).toBeInTheDocument();
    expect(screen.getByText("Verify domain ownership")).toBeInTheDocument();
    // The checklist shows step 1 complete and step 2 active.
    expect(screen.getByText("Domain selected")).toBeInTheDocument();
    expect(mockListDomains).not.toHaveBeenCalled();
  });

  it("resumes at inbox creation for a verified initial domain", () => {
    render(
      <CustomDomainChecklist
        initialDomain={makeDomain({ verified: true, verified_at: "2026-01-15T00:00:00Z" })}
        onComplete={jest.fn()}
      />,
    );
    expect(screen.getByText(/is verified\. Create an inbox on it\./)).toBeInTheDocument();
    expect(screen.getByPlaceholderText("support")).toBeInTheDocument();
  });

  it("loads existing domains when no initial domain is given", async () => {
    mockListDomains.mockResolvedValue([makeDomain()]);
    render(<CustomDomainChecklist onComplete={jest.fn()} />);

    expect(await screen.findByText("mail.example.com")).toBeInTheDocument();
    expect(mockListDomains).toHaveBeenCalledTimes(1);
  });

  it("still shows the selector (new-domain form) when listing domains fails", async () => {
    mockListDomains.mockRejectedValue(new Error("boom"));
    render(<CustomDomainChecklist onComplete={jest.fn()} />);

    expect(await screen.findByPlaceholderText("mail.yourcompany.com")).toBeInTheDocument();
  });
});

describe("CustomDomainChecklist — progression", () => {
  it("moves from domain selection to DNS + verify when a domain is chosen", async () => {
    mockListDomains.mockResolvedValue([makeDomain()]);
    render(<CustomDomainChecklist onComplete={jest.fn()} />);

    await userEvent.click(await screen.findByText("mail.example.com"));

    expect(screen.getByText("Configure DNS records")).toBeInTheDocument();
    expect(screen.getByText("Verify domain ownership")).toBeInTheDocument();
  });

  it("advances to inbox creation once verification succeeds", async () => {
    mockVerify.mockResolvedValue({ domain: "mail.example.com", verified: true });
    render(
      <CustomDomainChecklist initialDomain={makeDomain()} onComplete={jest.fn()} />,
    );

    await userEvent.click(screen.getByRole("button", { name: "Verify domain" }));

    expect(await screen.findByPlaceholderText("support")).toBeInTheDocument();
  });

  it("hands the created inbox to onComplete", async () => {
    const created = { id: "ag_1", domain: "mail.example.com", email: "support@mail.example.com" };
    (createAgent as jest.Mock).mockResolvedValue(created);
    const onComplete = jest.fn();
    render(
      <CustomDomainChecklist
        initialDomain={makeDomain({ verified: true, verified_at: "2026-01-15T00:00:00Z" })}
        onComplete={onComplete}
      />,
    );

    await userEvent.type(screen.getByPlaceholderText("support"), "support");
    await userEvent.click(screen.getByRole("button", { name: "Create inbox" }));

    await waitFor(() => expect(onComplete).toHaveBeenCalledWith(created));
  });

  it("renders a back button only when onBack is provided", async () => {
    const { unmount } = render(
      <CustomDomainChecklist initialDomain={makeDomain()} onComplete={jest.fn()} />,
    );
    expect(screen.queryByRole("button", { name: /Back/ })).not.toBeInTheDocument();
    unmount();

    const onBack = jest.fn();
    render(
      <CustomDomainChecklist initialDomain={makeDomain()} onComplete={jest.fn()} onBack={onBack} />,
    );
    await userEvent.click(screen.getByRole("button", { name: /Back/ }));
    expect(onBack).toHaveBeenCalledTimes(1);
  });
});
