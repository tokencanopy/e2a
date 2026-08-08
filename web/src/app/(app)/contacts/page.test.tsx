import { act, render, screen, waitFor, within } from "@testing-library/react";
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

  // View switcher to the account-wide suppression list.
  const suppressionsLink = screen.getByRole("link", { name: "Suppressions" });
  expect(suppressionsLink).toHaveAttribute("href", "/contacts/suppressions");
  expect(screen.getByRole("link", { name: "Contacts" })).toHaveAttribute("aria-current", "page");

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

it("renders contact metadata safely and discloses fields beyond the inline cap", async () => {
  fetchMock.mockResolvedValue({
    ok: true,
    json: async () => ({
      items: [{
        address: "partner@example.com",
        display_name: "A. Partner",
        metadata: {
          company: "Example Capital",
          role: "GP",
          notes: "met at demo day",
          score: 42,
          last_contact: null,
          profile: { tier: "gold" },
        },
        source: "import",
        created_at: "2026-07-27T00:00:00Z",
        updated_at: "2026-07-27T00:00:00Z",
      }],
      next_cursor: null,
    }),
  });
  render(<ContactsPage />);

  expect((await screen.findAllByText("Metadata")).length).toBe(2);
  expect(screen.getAllByText("company").length).toBeGreaterThan(0);
  expect(screen.getAllByText("Example Capital").length).toBeGreaterThan(0);
  expect(screen.getAllByText("role").length).toBeGreaterThan(0);
  expect(screen.getAllByText("GP").length).toBeGreaterThan(0);
  expect(screen.getAllByText("notes").length).toBeGreaterThan(0);
  expect(screen.getAllByText("met at demo day").length).toBeGreaterThan(0);

  const summaries = screen.getAllByText("View all 6 metadata fields (3 more)");
  expect(summaries).toHaveLength(2);
  await userEvent.click(summaries[0]);
  const disclosure = summaries[0].closest("details");
  expect(disclosure).not.toBeNull();
  expect(within(disclosure as HTMLElement).getByText("score")).toBeInTheDocument();
  expect(within(disclosure as HTMLElement).getByText("42")).toBeInTheDocument();
  expect(within(disclosure as HTMLElement).getByText("last_contact")).toBeInTheDocument();
  expect(within(disclosure as HTMLElement).getByText("null")).toBeInTheDocument();
  expect(within(disclosure as HTMLElement).getByText('{"tier":"gold"}')).toBeInTheDocument();
  expect(screen.queryByText("[object Object]")).not.toBeInTheDocument();
});

it("does not render a metadata affordance for empty metadata", async () => {
  fetchMock.mockResolvedValue({
    ok: true,
    json: async () => ({
      items: [{
        address: "partner@example.com",
        display_name: "A. Partner",
        metadata: {},
        source: "manual",
        created_at: "2026-07-27T00:00:00Z",
        updated_at: "2026-07-27T00:00:00Z",
      }],
      next_cursor: null,
    }),
  });
  render(<ContactsPage />);

  await screen.findAllByText("A. Partner");
  expect(screen.queryByText("Metadata")).not.toBeInTheDocument();
  expect(screen.queryByText(/View (?:all|full) metadata/)).not.toBeInTheDocument();
});

it("shows metadata read-only in the editor and omits it from PATCH", async () => {
  const contact = {
    address: "partner@example.com",
    display_name: "A. Partner",
    metadata: { company: "Example Capital", score: 42 },
    source: "import",
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

  await userEvent.click(await screen.findByRole("button", { name: "Edit partner@example.com" }));
  const editHeading = await screen.findByRole("heading", { name: "Edit partner@example.com" });
  const editPanel = editHeading.closest("section");
  expect(editPanel).not.toBeNull();
  expect(within(editPanel as HTMLElement).getByText(
    "Metadata is read-only here. Manage it through CSV import or the API.",
  )).toBeInTheDocument();
  const editorMetadataHeading = within(editPanel as HTMLElement).getByText("Metadata");
  const editorMetadata = editorMetadataHeading.closest("div");
  expect(editorMetadata).not.toBeNull();
  expect(within(editorMetadata as HTMLElement).getAllByText("Example Capital").length).toBeGreaterThan(0);
  expect(within(editorMetadata as HTMLElement).getAllByText("42").length).toBeGreaterThan(0);

  const input = screen.getByRole("textbox", { name: "Display name" });
  await userEvent.clear(input);
  await userEvent.type(input, "Renamed");
  await userEvent.click(screen.getByRole("button", { name: "Save contact" }));

  await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(4));
  const [, patchRequest] = fetchMock.mock.calls[2] as [string, RequestInit];
  const body = JSON.parse(patchRequest.body as string) as Record<string, unknown>;
  expect(body).toEqual({ display_name: "Renamed" });
  expect(body).not.toHaveProperty("metadata");
});

it("previews CSV metadata mapping and preserves the import request shape", async () => {
  fetchMock
    .mockResolvedValueOnce({ ok: true, json: async () => ({ items: [], next_cursor: null }) })
    .mockResolvedValueOnce({
      ok: true,
      json: async () => ({
        batch_id: "cimp_preview",
        created: 1,
        updated: 0,
        skipped: 0,
        failed: 0,
        results: [{ index: 0, address: "partner@example.com", status: "created" }],
      }),
    })
    .mockResolvedValueOnce({ ok: true, json: async () => ({ items: [], next_cursor: null }) });
  render(<ContactsPage />);

  await screen.findAllByText("No contacts yet. Create one or import a CSV.");
  await userEvent.click(screen.getByRole("button", { name: "Import CSV" }));
  const csv = [
    "email,name,company,role,notes",
    "partner@example.com,A. Partner,Example Capital,GP,met at demo day",
  ].join("\n");
  const file = new File([csv], "contacts.csv", { type: "text/csv" });
  Object.defineProperty(file, "text", { value: async () => csv });
  await userEvent.upload(screen.getByLabelText("CSV file"), file);

  const mapping = await screen.findByRole("region", { name: "CSV metadata mapping" });
  expect(within(mapping).getByText("company")).toBeInTheDocument();
  expect(within(mapping).getByText("Example Capital")).toBeInTheDocument();
  expect(within(mapping).getByText("role")).toBeInTheDocument();
  expect(within(mapping).getByText("GP")).toBeInTheDocument();
  expect(within(mapping).getByText("notes")).toBeInTheDocument();
  expect(within(mapping).getByText("met at demo day")).toBeInTheDocument();

  await userEvent.selectOptions(screen.getByRole("combobox", { name: "Name column" }), "company");
  expect(within(mapping).getByText("name")).toBeInTheDocument();
  expect(within(mapping).getByText("A. Partner")).toBeInTheDocument();
  expect(within(mapping).queryByText("company")).not.toBeInTheDocument();
  await userEvent.selectOptions(screen.getByRole("combobox", { name: "Name column" }), "name");

  await userEvent.click(screen.getByRole("button", { name: "Import 1 contact" }));
  await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(3));
  const [url, importRequest] = fetchMock.mock.calls[1] as [string, RequestInit];
  expect(url).toBe("/v1/contacts/import");
  expect(JSON.parse(importRequest.body as string)).toEqual({
    contacts: [{
      address: "partner@example.com",
      display_name: "A. Partner",
      metadata: {
        company: "Example Capital",
        role: "GP",
        notes: "met at demo day",
      },
    }],
    on_conflict: "merge",
  });
});

it("states when a CSV has no metadata columns", async () => {
  fetchMock.mockResolvedValue({
    ok: true,
    json: async () => ({ items: [], next_cursor: null }),
  });
  render(<ContactsPage />);

  await screen.findAllByText("No contacts yet. Create one or import a CSV.");
  await userEvent.click(screen.getByRole("button", { name: "Import CSV" }));
  const file = new File(["email,name\npartner@example.com,A. Partner"], "contacts.csv", {
    type: "text/csv",
  });
  Object.defineProperty(file, "text", {
    value: async () => "email,name\npartner@example.com,A. Partner",
  });
  await userEvent.upload(screen.getByLabelText("CSV file"), file);

  expect(await screen.findByText("No columns will be stored as metadata.")).toBeInTheDocument();
});

it("disarms a staged import when a replacement CSV cannot be mapped", async () => {
  fetchMock.mockResolvedValue({
    ok: true,
    json: async () => ({ items: [], next_cursor: null }),
  });
  render(<ContactsPage />);

  await screen.findAllByText("No contacts yet. Create one or import a CSV.");
  await userEvent.click(screen.getByRole("button", { name: "Import CSV" }));
  const importHeading = screen.getByRole("heading", { name: "Import CSV" });
  const importPanel = importHeading.closest("section");
  expect(importPanel).not.toBeNull();
  const fileInput = within(importPanel as HTMLElement).getByLabelText("CSV file");

  const validCsv = "email,name,company\np@x.com,A. Partner,Example Capital";
  const validFile = new File([validCsv], "a.csv", { type: "text/csv" });
  Object.defineProperty(validFile, "text", { value: async () => validCsv });
  await userEvent.upload(fileInput, validFile);
  expect(await within(importPanel as HTMLElement).findByText(/a\.csv: 1 row ready/)).toBeInTheDocument();
  expect(within(importPanel as HTMLElement).getByRole("button", { name: "Import 1 contact" })).toBeEnabled();

  const invalidCsv = "email,name\n";
  const invalidFile = new File([invalidCsv], "b.csv", { type: "text/csv" });
  Object.defineProperty(invalidFile, "text", { value: async () => invalidCsv });
  await userEvent.upload(fileInput, invalidFile);

  expect(await within(importPanel as HTMLElement).findByRole("alert")).toHaveTextContent(
    "CSV must include a header and at least one row",
  );
  expect(within(importPanel as HTMLElement).queryByText(/a\.csv: 1 row ready/)).not.toBeInTheDocument();
  expect(within(importPanel as HTMLElement).queryByText(/b\.csv:/)).not.toBeInTheDocument();
  expect(within(importPanel as HTMLElement).queryByRole("region", {
    name: "CSV metadata mapping",
  })).not.toBeInTheDocument();
  expect(within(importPanel as HTMLElement).getByRole("button", { name: "Import contacts" })).toBeDisabled();
  expect(fetchMock).toHaveBeenCalledTimes(1);
});

it("previews and posts metadata when the selected email header is empty", async () => {
  fetchMock
    .mockResolvedValueOnce({ ok: true, json: async () => ({ items: [], next_cursor: null }) })
    .mockResolvedValueOnce({
      ok: true,
      json: async () => ({
        batch_id: "cimp_empty_header",
        created: 1,
        updated: 0,
        skipped: 0,
        failed: 0,
        results: [{ index: 0, address: "1", status: "created" }],
      }),
    })
    .mockResolvedValueOnce({ ok: true, json: async () => ({ items: [], next_cursor: null }) });
  render(<ContactsPage />);

  await screen.findAllByText("No contacts yet. Create one or import a CSV.");
  await userEvent.click(screen.getByRole("button", { name: "Import CSV" }));
  const csv = ",alias,company\n1,A. Partner,Example Capital";
  const file = new File([csv], "blank-email-header.csv", { type: "text/csv" });
  Object.defineProperty(file, "text", { value: async () => csv });
  await userEvent.upload(screen.getByLabelText("CSV file"), file);

  const mapping = await screen.findByRole("region", { name: "CSV metadata mapping" });
  expect(within(mapping).getByText("alias")).toBeInTheDocument();
  expect(within(mapping).getByText("A. Partner")).toBeInTheDocument();
  expect(within(mapping).getByText("company")).toBeInTheDocument();
  expect(within(mapping).getByText("Example Capital")).toBeInTheDocument();
  expect(within(mapping).queryByText("No columns will be stored as metadata.")).not.toBeInTheDocument();

  await userEvent.click(screen.getByRole("button", { name: "Import 1 contact" }));
  await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(3));
  const [, importRequest] = fetchMock.mock.calls[1] as [string, RequestInit];
  expect(JSON.parse(importRequest.body as string)).toEqual({
    contacts: [{
      address: "1",
      metadata: { alias: "A. Partner", company: "Example Capital" },
    }],
    on_conflict: "merge",
  });
});

it("does not emit duplicate-key warnings for headers that collide after trimming", async () => {
  const consoleError = jest.spyOn(console, "error").mockImplementation(() => undefined);
  try {
    fetchMock.mockResolvedValue({
      ok: true,
      json: async () => ({ items: [], next_cursor: null }),
    });
    render(<ContactsPage />);

    await screen.findAllByText("No contacts yet. Create one or import a CSV.");
    await userEvent.click(screen.getByRole("button", { name: "Import CSV" }));
    const csv = "email,tag, tag\np@x.com,first,second";
    const file = new File([csv], "duplicate-trimmed-headers.csv", { type: "text/csv" });
    Object.defineProperty(file, "text", { value: async () => csv });
    await userEvent.upload(screen.getByLabelText("CSV file"), file);
    await screen.findByRole("region", { name: "CSV metadata mapping" });

    const messages = consoleError.mock.calls.flat().join(" ");
    expect(messages).not.toMatch(/same key|Encountered two children with the same key/i);
  } finally {
    consoleError.mockRestore();
  }
});

it("keeps the latest CSV when an earlier file read resolves last", async () => {
  fetchMock
    .mockResolvedValueOnce({ ok: true, json: async () => ({ items: [], next_cursor: null }) })
    .mockResolvedValueOnce({
      ok: true,
      json: async () => ({
        batch_id: "cimp_latest_file",
        created: 1,
        updated: 0,
        skipped: 0,
        failed: 0,
        results: [{ index: 0, address: "second@example.com", status: "created" }],
      }),
    })
    .mockResolvedValueOnce({ ok: true, json: async () => ({ items: [], next_cursor: null }) });
  render(<ContactsPage />);

  await screen.findAllByText("No contacts yet. Create one or import a CSV.");
  await userEvent.click(screen.getByRole("button", { name: "Import CSV" }));
  const importHeading = screen.getByRole("heading", { name: "Import CSV" });
  const importPanel = importHeading.closest("section");
  expect(importPanel).not.toBeNull();
  const fileInput = within(importPanel as HTMLElement).getByLabelText("CSV file");

  let resolveFirstText!: (value: string) => void;
  let resolveSecondText!: (value: string) => void;
  const firstText = new Promise<string>((resolve) => { resolveFirstText = resolve; });
  const secondText = new Promise<string>((resolve) => { resolveSecondText = resolve; });
  const firstCsv = "email,name,company\nfirst@example.com,First,First Capital";
  const secondCsv = "email,name,role\nsecond@example.com,Second,GP";
  const firstFile = new File([firstCsv], "first.csv", { type: "text/csv" });
  const secondFile = new File([secondCsv], "second.csv", { type: "text/csv" });
  Object.defineProperty(firstFile, "text", { value: () => firstText });
  Object.defineProperty(secondFile, "text", { value: () => secondText });

  await userEvent.upload(fileInput, firstFile);
  await userEvent.upload(fileInput, secondFile);
  await act(async () => {
    resolveSecondText(secondCsv);
    await secondText;
  });
  expect(within(importPanel as HTMLElement).getByText(/second\.csv: 1 row ready/)).toBeInTheDocument();

  await act(async () => {
    resolveFirstText(firstCsv);
    await firstText;
  });
  expect(within(importPanel as HTMLElement).getByText(/second\.csv: 1 row ready/)).toBeInTheDocument();
  expect(within(importPanel as HTMLElement).queryByText(/first\.csv:/)).not.toBeInTheDocument();
  const mapping = within(importPanel as HTMLElement).getByRole("region", {
    name: "CSV metadata mapping",
  });
  expect(within(mapping).getByText("role")).toBeInTheDocument();
  expect(within(mapping).getByText("GP")).toBeInTheDocument();
  expect(within(mapping).queryByText("company")).not.toBeInTheDocument();
  expect(within(mapping).queryByText("First Capital")).not.toBeInTheDocument();

  await userEvent.click(within(importPanel as HTMLElement).getByRole("button", {
    name: "Import 1 contact",
  }));
  await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(3));
  const [, importRequest] = fetchMock.mock.calls[1] as [string, RequestInit];
  expect(JSON.parse(importRequest.body as string)).toEqual({
    contacts: [{
      address: "second@example.com",
      display_name: "Second",
      metadata: { role: "GP" },
    }],
    on_conflict: "merge",
  });
});
