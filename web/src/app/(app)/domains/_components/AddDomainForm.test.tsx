import { render, screen, waitFor } from "../../../../test-utils/swr";
import userEvent from "@testing-library/user-event";
import { AddDomainForm } from "./AddDomainForm";
import { registerDomain } from "../../../components/onboarding/api";

jest.mock("../../../components/onboarding/api", () => ({
  ...jest.requireActual("../../../components/onboarding/api"),
  registerDomain: jest.fn(),
}));
const mockRegister = registerDomain as jest.MockedFunction<typeof registerDomain>;

beforeEach(() => {
  mockRegister.mockReset();
});

describe("AddDomainForm", () => {
  it("keeps the submit button disabled while the domain input is empty", () => {
    render(<AddDomainForm onRegistered={jest.fn()} />);
    expect(screen.getByRole("button", { name: "Register domain" })).toBeDisabled();
  });

  it("lowercases the domain as the user types", async () => {
    render(<AddDomainForm onRegistered={jest.fn()} />);
    const input = screen.getByPlaceholderText("mail.yourcompany.com");
    await userEvent.type(input, "MAIL.Example.COM");
    expect(input).toHaveValue("mail.example.com");
  });

  it("rejects an invalid domain client-side without calling the API", async () => {
    render(<AddDomainForm onRegistered={jest.fn()} />);
    await userEvent.type(screen.getByPlaceholderText("mail.yourcompany.com"), "not a domain");
    await userEvent.click(screen.getByRole("button", { name: "Register domain" }));

    expect(
      screen.getByText("Enter a valid domain (e.g. mail.yourcompany.com)"),
    ).toBeInTheDocument();
    expect(mockRegister).not.toHaveBeenCalled();
  });

  it("registers the domain, clears the input, and notifies the parent on success", async () => {
    const onRegistered = jest.fn();
    mockRegister.mockResolvedValue({ domain: "mail.example.com" } as never);
    render(<AddDomainForm onRegistered={onRegistered} />);

    await userEvent.type(screen.getByPlaceholderText("mail.yourcompany.com"), "mail.example.com");
    await userEvent.click(screen.getByRole("button", { name: "Register domain" }));

    await waitFor(() => expect(onRegistered).toHaveBeenCalledTimes(1));
    expect(mockRegister).toHaveBeenCalledWith("mail.example.com");
    expect(screen.getByPlaceholderText("mail.yourcompany.com")).toHaveValue("");
  });

  it("shows the server error and does not notify the parent on failure", async () => {
    const onRegistered = jest.fn();
    mockRegister.mockRejectedValue(new Error("Domain already registered"));
    render(<AddDomainForm onRegistered={onRegistered} />);

    await userEvent.type(screen.getByPlaceholderText("mail.yourcompany.com"), "mail.example.com");
    await userEvent.click(screen.getByRole("button", { name: "Register domain" }));

    expect(await screen.findByText("Domain already registered")).toBeInTheDocument();
    expect(onRegistered).not.toHaveBeenCalled();
  });

  it("falls back to a generic error message for non-Error rejections", async () => {
    mockRegister.mockRejectedValue("boom");
    render(<AddDomainForm onRegistered={jest.fn()} />);

    await userEvent.type(screen.getByPlaceholderText("mail.yourcompany.com"), "mail.example.com");
    await userEvent.click(screen.getByRole("button", { name: "Register domain" }));

    expect(await screen.findByText("Failed to register domain")).toBeInTheDocument();
  });

  it("shows a Registering... state and disables submit while the request is in flight", async () => {
    let resolveRegister: (v: unknown) => void = () => {};
    mockRegister.mockImplementation(
      () => new Promise((r) => { resolveRegister = r; }) as never,
    );
    render(<AddDomainForm onRegistered={jest.fn()} />);

    await userEvent.type(screen.getByPlaceholderText("mail.yourcompany.com"), "mail.example.com");
    await userEvent.click(screen.getByRole("button", { name: "Register domain" }));

    const pending = await screen.findByRole("button", { name: "Registering..." });
    expect(pending).toBeDisabled();

    resolveRegister({ domain: "mail.example.com" });
    // Once the request settles the button leaves the pending label — it
    // stays disabled only because a successful registration clears the input.
    const settled = await screen.findByRole("button", { name: "Register domain" });
    expect(settled).toBeDisabled();
    expect(screen.getByPlaceholderText("mail.yourcompany.com")).toHaveValue("");
  });
});
