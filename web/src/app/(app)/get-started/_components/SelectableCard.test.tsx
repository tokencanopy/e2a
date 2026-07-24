import { render, screen, fireEvent } from "../../../../test-utils/swr";
import { SelectableCard } from "./SelectableCard";

describe("SelectableCard", () => {
  it("renders children and fires onClick", () => {
    const onClick = jest.fn();
    render(
      <SelectableCard active={false} onClick={onClick}>
        <span>Option body</span>
      </SelectableCard>,
    );
    const card = screen.getByRole("button", { name: "Option body" });
    fireEvent.click(card);
    expect(onClick).toHaveBeenCalledTimes(1);
  });

  it("exposes the selection state via aria-pressed", () => {
    const { rerender } = render(
      <SelectableCard active={false} onClick={jest.fn()}>opt</SelectableCard>,
    );
    expect(screen.getByRole("button")).toHaveAttribute("aria-pressed", "false");
    rerender(<SelectableCard active={true} onClick={jest.fn()}>opt</SelectableCard>);
    expect(screen.getByRole("button")).toHaveAttribute("aria-pressed", "true");
  });

  it("switches to the elevated/accent styling on hover and reverts on leave", () => {
    render(
      <SelectableCard active={false} onClick={jest.fn()}>opt</SelectableCard>,
    );
    const card = screen.getByRole("button");
    expect(card.style.background).toBe("var(--bg-panel)");
    expect(card.style.border).toContain("var(--border)");

    fireEvent.mouseEnter(card);
    expect(card.style.background).toBe("var(--bg-elev)");
    expect(card.style.border).toContain("var(--accent)");

    fireEvent.mouseLeave(card);
    expect(card.style.background).toBe("var(--bg-panel)");
    expect(card.style.border).toContain("var(--border)");
  });

  it("keeps the accent border while active even without hover", () => {
    render(
      <SelectableCard active={true} onClick={jest.fn()}>opt</SelectableCard>,
    );
    expect(screen.getByRole("button").style.border).toContain("var(--accent)");
  });
});
