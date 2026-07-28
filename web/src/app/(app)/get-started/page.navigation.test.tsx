import { fireEvent, render, screen } from "@testing-library/react";
import GetStartedPage from "./page";

const mockPush = jest.fn();
const mockReplace = jest.fn();
const mockBack = jest.fn();
let mockStep: string | null = null;

// Keep navigation pending unless a test explicitly changes mockStep. This
// models the same-page transition window where useSearchParams still exposes
// the previous URL after router.push/replace has been called.
jest.mock("next/navigation", () => ({
  useSearchParams: () => ({
    get: (key: string) => (key === "step" ? mockStep : null),
  }),
  useRouter: () => ({
    push: mockPush,
    replace: mockReplace,
    back: mockBack,
  }),
}));

jest.mock("next/link", () => {
  return function MockLink({
    href,
    children,
    ...props
  }: {
    href: string;
    children: React.ReactNode;
    [key: string]: unknown;
  }) {
    return (
      <a href={href} {...props}>
        {children}
      </a>
    );
  };
});

beforeEach(() => {
  mockPush.mockReset();
  mockReplace.mockReset();
  mockBack.mockReset();
  mockStep = null;
});

it("advances immediately while its same-page URL push is pending", async () => {
  const { rerender } = render(<GetStartedPage />);

  fireEvent.click(await screen.findByText("Set up in the web UI"));

  expect(mockPush).toHaveBeenCalledWith("/get-started?step=address");
  expect(await screen.findByText("Shared e2a domain")).toBeInTheDocument();

  // The pending push commits. Next's app router queues navigations and
  // commits each in order, so our own target always lands before any later
  // URL change — model that ordering rather than skipping over it.
  mockStep = "address";
  rerender(<GetStartedPage />);
  expect(screen.getByText("Shared e2a domain")).toBeInTheDocument();

  // The URL remains the canonical external-navigation input once it has
  // caught up: a browser back/forward or deep link still wins.
  mockStep = "agent_mcp";
  rerender(<GetStartedPage />);
  expect(await screen.findByText("Claude Code setup")).toBeInTheDocument();
});

it("ignores the superseded step when two pushes are queued back to back", async () => {
  const { rerender } = render(<GetStartedPage />);

  // choose -> address, then address -> shared_form before either commits.
  fireEvent.click(await screen.findByText("Set up in the web UI"));
  fireEvent.click(await screen.findByText("Shared e2a domain"));
  expect(mockPush).toHaveBeenLastCalledWith("/get-started?step=shared_form");
  expect(await screen.findByText("How it works")).toBeInTheDocument();

  // The FIRST push commits — a superseded echo, not external navigation.
  // Syncing from it would throw the user back to the address chooser.
  mockStep = "address";
  rerender(<GetStartedPage />);
  expect(screen.queryByText("Shared e2a domain")).not.toBeInTheDocument();

  // The real target lands and the form stays put.
  mockStep = "shared_form";
  rerender(<GetStartedPage />);
  expect(await screen.findByText("How it works")).toBeInTheDocument();
});

it("stays on the chooser when Back is pressed before the forward push commits", async () => {
  // Non-trivial history so handleBackToChoose would otherwise take back().
  Object.defineProperty(window.history, "length", {
    configurable: true,
    value: 3,
  });

  render(<GetStartedPage />);
  fireEvent.click(await screen.findByText("Set up in the web UI"));

  // ?step=address has NOT committed, so the entry on top of the history
  // stack is still whatever preceded /get-started. back() there would leave
  // onboarding entirely; the chooser push is the correct move.
  fireEvent.click(await screen.findByText("← Back"));
  expect(mockBack).not.toHaveBeenCalled();
  expect(mockPush).toHaveBeenLastCalledWith("/get-started");
  expect(await screen.findByText("Set up in the web UI")).toBeInTheDocument();
});

it("walks browser history on Back once the forward push has committed", async () => {
  Object.defineProperty(window.history, "length", {
    configurable: true,
    value: 3,
  });

  const { rerender } = render(<GetStartedPage />);
  fireEvent.click(await screen.findByText("Set up in the web UI"));
  mockStep = "address";
  rerender(<GetStartedPage />);

  fireEvent.click(await screen.findByText("← Back"));
  expect(mockBack).toHaveBeenCalledTimes(1);
});
