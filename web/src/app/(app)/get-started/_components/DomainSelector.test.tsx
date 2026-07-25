import { render, screen, waitFor } from "../../../../test-utils/swr";
import userEvent from "@testing-library/user-event";
import { DomainSelector } from "./DomainSelector";
import { registerDomain } from "../../../components/onboarding/api";
import type { DomainInfo } from "../../../components/onboarding/types";

jest.mock("../../../components/onboarding/api", () => ({
  ...jest.requireActual("../../../components/onboarding/api"),
  registerDomain: jest.fn(),
}));
const mockRegister = registerDomain as jest.MockedFunction<typeof registerDomain>;

function makeDomain(domain: string, verified: boolean): DomainInfo {
  return {
    domain,
    verified,
    verification_token: "e2a-verify=abc123",
    dns_records: [],
    created_at: "2026-01-01T00:00:00Z",
    verified_at: verified ? "2026-01-15T00:00:00Z" : null,
  };
}

beforeEach(() => mockRegister.mockReset());

describe("DomainSelector — with existing domains", () => {
  const existing = [
    makeDomain("verified.example.com", true),
    makeDomain("pending.example.com", false),
  ];

  it("lists each domain with its verification badge", () => {
    render(<DomainSelector existingDomains={existing} onSelected={jest.fn()} />);
    expect(screen.getByText("verified.example.com")).toBeInTheDocument();
    expect(screen.getByText("pending.example.com")).toBeInTheDocument();
    expect(screen.getByText("Verified")).toBeInTheDocument();
    expect(screen.getByText("Unverified")).toBeInTheDocument();
  });

  it("selects an existing domain on click without registering anything", async () => {
    const onSelected = jest.fn();
    render(<DomainSelector existingDomains={existing} onSelected={onSelected} />);

    await userEvent.click(screen.getByText("verified.example.com"));
    expect(onSelected).toHaveBeenCalledWith(existing[0]);
    expect(mockRegister).not.toHaveBeenCalled();
  });

  it("keeps the new-domain form hidden until the toggle is clicked", async () => {
    render(<DomainSelector existingDomains={existing} onSelected={jest.fn()} />);
    expect(screen.queryByPlaceholderText("mail.yourcompany.com")).not.toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "+ Add a new domain" }));
    expect(screen.getByPlaceholderText("mail.yourcompany.com")).toBeInTheDocument();
  });
});

describe("DomainSelector — new domain registration", () => {
  it("opens with the form visible when there are no existing domains", () => {
    render(<DomainSelector existingDomains={[]} onSelected={jest.fn()} />);
    expect(screen.getByPlaceholderText("mail.yourcompany.com")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "+ Add a new domain" })).not.toBeInTheDocument();
  });

  it("rejects an invalid domain without calling the API", async () => {
    render(<DomainSelector existingDomains={[]} onSelected={jest.fn()} />);
    await userEvent.type(screen.getByPlaceholderText("mail.yourcompany.com"), "nope");
    await userEvent.click(screen.getByRole("button", { name: "Register domain" }));

    expect(
      screen.getByText("Enter a valid domain (e.g. mail.yourcompany.com)"),
    ).toBeInTheDocument();
    expect(mockRegister).not.toHaveBeenCalled();
  });

  it("registers the domain and hands it to the parent on success", async () => {
    const registered = makeDomain("mail.newco.com", false);
    mockRegister.mockResolvedValue(registered);
    const onSelected = jest.fn();
    render(<DomainSelector existingDomains={[]} onSelected={onSelected} />);

    await userEvent.type(screen.getByPlaceholderText("mail.yourcompany.com"), "mail.newco.com");
    await userEvent.click(screen.getByRole("button", { name: "Register domain" }));

    await waitFor(() => expect(onSelected).toHaveBeenCalledWith(registered));
    expect(mockRegister).toHaveBeenCalledWith("mail.newco.com");
  });

  it("shows the server error on failure", async () => {
    mockRegister.mockRejectedValue(new Error("Domain already claimed"));
    render(<DomainSelector existingDomains={[]} onSelected={jest.fn()} />);

    await userEvent.type(screen.getByPlaceholderText("mail.yourcompany.com"), "mail.newco.com");
    await userEvent.click(screen.getByRole("button", { name: "Register domain" }));

    expect(await screen.findByText("Domain already claimed")).toBeInTheDocument();
  });
});
