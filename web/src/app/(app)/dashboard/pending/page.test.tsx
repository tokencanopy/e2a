import { render } from "@testing-library/react";
import PendingRedirect from "./page";

const mockReplace = jest.fn();
let mockId: string | null = null;

jest.mock("next/navigation", () => ({
  useRouter: () => ({ replace: mockReplace }),
  useSearchParams: () => ({ get: (key: string) => (key === "id" ? mockId : null) }),
}));

beforeEach(() => {
  mockReplace.mockClear();
  mockId = null;
});

describe("/dashboard/pending — back-compat redirect", () => {
  it("redirects to /reviews when no id param is present", () => {
    render(<PendingRedirect />);
    expect(mockReplace).toHaveBeenCalledWith("/reviews");
  });

  it("preserves the id param when redirecting to /reviews", () => {
    mockId = "msg_abc123";
    render(<PendingRedirect />);
    expect(mockReplace).toHaveBeenCalledWith("/reviews?id=msg_abc123");
  });

  it("URL-encodes an id containing reserved characters", () => {
    mockId = "a b&c=d";
    render(<PendingRedirect />);
    expect(mockReplace).toHaveBeenCalledWith(
      `/reviews?id=${encodeURIComponent("a b&c=d")}`,
    );
  });
});
