import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import AccountSuppressionsPage from "./page";

const fetchMock = jest.fn();
beforeEach(() => {
  fetchMock.mockReset();
  global.fetch = fetchMock as unknown as typeof fetch;
});

const bounceRow = {
  address: "gone@example.net",
  reason: "smtp; 550 5.1.1 The email account that you tried to reach does not exist.",
  source: "bounce",
  source_message_id: "msg_48e46",
  created_at: "2026-07-06T18:42:20Z",
};

it("lists the account-wide suppression list with source and provenance", async () => {
  fetchMock.mockResolvedValue({
    ok: true,
    json: async () => ({ items: [bounceRow] }),
  });
  render(<AccountSuppressionsPage />);

  expect((await screen.findAllByText("gone@example.net")).length).toBeGreaterThan(0);
  expect(screen.getAllByText("Bounce").length).toBeGreaterThan(0);
  expect(screen.getAllByText(/550 5\.1\.1/).length).toBeGreaterThan(0);
  expect(screen.getAllByText("msg_48e46").length).toBeGreaterThan(0);
  // Scope statement: this list blocks sends from EVERY inbox.
  expect(screen.getByText(/every inbox/i)).toBeInTheDocument();
  const [url] = fetchMock.mock.calls[0] as [string];
  expect(url).toContain("/v1/account/suppressions");
});

it("links back and forth between the Contacts and Suppressions views", async () => {
  fetchMock.mockResolvedValue({ ok: true, json: async () => ({ items: [] }) });
  render(<AccountSuppressionsPage />);

  const contactsLink = await screen.findByRole("link", { name: "Contacts" });
  expect(contactsLink).toHaveAttribute("href", "/contacts");
  const selfLink = screen.getByRole("link", { name: "Suppressions" });
  expect(selfLink).toHaveAttribute("href", "/contacts/suppressions");
  expect(selfLink).toHaveAttribute("aria-current", "page");
});

it("removes an entry behind a consequence-stating confirm", async () => {
  fetchMock
    .mockResolvedValueOnce({ ok: true, json: async () => ({ items: [bounceRow] }) })
    .mockResolvedValueOnce({ ok: true, json: async () => ({ deleted: true, address: bounceRow.address }) })
    .mockResolvedValueOnce({ ok: true, json: async () => ({ items: [] }) });
  const confirmSpy = jest.spyOn(window, "confirm").mockReturnValue(true);
  render(<AccountSuppressionsPage />);

  await userEvent.click((await screen.findAllByRole("button", { name: `Remove ${bounceRow.address}` }))[0]);

  await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(3));
  // The confirm copy must state the deliverability consequence, not just "sure?".
  expect(String(confirmSpy.mock.calls[0][0])).toMatch(/reputation/i);
  const [url, init] = fetchMock.mock.calls[1] as [string, RequestInit];
  expect(url).toBe("/v1/account/suppressions/gone%40example.net?confirm=DELETE");
  expect(init.method).toBe("DELETE");
  confirmSpy.mockRestore();
});

it("does not remove when the confirm is declined", async () => {
  fetchMock.mockResolvedValue({ ok: true, json: async () => ({ items: [bounceRow] }) });
  const confirmSpy = jest.spyOn(window, "confirm").mockReturnValue(false);
  render(<AccountSuppressionsPage />);

  await userEvent.click((await screen.findAllByRole("button", { name: `Remove ${bounceRow.address}` }))[0]);

  expect(fetchMock).toHaveBeenCalledTimes(1);
  confirmSpy.mockRestore();
});

it("surfaces the server error when the list cannot load", async () => {
  fetchMock.mockResolvedValue({
    ok: false,
    status: 501,
    json: async () => ({ error: { message: "suppressions are not available on this deployment" } }),
  });
  render(<AccountSuppressionsPage />);

  expect(await screen.findByRole("alert")).toHaveTextContent(
    "suppressions are not available on this deployment",
  );
});

it("never shows the all-clear empty state while the list failed to load", async () => {
  fetchMock.mockResolvedValue({
    ok: false,
    status: 401,
    json: async () => ({ error: { message: "unauthorized" } }),
  });
  render(<AccountSuppressionsPage />);

  await screen.findByRole("alert");
  expect(screen.queryByText(/No suppressed recipients/)).not.toBeInTheDocument();
});

it("reconverges with the server when a remove fails", async () => {
  fetchMock
    .mockResolvedValueOnce({ ok: true, json: async () => ({ items: [bounceRow] }) })
    .mockResolvedValueOnce({
      ok: false,
      status: 404,
      json: async () => ({ error: { message: "address not on the suppression list" } }),
    })
    .mockResolvedValueOnce({ ok: true, json: async () => ({ items: [] }) });
  const confirmSpy = jest.spyOn(window, "confirm").mockReturnValue(true);
  render(<AccountSuppressionsPage />);

  await userEvent.click((await screen.findAllByRole("button", { name: `Remove ${bounceRow.address}` }))[0]);

  await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(3));
  await waitFor(() => expect(screen.queryByText(bounceRow.address)).not.toBeInTheDocument());
  expect(screen.getByRole("alert")).toHaveTextContent("address not on the suppression list");
  confirmSpy.mockRestore();
});

it("refuses to build a request from a path-traversal address", async () => {
  // ".." would collapse DELETE /v1/account/suppressions/.. onto /v1/account —
  // itself a real DELETE endpoint taking the same ?confirm=DELETE token.
  fetchMock.mockResolvedValue({
    ok: true,
    json: async () => ({ items: [{ ...bounceRow, address: ".." }] }),
  });
  const confirmSpy = jest.spyOn(window, "confirm").mockReturnValue(true);
  render(<AccountSuppressionsPage />);

  await userEvent.click((await screen.findAllByRole("button", { name: "Remove .." }))[0]);

  expect(fetchMock).toHaveBeenCalledTimes(1); // list only; no DELETE issued
  expect(confirmSpy).not.toHaveBeenCalled();
  expect(await screen.findByRole("alert")).toHaveTextContent(/not a valid address/i);
  confirmSpy.mockRestore();
});

it("does not double-submit a remove on a double click", async () => {
  fetchMock
    .mockResolvedValueOnce({ ok: true, json: async () => ({ items: [bounceRow] }) })
    .mockImplementationOnce(() => new Promise((resolve) => setTimeout(() => resolve({
      ok: true, json: async () => ({ deleted: true, address: bounceRow.address }),
    }), 30)))
    .mockResolvedValue({ ok: true, json: async () => ({ items: [] }) });
  const confirmSpy = jest.spyOn(window, "confirm").mockReturnValue(true);
  render(<AccountSuppressionsPage />);

  const button = (await screen.findAllByRole("button", { name: `Remove ${bounceRow.address}` }))[0];
  await userEvent.click(button);
  await userEvent.click(button);

  await waitFor(() => expect(screen.queryByText(bounceRow.address)).not.toBeInTheDocument());
  const deletes = fetchMock.mock.calls.filter((call) => (call[1] as RequestInit | undefined)?.method === "DELETE");
  expect(deletes).toHaveLength(1);
  confirmSpy.mockRestore();
});

it("keeps appended pages free of duplicate rows", async () => {
  const second = { ...bounceRow, address: "later@example.net" };
  fetchMock
    .mockResolvedValueOnce({ ok: true, json: async () => ({ items: [bounceRow], next_cursor: "c2" }) })
    .mockResolvedValueOnce({ ok: true, json: async () => ({ items: [bounceRow, second] }) });
  render(<AccountSuppressionsPage />);

  await userEvent.click(await screen.findByRole("button", { name: "Load more suppressions" }));

  await waitFor(() => expect(screen.getAllByText("later@example.net").length).toBeGreaterThan(0));
  const [url] = fetchMock.mock.calls[1] as [string];
  expect(url).toContain("cursor=c2");
  expect(screen.getAllByText(bounceRow.address)).toHaveLength(2);
});
