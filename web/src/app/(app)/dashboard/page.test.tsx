import { render } from "@testing-library/react";
import DashboardRedirect from "./page";

const mockReplace = jest.fn();
jest.mock("next/navigation", () => ({
  useRouter: () => ({ replace: mockReplace }),
}));

beforeEach(() => mockReplace.mockClear());

describe("/dashboard — back-compat redirect", () => {
  it("renders nothing and replaces the route with /inboxes", () => {
    const { container } = render(<DashboardRedirect />);
    expect(container.firstChild).toBeNull();
    expect(mockReplace).toHaveBeenCalledTimes(1);
    expect(mockReplace).toHaveBeenCalledWith("/inboxes");
  });
});
