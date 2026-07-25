import { render, screen } from "../../../../test-utils/swr";
import userEvent from "@testing-library/user-event";
import { SetupMethodChoice } from "./SetupMethodChoice";

describe("SetupMethodChoice", () => {
  it("renders both setup methods and marks the agent path as recommended", () => {
    render(<SetupMethodChoice selected={null} onSelect={jest.fn()} />);
    expect(screen.getByText("With an agent")).toBeInTheDocument();
    expect(screen.getByText("Set up in the web UI")).toBeInTheDocument();
    expect(screen.getByText("Recommended")).toBeInTheDocument();
  });

  it("reports clicks with the chosen method", async () => {
    const onSelect = jest.fn();
    render(<SetupMethodChoice selected={null} onSelect={onSelect} />);

    await userEvent.click(screen.getByText("With an agent"));
    expect(onSelect).toHaveBeenCalledWith("agent");

    await userEvent.click(screen.getByText("Set up in the web UI"));
    expect(onSelect).toHaveBeenCalledWith("web");
  });

  it("marks only the selected card as pressed", () => {
    render(<SetupMethodChoice selected="web" onSelect={jest.fn()} />);
    expect(screen.getByText("With an agent").closest("button")).toHaveAttribute("aria-pressed", "false");
    expect(screen.getByText("Set up in the web UI").closest("button")).toHaveAttribute("aria-pressed", "true");
  });
});
