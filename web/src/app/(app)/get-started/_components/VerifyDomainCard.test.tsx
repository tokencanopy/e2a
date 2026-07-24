import { render, screen, waitFor } from "../../../../test-utils/swr";
import userEvent from "@testing-library/user-event";
import { VerifyDomainCard } from "./VerifyDomainCard";
import { verifyDomain } from "../../../components/onboarding/api";
import type { DomainInfo } from "../../../components/onboarding/types";

jest.mock("../../../components/onboarding/api", () => ({
  ...jest.requireActual("../../../components/onboarding/api"),
  verifyDomain: jest.fn(),
}));
const mockVerify = verifyDomain as jest.MockedFunction<typeof verifyDomain>;

const domain: DomainInfo = {
  domain: "mail.example.com",
  verified: false,
  verification_token: "e2a-verify=abc123",
  dns_records: [],
  created_at: "2026-01-01T00:00:00Z",
  verified_at: null,
};

beforeEach(() => mockVerify.mockReset());

describe("VerifyDomainCard", () => {
  it("names the domain being verified", () => {
    render(<VerifyDomainCard domain={domain} onVerified={jest.fn()} />);
    expect(screen.getByText("Verify domain ownership")).toBeInTheDocument();
    expect(screen.getByText("mail.example.com")).toBeInTheDocument();
  });

  it("calls the verify endpoint and notifies the parent on success", async () => {
    mockVerify.mockResolvedValue({ domain: domain.domain, verified: true });
    const onVerified = jest.fn();
    render(<VerifyDomainCard domain={domain} onVerified={onVerified} />);

    await userEvent.click(screen.getByRole("button", { name: "Verify domain" }));

    await waitFor(() => expect(onVerified).toHaveBeenCalledTimes(1));
    expect(mockVerify).toHaveBeenCalledWith("mail.example.com");
  });

  it("shows the error and a propagation hint when verification fails", async () => {
    mockVerify.mockRejectedValue(new Error("TXT record not found"));
    render(<VerifyDomainCard domain={domain} onVerified={jest.fn()} />);

    await userEvent.click(screen.getByRole("button", { name: "Verify domain" }));

    expect(await screen.findByText("TXT record not found")).toBeInTheDocument();
    expect(
      screen.getByText(/DNS changes can take a few minutes to propagate/),
    ).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Verify domain" })).toBeEnabled();
  });

  it("disables the button while verification is in flight", async () => {
    let resolveVerify: (v: unknown) => void = () => {};
    mockVerify.mockImplementation(
      () => new Promise((r) => { resolveVerify = r; }) as never,
    );
    render(<VerifyDomainCard domain={domain} onVerified={jest.fn()} />);

    await userEvent.click(screen.getByRole("button", { name: "Verify domain" }));
    expect(await screen.findByRole("button", { name: "Verifying..." })).toBeDisabled();

    resolveVerify({ domain: domain.domain, verified: true });
    await waitFor(() =>
      expect(screen.getByRole("button", { name: "Verify domain" })).toBeEnabled(),
    );
  });
});
