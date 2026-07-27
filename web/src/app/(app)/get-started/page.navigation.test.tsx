import { fireEvent, render, screen } from "@testing-library/react";
import GetStartedPage from "./page";

const mockPush = jest.fn();
const mockReplace = jest.fn();
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
    back: jest.fn(),
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
  mockStep = null;
});

it("advances immediately while its same-page URL push is pending", async () => {
  const { rerender } = render(<GetStartedPage />);

  fireEvent.click(await screen.findByText("Set up in the web UI"));

  expect(mockPush).toHaveBeenCalledWith("/get-started?step=address");
  expect(await screen.findByText("Shared e2a domain")).toBeInTheDocument();

  // The eventual URL commit remains the canonical external-navigation input.
  mockStep = "agent_mcp";
  rerender(<GetStartedPage />);
  expect(await screen.findByText("Claude Code setup")).toBeInTheDocument();
});
