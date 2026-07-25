import { render, screen, waitFor } from "../../../../test-utils/swr";
import userEvent from "@testing-library/user-event";
import { CustomAgentForm } from "./CustomAgentForm";
import { createAgent } from "../../../components/onboarding/api";

jest.mock("../../../components/onboarding/api", () => ({
  ...jest.requireActual("../../../components/onboarding/api"),
  createAgent: jest.fn(),
}));
jest.mock("../../../../lib/swrKeys", () => ({
  invalidateAgents: jest.fn().mockResolvedValue(undefined),
}));
const mockCreateAgent = createAgent as jest.MockedFunction<typeof createAgent>;

beforeEach(() => mockCreateAgent.mockReset());

describe("CustomAgentForm", () => {
  it("shows the verified domain as the address suffix", () => {
    render(<CustomAgentForm domain="mail.example.com" onCreated={jest.fn()} />);
    expect(screen.getByText("@mail.example.com")).toBeInTheDocument();
  });

  it("keeps submit disabled until a local part is entered", async () => {
    render(<CustomAgentForm domain="mail.example.com" onCreated={jest.fn()} />);
    expect(screen.getByRole("button", { name: "Create inbox" })).toBeDisabled();
    await userEvent.type(screen.getByPlaceholderText("support"), "support");
    expect(screen.getByRole("button", { name: "Create inbox" })).toBeEnabled();
  });

  it("rejects an invalid local part client-side without calling the API", async () => {
    render(<CustomAgentForm domain="mail.example.com" onCreated={jest.fn()} />);
    await userEvent.type(screen.getByPlaceholderText("support"), "bad part!");
    await userEvent.click(screen.getByRole("button", { name: "Create inbox" }));

    expect(
      screen.getByText("Local part must be lowercase letters, numbers, dots, or hyphens."),
    ).toBeInTheDocument();
    expect(mockCreateAgent).not.toHaveBeenCalled();
  });

  it("creates the inbox on the custom domain and notifies the parent", async () => {
    const created = { id: "ag_1", domain: "mail.example.com", email: "support@mail.example.com" };
    mockCreateAgent.mockResolvedValue(created);
    const onCreated = jest.fn();
    render(<CustomAgentForm domain="mail.example.com" onCreated={onCreated} />);

    await userEvent.type(screen.getByPlaceholderText("support"), "support");
    await userEvent.type(screen.getByPlaceholderText("Support Agent"), "Support Agent");
    await userEvent.click(screen.getByRole("button", { name: "Create inbox" }));

    await waitFor(() => expect(onCreated).toHaveBeenCalledWith(created));
    expect(mockCreateAgent).toHaveBeenCalledWith({
      email: "support@mail.example.com",
      name: "Support Agent",
    });
  });

  it("shows the server error on failure", async () => {
    mockCreateAgent.mockRejectedValue(new Error("Address already registered"));
    render(<CustomAgentForm domain="mail.example.com" onCreated={jest.fn()} />);

    await userEvent.type(screen.getByPlaceholderText("support"), "support");
    await userEvent.click(screen.getByRole("button", { name: "Create inbox" }));

    expect(await screen.findByText("Address already registered")).toBeInTheDocument();
  });
});
