import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import SuppressionsPage from "./page";

jest.mock("next/navigation", () => ({
  useSearchParams: () => new URLSearchParams("email=raise%40example.com"),
}));

const fetchMock = jest.fn();
beforeEach(() => {
  fetchMock.mockReset();
  global.fetch = fetchMock as unknown as typeof fetch;
});

const unsubscribeRow = {
  agent_email: "raise@example.com",
  address: "optout@example.net",
  reason: "clicked unsubscribe",
  source: "unsubscribe",
  created_at: "2026-07-20T09:00:00Z",
};

it("lists the agent's blocked recipients with source and reason", async () => {
  fetchMock.mockResolvedValue({
    ok: true,
    json: async () => ({ items: [unsubscribeRow] }),
  });
  render(<SuppressionsPage />);

  expect((await screen.findAllByText("optout@example.net")).length).toBeGreaterThan(0);
  expect(screen.getAllByText("Unsubscribe").length).toBeGreaterThan(0);
  expect(screen.getAllByText(/clicked unsubscribe/).length).toBeGreaterThan(0);
  // Scope note: account-level bounce/complaint blocks live elsewhere.
  expect(screen.getByText(/only for this inbox/i)).toBeInTheDocument();
  const [url] = fetchMock.mock.calls[0] as [string];
  expect(url).toContain("/v1/agents/raise%40example.com/suppressions");
});

it("adds a manual block and refreshes the list", async () => {
  fetchMock
    .mockResolvedValueOnce({ ok: true, json: async () => ({ items: [] }) })
    .mockResolvedValueOnce({
      ok: true,
      json: async () => ({ ...unsubscribeRow, address: "noreply@example.net", source: "manual" }),
    })
    .mockResolvedValueOnce({ ok: true, json: async () => ({ items: [unsubscribeRow] }) });
  render(<SuppressionsPage />);

  await userEvent.click(await screen.findByRole("button", { name: "Suppress an address" }));
  await userEvent.type(screen.getByRole("textbox", { name: "Address" }), "noreply@example.net");
  await userEvent.type(screen.getByRole("textbox", { name: "Reason" }), "test data");
  await userEvent.click(screen.getByRole("button", { name: "Add suppression" }));

  await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(3));
  const [url, init] = fetchMock.mock.calls[1] as [string, RequestInit];
  expect(url).toBe("/v1/agents/raise%40example.com/suppressions");
  expect(init.method).toBe("POST");
  expect(JSON.parse(String(init.body))).toEqual({
    address: "noreply@example.net",
    reason: "test data",
  });
});

it("removes a block behind an explicit confirm", async () => {
  fetchMock
    .mockResolvedValueOnce({ ok: true, json: async () => ({ items: [unsubscribeRow] }) })
    .mockResolvedValueOnce({ ok: true, json: async () => ({ deleted: true, address: unsubscribeRow.address }) })
    .mockResolvedValueOnce({ ok: true, json: async () => ({ items: [] }) });
  const confirmSpy = jest.spyOn(window, "confirm").mockReturnValue(true);
  render(<SuppressionsPage />);

  await userEvent.click((await screen.findAllByRole("button", { name: `Remove ${unsubscribeRow.address}` }))[0]);

  await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(3));
  const [url, init] = fetchMock.mock.calls[1] as [string, RequestInit];
  expect(url).toBe("/v1/agents/raise%40example.com/suppressions/optout%40example.net?confirm=DELETE");
  expect(init.method).toBe("DELETE");
  confirmSpy.mockRestore();
});

it("does not remove when the confirm is declined", async () => {
  fetchMock.mockResolvedValue({ ok: true, json: async () => ({ items: [unsubscribeRow] }) });
  const confirmSpy = jest.spyOn(window, "confirm").mockReturnValue(false);
  render(<SuppressionsPage />);

  await userEvent.click((await screen.findAllByRole("button", { name: `Remove ${unsubscribeRow.address}` }))[0]);

  expect(fetchMock).toHaveBeenCalledTimes(1);
  confirmSpy.mockRestore();
});

it("surfaces the server error when suppressions are unavailable", async () => {
  fetchMock.mockResolvedValue({
    ok: false,
    status: 501,
    json: async () => ({ error: { message: "agent suppressions are not available on this deployment" } }),
  });
  render(<SuppressionsPage />);

  expect(await screen.findByRole("alert")).toHaveTextContent(
    "agent suppressions are not available on this deployment",
  );
});

it("never shows the all-clear empty state while the list failed to load", async () => {
  // A suppression list is a safety list: "no blocked recipients" after a failed
  // fetch reads as the exact inverse of the truth.
  fetchMock.mockResolvedValue({
    ok: false,
    status: 501,
    json: async () => ({ error: { message: "agent suppressions are not available on this deployment" } }),
  });
  render(<SuppressionsPage />);

  await screen.findByRole("alert");
  expect(screen.queryByText(/No blocked recipients for this inbox/)).not.toBeInTheDocument();
});

it("reconverges with the server when a remove fails", async () => {
  fetchMock
    .mockResolvedValueOnce({ ok: true, json: async () => ({ items: [unsubscribeRow] }) })
    .mockResolvedValueOnce({
      ok: false,
      status: 404,
      json: async () => ({ error: { message: "address not on the agent suppression list" } }),
    })
    .mockResolvedValueOnce({ ok: true, json: async () => ({ items: [] }) });
  const confirmSpy = jest.spyOn(window, "confirm").mockReturnValue(true);
  render(<SuppressionsPage />);

  await userEvent.click((await screen.findAllByRole("button", { name: `Remove ${unsubscribeRow.address}` }))[0]);

  // Failed delete still refetches, so a row removed elsewhere disappears
  // instead of lingering as if still blocked.
  await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(3));
  await waitFor(() =>
    expect(screen.queryByText(unsubscribeRow.address)).not.toBeInTheDocument(),
  );
  expect(screen.getByRole("alert")).toHaveTextContent("address not on the agent suppression list");
  confirmSpy.mockRestore();
});

it("refuses to build a request from a path-traversal address", async () => {
  // encodeURIComponent leaves "." alone, so an address of ".." would collapse
  // DELETE /v1/agents/x/suppressions/.. onto the AGENT resource itself.
  fetchMock.mockResolvedValue({
    ok: true,
    json: async () => ({ items: [{ ...unsubscribeRow, address: ".." }] }),
  });
  const confirmSpy = jest.spyOn(window, "confirm").mockReturnValue(true);
  render(<SuppressionsPage />);

  await userEvent.click((await screen.findAllByRole("button", { name: "Remove .." }))[0]);

  expect(fetchMock).toHaveBeenCalledTimes(1); // list only; no DELETE issued
  expect(await screen.findByRole("alert")).toHaveTextContent(/not a valid address/i);
  confirmSpy.mockRestore();
});

it("does not double-submit a remove on a double click", async () => {
  fetchMock
    .mockResolvedValueOnce({ ok: true, json: async () => ({ items: [unsubscribeRow] }) })
    .mockImplementationOnce(() => new Promise((resolve) => setTimeout(() => resolve({
      ok: true, json: async () => ({ deleted: true, address: unsubscribeRow.address }),
    }), 30)))
    .mockResolvedValue({ ok: true, json: async () => ({ items: [] }) });
  const confirmSpy = jest.spyOn(window, "confirm").mockReturnValue(true);
  render(<SuppressionsPage />);

  const button = (await screen.findAllByRole("button", { name: `Remove ${unsubscribeRow.address}` }))[0];
  await userEvent.click(button);
  await userEvent.click(button);

  await waitFor(() => expect(screen.queryByText(unsubscribeRow.address)).not.toBeInTheDocument());
  const deletes = fetchMock.mock.calls.filter((call) => (call[1] as RequestInit | undefined)?.method === "DELETE");
  expect(deletes).toHaveLength(1);
  confirmSpy.mockRestore();
});

it("keeps appended pages free of duplicate rows", async () => {
  const second = { ...unsubscribeRow, address: "later@example.net" };
  fetchMock
    .mockResolvedValueOnce({ ok: true, json: async () => ({ items: [unsubscribeRow], next_cursor: "c2" }) })
    // A server that re-emits a row across pages must not produce duplicate keys.
    .mockResolvedValueOnce({ ok: true, json: async () => ({ items: [unsubscribeRow, second] }) });
  render(<SuppressionsPage />);

  await userEvent.click(await screen.findByRole("button", { name: "Load more suppressions" }));

  await waitFor(() => expect(screen.getAllByText("later@example.net").length).toBeGreaterThan(0));
  const [url] = fetchMock.mock.calls[1] as [string];
  expect(url).toContain("cursor=c2");
  // One row per address across both pages (mobile + desktop render each once).
  const rendered = screen.getAllByText(unsubscribeRow.address);
  expect(rendered).toHaveLength(2);
});
