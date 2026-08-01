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
