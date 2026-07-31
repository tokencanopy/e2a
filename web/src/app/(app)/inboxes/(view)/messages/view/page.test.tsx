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
  global.fetch = jest.fn() as unknown as typeof fetch;
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

  it("resolves ordinary message links to their canonical thread_id fragment", async () => {
    setSearchParams({
      email: "support@acme.io",
      id: "msg_inbound",
      direction: "inbound",
      headers: "1",
    });
    (global.fetch as jest.Mock).mockResolvedValue({
      ok: true,
      status: 200,
      text: () =>
        Promise.resolve(
          JSON.stringify({
            id: "msg_inbound",
            direction: "inbound",
            thread_id: "客户 1%ready",
            conversation_id: "客户 1%ready",
          }),
        ),
    });

    render(<LegacyMessageFocusRedirect />);

    await waitFor(() => {
      expect(mockRouterReplace).toHaveBeenCalledWith(
        "/inboxes/messages?email=support%40acme.io#thr:%E5%AE%A2%E6%88%B7%201%25ready",
      );
    });
  });

  it("preserves legacy conversation navigation for an older server without thread_id", async () => {
    setSearchParams({
      email: "support@acme.io",
      id: "msg_legacy",
      direction: "inbound",
    });
    (global.fetch as jest.Mock).mockResolvedValue({
      ok: true,
      status: 200,
      text: () =>
        Promise.resolve(
          JSON.stringify({
            id: "msg_legacy",
            direction: "inbound",
            conversation_id: "legacy workflow",
          }),
        ),
    });

    render(<LegacyMessageFocusRedirect />);

    await waitFor(() => {
      expect(mockRouterReplace).toHaveBeenCalledWith(
        "/inboxes/messages?email=support%40acme.io#conv:legacy%20workflow",
      );
    });
  });

  it("falls back to the owning inbox when legacy detail lookup fails", async () => {
    setSearchParams({ email: "support@acme.io", id: "msg_missing" });
    (global.fetch as jest.Mock).mockResolvedValue({
      ok: false,
      status: 404,
      text: () => Promise.resolve("not found"),
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
