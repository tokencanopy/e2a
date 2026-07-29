import { render, waitFor } from "../../../../../../test-utils/swr";
import LegacyMessageFocusRedirect from "./page";

const mockUseSearchParams = jest.fn();
const mockRouterReplace = jest.fn();

jest.mock("next/navigation", () => ({
  useSearchParams: () => mockUseSearchParams(),
  useRouter: () => ({ replace: mockRouterReplace }),
}));

function setSearchParams(params: Record<string, string>) {
  mockUseSearchParams.mockReturnValue({
    get: (key: string) => params[key] ?? null,
  });
}

beforeEach(() => {
  mockRouterReplace.mockReset();
});

describe("legacy message focus route", () => {
  it("redirects held-message links to the expanded consolidated Review row", async () => {
    setSearchParams({
      email: "support@acme.io",
      id: "msg_pending",
      direction: "outbound",
      pending: "1",
    });

    render(<LegacyMessageFocusRedirect />);

    await waitFor(() => {
      expect(mockRouterReplace).toHaveBeenCalledWith("/reviews?id=msg_pending");
    });
  });

  it("redirects ordinary message links to the owning inbox", async () => {
    setSearchParams({
      email: "support@acme.io",
      id: "msg_inbound",
      direction: "inbound",
      headers: "1",
    });

    render(<LegacyMessageFocusRedirect />);

    await waitFor(() => {
      expect(mockRouterReplace).toHaveBeenCalledWith(
        "/inboxes/messages?email=support%40acme.io",
      );
    });
  });

  it("falls back to the inbox list when the legacy link has no owner", async () => {
    setSearchParams({ id: "msg_orphaned" });

    render(<LegacyMessageFocusRedirect />);

    await waitFor(() => {
      expect(mockRouterReplace).toHaveBeenCalledWith("/inboxes");
    });
  });
});
