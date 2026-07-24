import { render, screen } from "../../../../test-utils/swr";
import userEvent from "@testing-library/user-event";
import { AddressChoice } from "./AddressChoice";

describe("AddressChoice", () => {
  it("renders both address types and marks shared as recommended", () => {
    render(<AddressChoice selected={null} onSelect={jest.fn()} />);
    expect(screen.getByText("Shared e2a domain")).toBeInTheDocument();
    expect(screen.getByText("Custom domain")).toBeInTheDocument();
    expect(screen.getByText("Recommended")).toBeInTheDocument();
  });

  it("shows the shared-domain address template using the configured agents domain", () => {
    render(<AddressChoice selected={null} onSelect={jest.fn()} />);
    // NEXT_PUBLIC_AGENTS_DOMAIN is set to agents.e2a.dev in jest.env.ts.
    expect(screen.getByText("your-slug@agents.e2a.dev")).toBeInTheDocument();
  });

  it("reports clicks with the chosen address type", async () => {
    const onSelect = jest.fn();
    render(<AddressChoice selected={null} onSelect={onSelect} />);

    await userEvent.click(screen.getByText("Shared e2a domain"));
    expect(onSelect).toHaveBeenCalledWith("shared");

    await userEvent.click(screen.getByText("Custom domain"));
    expect(onSelect).toHaveBeenCalledWith("custom");
  });

  it("marks only the selected card as pressed", () => {
    render(<AddressChoice selected="shared" onSelect={jest.fn()} />);
    expect(screen.getByText("Shared e2a domain").closest("button")).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByText("Custom domain").closest("button")).toHaveAttribute("aria-pressed", "false");
  });
});
