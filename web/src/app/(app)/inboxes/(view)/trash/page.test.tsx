// Per-inbox message Trash tab contract.
//
// Covers: the trash fetch (GET …/messages?deleted=true), empty state,
// row rendering with purge countdown, Restore → POST …/{id}/restore,
// and the two-click Delete forever → DELETE …?permanent=true&confirm=DELETE.

import { render, screen, waitFor, fireEvent } from "../../../../../test-utils/swr";
import AgentTrashPage from "./page";

const mockUseSearchParams = jest.fn();

jest.mock("next/navigation", () => ({
  useSearchParams: () => mockUseSearchParams(),
}));

const mockFetch = jest.fn();
global.fetch = mockFetch;

const EMAIL = "support@acme.com";
const LIST_URL = `/v1/agents/${encodeURIComponent(EMAIL)}/messages?limit=100&deleted=true`;

const trashedMessage = {
  id: "msg_1",
  direction: "inbound",
  from: "alice@gmail.com",
  to: [EMAIL],
  delivered_to: EMAIL,
  subject: "Quarterly report",
  read_status: "read",
  created_at: "2026-07-01T00:00:00Z",
  deleted_at: new Date(Date.now() - 2 * 24 * 3600 * 1000).toISOString(), // 2d ago
};

function mockList(items: unknown[]) {
  mockFetch.mockImplementation((url: string, init?: RequestInit) => {
    if (url === LIST_URL && !init?.method) {
      return Promise.resolve({
        ok: true,
        status: 200,
        json: () => Promise.resolve({ items }),
      });
    }
    return Promise.resolve({
      ok: false,
      status: 404,
      text: () => Promise.resolve("not found"),
    });
  });
}

beforeEach(() => {
  mockFetch.mockReset();
  mockUseSearchParams.mockReturnValue({
    get: (k: string) => (k === "email" ? EMAIL : null),
  });
});

describe("AgentTrashPage", () => {
  it("shows the empty state for an empty trash", async () => {
    mockList([]);
    render(<AgentTrashPage />);
    await waitFor(() => {
      expect(screen.getByTestId("trash-empty")).toBeInTheDocument();
    });
  });

  it("renders trashed messages with deletion age + purge countdown", async () => {
    mockList([trashedMessage]);
    render(<AgentTrashPage />);
    await waitFor(() => {
      expect(screen.getByTestId("trash-row")).toBeInTheDocument();
    });
    expect(screen.getByText("Quarterly report")).toBeInTheDocument();
    // Deleted 2 days ago with a 30-day window → 28 days left.
    expect(screen.getByText(/purges in 28d/)).toBeInTheDocument();
  });

  it("keeps trash flat when deleted rows share a thread_id", async () => {
    mockList([
      { ...trashedMessage, id: "msg_1", thread_id: "thread_A" },
      { ...trashedMessage, id: "msg_2", thread_id: "thread_A" },
    ]);

    render(<AgentTrashPage />);

    await waitFor(() => {
      expect(screen.getAllByTestId("trash-row")).toHaveLength(2);
    });
  });

  it("Restore POSTs to the message restore endpoint", async () => {
    let restored = false;
    mockFetch.mockImplementation((url: string, init?: RequestInit) => {
      if (url === LIST_URL && !init?.method) {
        return Promise.resolve({
          ok: true,
          status: 200,
          json: () => Promise.resolve({ items: restored ? [] : [trashedMessage] }),
        });
      }
      if (
        url === `/v1/agents/${encodeURIComponent(EMAIL)}/messages/msg_1/restore` &&
        init?.method === "POST"
      ) {
        restored = true;
        return Promise.resolve({
          ok: true,
          status: 200,
          json: () => Promise.resolve({ id: "msg_1" }),
        });
      }
      return Promise.resolve({
        ok: false,
        status: 404,
        text: () => Promise.resolve("not found"),
      });
    });

    render(<AgentTrashPage />);
    await waitFor(() => {
      expect(screen.getByRole("button", { name: /^Restore$/ })).toBeInTheDocument();
    });
    fireEvent.click(screen.getByRole("button", { name: /^Restore$/ }));
    await waitFor(() => {
      expect(restored).toBe(true);
    });
  });

  it("Delete forever is two-click and hits the permanent endpoint", async () => {
    let purgedUrl: string | null = null;
    mockFetch.mockImplementation((url: string, init?: RequestInit) => {
      if (url === LIST_URL && !init?.method) {
        return Promise.resolve({
          ok: true,
          status: 200,
          json: () => Promise.resolve({ items: purgedUrl ? [] : [trashedMessage] }),
        });
      }
      if (init?.method === "DELETE") {
        purgedUrl = url;
        return Promise.resolve({
          ok: true,
          status: 204,
          text: () => Promise.resolve(""),
        });
      }
      return Promise.resolve({
        ok: false,
        status: 404,
        text: () => Promise.resolve("not found"),
      });
    });

    render(<AgentTrashPage />);
    await waitFor(() => {
      expect(
        screen.getByRole("button", { name: /Delete forever/ }),
      ).toBeInTheDocument();
    });

    fireEvent.click(screen.getByRole("button", { name: /Delete forever/ }));
    expect(purgedUrl).toBeNull(); // armed only
    fireEvent.click(
      await screen.findByRole("button", { name: /Click again to confirm/ }),
    );
    await waitFor(() => {
      expect(purgedUrl).toBe(
        `/v1/agents/${encodeURIComponent(EMAIL)}/messages/msg_1?permanent=true&confirm=DELETE`,
      );
    });
  });
});
