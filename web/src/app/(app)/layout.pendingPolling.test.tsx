import { render, screen } from "@testing-library/react";
import AppLayout from "./layout";

jest.mock("next/link", () => {
  return function MockLink({ children }: { children: React.ReactNode }) {
    return <a>{children}</a>;
  };
});

jest.mock("next/navigation", () => ({
  usePathname: () => "/inboxes",
  useRouter: () => ({ replace: jest.fn(), push: jest.fn(), back: jest.fn() }),
}));

jest.mock("../components/AuthProvider", () => ({
  useAuth: () => ({ user: { email: "user@example.com" }, loading: false }),
}));

jest.mock("../components/swr/SWRProvider", () => ({
  SWRProvider: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}));

jest.mock("../components/swr/PendingPollingOwner", () => ({
  PendingPollingOwner: () => <span data-testid="pending-polling-owner" />,
}));

jest.mock("../components/loft/Sidebar", () => ({
  Sidebar: () => <nav />,
}));

jest.mock("../components/SignInLink", () => ({
  SignInLink: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}));

it("renders exactly one pending polling owner for the authenticated app", () => {
  render(
    <AppLayout>
      <div>content</div>
    </AppLayout>,
  );

  expect(screen.getAllByTestId("pending-polling-owner")).toHaveLength(1);
});
