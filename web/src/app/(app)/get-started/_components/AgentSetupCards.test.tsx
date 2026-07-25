import { render, screen } from "../../../../test-utils/swr";
import userEvent from "@testing-library/user-event";
import { AgentSetupCards } from "./AgentSetupCards";

const writeText = jest.fn(async () => {});
Object.assign(navigator, { clipboard: { writeText } });

beforeEach(() => writeText.mockClear());

describe("AgentSetupCards", () => {
  it("shows the Claude Code setup by default", () => {
    render(<AgentSetupCards onBack={jest.fn()} />);
    expect(screen.getByText("Claude Code setup")).toBeInTheDocument();
    expect(screen.getByText(/claude plugin marketplace add tokencanopy\/e2a/)).toBeInTheDocument();
  });

  it("switches between client tabs", async () => {
    render(<AgentSetupCards onBack={jest.fn()} />);

    await userEvent.click(screen.getByRole("button", { name: "Codex" }));
    expect(screen.getByText("Codex setup")).toBeInTheDocument();
    expect(screen.queryByText("Claude Code setup")).not.toBeInTheDocument();
    expect(screen.getByText(/codex plugin marketplace add tokencanopy\/e2a/)).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "Other agents" }));
    expect(screen.getByText("Connect any MCP client")).toBeInTheDocument();
    expect(screen.getByText("https://api.e2a.dev/mcp")).toBeInTheDocument();
    expect(screen.queryByText("Codex setup")).not.toBeInTheDocument();
  });

  it("marks the active client tab as pressed", async () => {
    render(<AgentSetupCards onBack={jest.fn()} />);
    expect(screen.getByRole("button", { name: "Claude Code" })).toHaveAttribute("aria-pressed", "true");
    await userEvent.click(screen.getByRole("button", { name: "Codex" }));
    expect(screen.getByRole("button", { name: "Codex" })).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByRole("button", { name: "Claude Code" })).toHaveAttribute("aria-pressed", "false");
  });

  it("copies the setup commands to the clipboard", async () => {
    render(<AgentSetupCards onBack={jest.fn()} />);
    await userEvent.click(screen.getByRole("button", { name: "Copy plugin commands" }));
    expect(writeText).toHaveBeenCalledWith(
      "claude plugin marketplace add tokencanopy/e2a\nclaude plugin install e2a@e2a",
    );
    expect(screen.getByRole("button", { name: "Copied!" })).toBeInTheDocument();
  });

  it("calls onBack from the back button", async () => {
    const onBack = jest.fn();
    render(<AgentSetupCards onBack={onBack} />);
    await userEvent.click(screen.getByRole("button", { name: /Back/ }));
    expect(onBack).toHaveBeenCalledTimes(1);
  });
});
