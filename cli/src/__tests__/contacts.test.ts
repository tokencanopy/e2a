import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { mkdtemp, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

const mockImport = vi.fn();
const mockSetOutreach = vi.fn();
const mockList = vi.fn();
const mockGetWithETag = vi.fn();
const mockUpdate = vi.fn();
const mockCreate = vi.fn();

vi.mock("../sdk.js", () => ({
  createClient: vi.fn(() => ({
    contacts: {
      import: mockImport,
      setOutreach: mockSetOutreach,
      list: mockList,
      getWithETag: mockGetWithETag,
      update: mockUpdate,
      create: mockCreate,
    },
  })),
  requireAgentEmail: vi.fn((agent?: string) => agent ?? "bot@agents.e2a.dev"),
}));

describe("contacts commands", () => {
  let stdout: ReturnType<typeof vi.spyOn>;

  beforeEach(() => {
    stdout = vi.spyOn(process.stdout, "write").mockImplementation(() => true);
    vi.spyOn(process.stderr, "write").mockImplementation(() => true);
    vi.spyOn(process, "exit").mockImplementation(() => {
      throw new Error("process.exit");
    });
  });

  afterEach(() => {
    vi.restoreAllMocks();
    vi.clearAllMocks();
  });

  it("parses BOM, quoted commas, escaped quotes, CRLF, and quoted newlines", async () => {
    const { parseCSV } = await import("../commands/contacts.js");
    expect(parseCSV('\uFEFFemail,name,notes\r\n"a@x.com","Doe, Jane","said ""yes"""\r\nb@x.com,Bob,"two\nlines"\r\n'))
      .toEqual([
        ["email", "name", "notes"],
        ["a@x.com", "Doe, Jane", 'said "yes"'],
        ["b@x.com", "Bob", "two\nlines"],
      ]);
  });

  it("dry-run previews CSV without calling the API", async () => {
    const dir = await mkdtemp(join(tmpdir(), "e2a-contacts-"));
    const path = join(dir, "contacts.csv");
    await writeFile(path, "email,name,fund\npartner@fund.vc,A. Partner,Example Capital\n");
    const { contactsImport } = await import("../commands/contacts.js");

    await contactsImport(path, {
      agent: "raise@example.com",
      stage: "prospect",
      dryRun: true,
      json: true,
    });

    expect(mockImport).not.toHaveBeenCalled();
    const preview = JSON.parse(String(stdout.mock.calls[0][0]));
    expect(preview.rows).toBe(1);
    expect(preview.contacts[0]).toEqual({
      address: "partner@fund.vc",
      displayName: "A. Partner",
      metadata: { fund: "Example Capital" },
    });
  });

  it("imports parsed rows and maps enrollment options", async () => {
    const dir = await mkdtemp(join(tmpdir(), "e2a-contacts-"));
    const path = join(dir, "contacts.csv");
    await writeFile(path, "address,full name\npartner@fund.vc,A. Partner\n");
    mockImport.mockResolvedValue({
      batchId: "imp_1", created: 1, updated: 0, skipped: 0, failed: 0, results: [],
    });
    const { contactsImport } = await import("../commands/contacts.js");

    await contactsImport(path, {
      emailColumn: "address",
      nameColumn: "full name",
      agent: "raise@example.com",
      stage: "prospect",
      onConflict: "skip",
      idempotencyKey: "contacts:upload:sha256",
    });

    expect(mockImport).toHaveBeenCalledWith({
      contacts: [{ address: "partner@fund.vc", displayName: "A. Partner" }],
      onConflict: "skip",
      agentEmail: "raise@example.com",
      stage: "prospect",
    }, { idempotencyKey: "contacts:upload:sha256" });
  });

  it("maps next-action clear to explicit null", async () => {
    mockSetOutreach.mockResolvedValue({ address: "partner@fund.vc" });
    const { outreachSet } = await import("../commands/contacts.js");
    await outreachSet("partner@fund.vc", {
      agent: "raise@example.com",
      nextAction: "clear",
    });
    expect(mockSetOutreach).toHaveBeenCalledWith(
      "raise@example.com",
      "partner@fund.vc",
      { stage: undefined, metadata: undefined, nextActionAt: null },
    );
  });

  it("maps contact creation-time filters", async () => {
    mockList.mockReturnValue({ toArray: vi.fn(async () => []) });
    const { contactsList } = await import("../commands/contacts.js");
    await contactsList({
      createdAfter: "2026-07-01T00:00:00Z",
      createdBefore: "2026-08-01T00:00:00Z",
    });
    expect(mockList).toHaveBeenCalledWith({
      source: undefined,
      importBatchId: undefined,
      createdAfter: new Date("2026-07-01T00:00:00Z"),
      createdBefore: new Date("2026-08-01T00:00:00Z"),
    });
  });

  it("prints a contact ETag and forwards it on update", async () => {
    mockGetWithETag.mockResolvedValue({
      data: { address: "partner@fund.vc", displayName: "", source: "manual", metadata: {} },
      etag: '"contact-v1"',
    });
    mockUpdate.mockResolvedValue({ address: "partner@fund.vc" });
    const { contactsGet, contactsUpdate } = await import("../commands/contacts.js");
    await contactsGet("partner@fund.vc", { json: true });
    expect(JSON.parse(String(stdout.mock.calls[0][0])).etag).toBe('"contact-v1"');
    await contactsUpdate("partner@fund.vc", {
      name: "Renamed",
      ifMatch: '"contact-v1"',
    });
    expect(mockUpdate).toHaveBeenCalledWith(
      "partner@fund.vc",
      { displayName: "Renamed", metadata: undefined },
      { ifMatch: '"contact-v1"' },
    );
  });

  it("forwards create idempotency and explicit clear operations", async () => {
    mockCreate.mockResolvedValue({ address: "partner@fund.vc" });
    mockUpdate.mockResolvedValue({ address: "partner@fund.vc" });
    mockSetOutreach.mockResolvedValue({ address: "partner@fund.vc" });
    const { contactsCreate, contactsUpdate, outreachSet } = await import("../commands/contacts.js");
    await contactsCreate("partner@fund.vc", { idempotencyKey: "contact:partner" });
    expect(mockCreate).toHaveBeenCalledWith(
      { address: "partner@fund.vc", displayName: undefined, metadata: undefined },
      { idempotencyKey: "contact:partner" },
    );
    await contactsUpdate("partner@fund.vc", { clearName: true });
    expect(mockUpdate).toHaveBeenCalledWith(
      "partner@fund.vc",
      { displayName: "", metadata: undefined },
    );
    await outreachSet("partner@fund.vc", { agent: "raise@example.com", clearStage: true });
    expect(mockSetOutreach).toHaveBeenCalledWith(
      "raise@example.com",
      "partner@fund.vc",
      { stage: "", metadata: undefined },
    );
  });
});
