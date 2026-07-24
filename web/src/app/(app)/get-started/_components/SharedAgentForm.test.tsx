import { render, screen, waitFor } from "../../../../test-utils/swr";
import userEvent from "@testing-library/user-event";
import { SharedAgentForm } from "./SharedAgentForm";
import { createAgent } from "../../../components/onboarding/api";
import { invalidateAgents } from "../../../../lib/swrKeys";

jest.mock("../../../components/onboarding/api", () => ({
  ...jest.requireActual("../../../components/onboarding/api"),
  createAgent: jest.fn(),
}));
jest.mock("../../../../lib/swrKeys", () => ({
  invalidateAgents: jest.fn().mockResolvedValue(undefined),
}));
const mockCreateAgent = createAgent as jest.MockedFunction<typeof createAgent>;
const mockInvalidate = invalidateAgents as jest.MockedFunction<typeof invalidateAgents>;

// NEXT_PUBLIC_AGENTS_DOMAIN is set to agents.e2a.dev in jest.env.ts.

beforeEach(() => {
  mockCreateAgent.mockReset();
  mockInvalidate.mockClear();
});

describe("SharedAgentForm", () => {
  it("keeps submit disabled until a slug is entered", async () => {
    render(<SharedAgentForm onCreated={jest.fn()} />);
    expect(screen.getByRole("button", { name: "Create inbox" })).toBeDisabled();
    await userEvent.type(screen.getByPlaceholderText("my-agent"), "abc");
    expect(screen.getByRole("button", { name: "Create inbox" })).toBeEnabled();
  });

  it("lowercases the slug as the user types", async () => {
    render(<SharedAgentForm onCreated={jest.fn()} />);
    const input = screen.getByPlaceholderText("my-agent");
    await userEvent.type(input, "My-Agent");
    expect(input).toHaveValue("my-agent");
  });

  it("rejects an invalid slug client-side without calling the API", async () => {
    render(<SharedAgentForm onCreated={jest.fn()} />);
    await userEvent.type(screen.getByPlaceholderText("my-agent"), "a");
    await userEvent.click(screen.getByRole("button", { name: "Create inbox" }));

    expect(screen.getByText(/Slug must be 2–40 lowercase characters/)).toBeInTheDocument();
    expect(mockCreateAgent).not.toHaveBeenCalled();
  });

  it("rejects a reserved slug", async () => {
    render(<SharedAgentForm onCreated={jest.fn()} />);
    await userEvent.type(screen.getByPlaceholderText("my-agent"), "admin");
    await userEvent.click(screen.getByRole("button", { name: "Create inbox" }));

    expect(screen.getByText(/Slug must be 2–40 lowercase characters/)).toBeInTheDocument();
    expect(mockCreateAgent).not.toHaveBeenCalled();
  });

  it("creates the inbox on the shared domain, invalidates the agents cache, and notifies the parent", async () => {
    const created = { id: "ag_1", domain: "agents.e2a.dev", email: "my-agent@agents.e2a.dev" };
    mockCreateAgent.mockResolvedValue(created);
    const onCreated = jest.fn();
    render(<SharedAgentForm onCreated={onCreated} />);

    await userEvent.type(screen.getByPlaceholderText("my-agent"), "my-agent");
    await userEvent.type(screen.getByPlaceholderText("My Agent"), "My Agent");
    await userEvent.click(screen.getByRole("button", { name: "Create inbox" }));

    await waitFor(() => expect(onCreated).toHaveBeenCalledWith(created));
    expect(mockCreateAgent).toHaveBeenCalledWith({
      email: "my-agent@agents.e2a.dev",
      name: "My Agent",
    });
    expect(mockInvalidate).toHaveBeenCalledTimes(1);
  });

  it("omits the display name from the request when left blank", async () => {
    mockCreateAgent.mockResolvedValue({ id: "ag_1", domain: "agents.e2a.dev", email: "my-agent@agents.e2a.dev" });
    render(<SharedAgentForm onCreated={jest.fn()} />);

    await userEvent.type(screen.getByPlaceholderText("my-agent"), "my-agent");
    await userEvent.click(screen.getByRole("button", { name: "Create inbox" }));

    await waitFor(() => expect(mockCreateAgent).toHaveBeenCalled());
    expect(mockCreateAgent).toHaveBeenCalledWith({ email: "my-agent@agents.e2a.dev" });
  });

  it("shows the server error on failure and does not notify the parent", async () => {
    mockCreateAgent.mockRejectedValue(new Error("Slug is taken"));
    const onCreated = jest.fn();
    render(<SharedAgentForm onCreated={onCreated} />);

    await userEvent.type(screen.getByPlaceholderText("my-agent"), "my-agent");
    await userEvent.click(screen.getByRole("button", { name: "Create inbox" }));

    expect(await screen.findByText("Slug is taken")).toBeInTheDocument();
    expect(onCreated).not.toHaveBeenCalled();
  });

  it("renders a back button only when onBack is provided", async () => {
    const { unmount } = render(<SharedAgentForm onCreated={jest.fn()} />);
    expect(screen.queryByRole("button", { name: /Back/ })).not.toBeInTheDocument();
    unmount();

    const onBack = jest.fn();
    render(<SharedAgentForm onCreated={jest.fn()} onBack={onBack} />);
    await userEvent.click(screen.getByRole("button", { name: /Back/ }));
    expect(onBack).toHaveBeenCalledTimes(1);
  });
});
