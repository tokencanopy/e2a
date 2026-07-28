import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import ContactsPage from "./page";

jest.mock("../../components/hooks/useAgents", () => ({
  useAgents: () => ({
    agents: [{ email: "raise@example.com" }],
    isLoading: false,
    error: null,
  }),
}));
jest.mock("../../components/AgentPromptCard", () => ({
  AgentPromptCard: () => <div>Agent setup prompt</div>,
}));

const fetchMock = jest.fn();
beforeEach(() => {
  fetchMock.mockReset();
  global.fetch = fetchMock as unknown as typeof fetch;
});

it("loads contacts and exposes create/import actions", async () => {
  fetchMock.mockResolvedValue({
    ok: true,
    json: async () => ({
      items: [{
        address: "partner@fund.vc",
        display_name: "A. Partner",
        metadata: {},
        source: "import",
        created_at: "2026-07-27T00:00:00Z",
        updated_at: "2026-07-27T00:00:00Z",
      }],
      next_cursor: null,
    }),
  });
  render(<ContactsPage />);

  expect((await screen.findAllByText("A. Partner")).length).toBeGreaterThan(0);
  expect(screen.getAllByText("partner@fund.vc").length).toBeGreaterThan(0);
  expect(screen.getByRole("button", { name: "Import CSV" })).toBeInTheDocument();

  await userEvent.click(screen.getByRole("button", { name: "New contact" }));
  expect(screen.getByRole("textbox", { name: "Email address" })).toBeInTheDocument();
});

it("surfaces list failures with a retry action", async () => {
  fetchMock.mockResolvedValue({
    ok: false,
    status: 503,
    json: async () => ({ error: { message: "Contacts are temporarily unavailable" } }),
  });
  render(<ContactsPage />);
  await waitFor(() =>
    expect(screen.getByRole("alert")).toHaveTextContent("Contacts are temporarily unavailable"),
  );
  expect(screen.getByRole("button", { name: "Try again" })).toBeInTheDocument();
});

it("edits in an accessible form and guards the write with If-Match", async () => {
  const contact = {
    address: "partner@fund.vc",
    display_name: "A. Partner",
    metadata: {},
    source: "manual",
    created_at: "2026-07-27T00:00:00Z",
    updated_at: "2026-07-27T00:00:00Z",
  };
  fetchMock
    .mockResolvedValueOnce({ ok: true, json: async () => ({ items: [contact], next_cursor: null }) })
    .mockResolvedValueOnce({
      ok: true,
      headers: new Headers({ ETag: '"contact-v1"' }),
      json: async () => contact,
    })
    .mockResolvedValueOnce({ ok: true, json: async () => ({ ...contact, display_name: "Renamed" }) })
    .mockResolvedValueOnce({
      ok: true,
      json: async () => ({ items: [{ ...contact, display_name: "Renamed" }], next_cursor: null }),
    });
  render(<ContactsPage />);

  await userEvent.click(await screen.findByRole("button", { name: "Edit partner@fund.vc" }));
  const input = await screen.findByRole("textbox", { name: "Display name" });
  await userEvent.clear(input);
  await userEvent.type(input, "Renamed");
  await userEvent.click(screen.getByRole("button", { name: "Save contact" }));

  await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(4));
  const [, patch] = fetchMock.mock.calls[2] as [string, RequestInit];
  expect(patch.method).toBe("PATCH");
  expect((patch.headers as Record<string, string>)["If-Match"]).toBe('"contact-v1"');
});

it("keeps contact editing fail-closed when the fresh version cannot be loaded", async () => {
  const contact = {
    address: "partner@fund.vc",
    display_name: "Stale list value",
    metadata: {},
    source: "manual",
    created_at: "2026-07-27T00:00:00Z",
    updated_at: "2026-07-27T00:00:00Z",
  };
  fetchMock
    .mockResolvedValueOnce({ ok: true, json: async () => ({ items: [contact], next_cursor: null }) })
    .mockResolvedValueOnce({
      ok: false,
      status: 503,
      json: async () => ({ error: { message: "Could not load the latest contact" } }),
    });
  render(<ContactsPage />);

  await userEvent.click(await screen.findByRole("button", { name: "Edit partner@fund.vc" }));

  expect(await screen.findByRole("alert")).toHaveTextContent("Could not load the latest contact");
  expect(screen.getByRole("textbox", { name: "Display name" })).toBeDisabled();
  expect(screen.getByRole("button", { name: "Save contact" })).toBeDisabled();
  expect(fetchMock).toHaveBeenCalledTimes(2);
});

it("clears the prior contact version before switching editors", async () => {
  const first = {
    address: "first@fund.vc",
    display_name: "First",
    metadata: {},
    source: "manual",
    created_at: "2026-07-27T00:00:00Z",
    updated_at: "2026-07-27T00:00:00Z",
  };
  const second = { ...first, address: "second@fund.vc", display_name: "Second" };
  fetchMock
    .mockResolvedValueOnce({ ok: true, json: async () => ({ items: [first, second], next_cursor: null }) })
    .mockResolvedValueOnce({
      ok: true,
      headers: new Headers({ ETag: '"first-v1"' }),
      json: async () => first,
    })
    .mockResolvedValueOnce({
      ok: false,
      status: 503,
      json: async () => ({ error: { message: "Could not load the second contact" } }),
    });
  render(<ContactsPage />);

  await userEvent.click(await screen.findByRole("button", { name: "Edit first@fund.vc" }));
  await waitFor(() => expect(screen.getByRole("button", { name: "Save contact" })).toBeEnabled());
  await userEvent.click(screen.getByRole("button", { name: "Edit second@fund.vc" }));

  expect(await screen.findByRole("alert")).toHaveTextContent("Could not load the second contact");
  expect(screen.getByRole("textbox", { name: "Display name" })).toBeDisabled();
  expect(screen.getByRole("button", { name: "Save contact" })).toBeDisabled();
});

it("shows a reversible import receipt and refreshes the contact list", async () => {
  jest.spyOn(window, "confirm").mockReturnValue(true);
  fetchMock
    .mockResolvedValueOnce({ ok: true, json: async () => ({ items: [], next_cursor: null }) })
    .mockResolvedValueOnce({
      ok: true,
      json: async () => ({
        batch_id: "cimp_test",
        created: 1,
        updated: 0,
        skipped: 0,
        failed: 0,
        results: [{ index: 0, address: "new@example.com", status: "created" }],
      }),
    })
    .mockResolvedValueOnce({
      ok: true,
      json: async () => ({
        items: [{
          address: "new@example.com",
          display_name: "New",
          metadata: {},
          source: "import",
          import_batch_id: "cimp_test",
          created_at: "2026-07-27T00:00:00Z",
          updated_at: "2026-07-27T00:00:00Z",
        }],
        next_cursor: null,
      }),
    })
    .mockResolvedValueOnce({
      ok: true,
      json: async () => ({
        deleted: true,
        batch_id: "cimp_test",
        contacts_deleted: 1,
        contacts_retained: 0,
      }),
    })
    .mockResolvedValueOnce({ ok: true, json: async () => ({ items: [], next_cursor: null }) });

  render(<ContactsPage />);
  expect((await screen.findAllByText("No contacts yet. Create one or import a CSV.")).length).toBeGreaterThan(0);
  await userEvent.click(screen.getByRole("button", { name: "Import CSV" }));
  const file = new File(["email,name\nnew@example.com,New"], "contacts.csv", { type: "text/csv" });
  Object.defineProperty(file, "text", {
    value: async () => "email,name\nnew@example.com,New",
  });
  await userEvent.upload(screen.getByLabelText("CSV file"), file);
  await userEvent.click(await screen.findByRole("button", { name: "Import 1 contact" }));
  const [, importRequest] = fetchMock.mock.calls[1] as [string, RequestInit];
  expect((importRequest.headers as Record<string, string>)["Idempotency-Key"]).toBeTruthy();
  await userEvent.click(await screen.findByRole("button", { name: "Reverse import" }));

  expect(await screen.findByText(
    "1 contacts and 0 enrolments removed · 0 contacts retained because they have correspondence history",
  )).toBeInTheDocument();
  const [url, request] = fetchMock.mock.calls[3] as [string, RequestInit];
  expect(url).toBe("/v1/contacts/imports/cimp_test?confirm=DELETE");
  expect(request.method).toBe("DELETE");
});
