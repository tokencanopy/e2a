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
const mockDelete = vi.fn();
const mockDeleteImport = vi.fn();
const mockOutreach = vi.fn();
const mockGetOutreachWithETag = vi.fn();
const mockDeleteOutreach = vi.fn();

vi.mock("../sdk.js", () => ({
  createClient: vi.fn(() => ({
    contacts: {
      import: mockImport,
      setOutreach: mockSetOutreach,
      list: mockList,
      getWithETag: mockGetWithETag,
      update: mockUpdate,
      create: mockCreate,
      delete: mockDelete,
      deleteImport: mockDeleteImport,
      outreach: mockOutreach,
      getOutreachWithETag: mockGetOutreachWithETag,
      deleteOutreach: mockDeleteOutreach,
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

  it("normalizes CRLF to LF inside a quoted multi-line field instead of leaking a raw CR", async () => {
    const { parseCSV } = await import("../commands/contacts.js");
    expect(parseCSV('email,notes\r\na@x.com,"two\r\nlines"\r\n')).toEqual([
      ["email", "notes"],
      ["a@x.com", "two\nlines"],
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

  it("covers destructive receipts and outreach list/read/delete output", async () => {
    mockDelete.mockResolvedValue({ deleted: true, address: "partner@fund.vc" });
    mockDeleteImport.mockResolvedValue({
      deleted: true,
      batchId: "imp_1",
      contactsDeleted: 2,
      contactsRetained: 1,
      engagementsDeleted: 3,
    });
    mockOutreach.mockReturnValue({
      toArray: vi.fn(async () => [{
        address: "partner@fund.vc",
        stage: "touch2",
        nextActionAt: new Date("2026-08-01T00:00:00Z"),
        replied: false,
        suppressed: false,
      }]),
    });
    mockGetOutreachWithETag.mockResolvedValue({
      data: {
        address: "partner@fund.vc",
        stage: "touch2",
        nextActionAt: undefined,
      },
      etag: '"outreach-v1"',
    });
    mockDeleteOutreach.mockResolvedValue({ deleted: true, address: "partner@fund.vc" });
    const {
      contactsDelete,
      contactsDeleteImport,
      outreachList,
      outreachGet,
      outreachDelete,
    } = await import("../commands/contacts.js");

    await contactsDelete("partner@fund.vc", { json: false });
    await contactsDeleteImport("imp_1", { json: false });
    await outreachList({
      agent: "raise@example.com",
      replied: "false",
      suppressed: "false",
      nextActionBefore: "2026-08-01T00:00:00Z",
      lastOutboundBefore: "2026-07-01T00:00:00Z",
      limit: "1",
      json: false,
    });
    await outreachGet("partner@fund.vc", { agent: "raise@example.com", json: true });
    await outreachDelete("partner@fund.vc", { agent: "raise@example.com", json: false });

    expect(mockOutreach).toHaveBeenCalledWith("raise@example.com", {
      stage: undefined,
      replied: false,
      suppressed: false,
      nextActionBefore: new Date("2026-08-01T00:00:00Z"),
      lastOutboundBefore: new Date("2026-07-01T00:00:00Z"),
    });
    expect(mockDeleteOutreach).toHaveBeenCalledWith(
      "raise@example.com",
      "partner@fund.vc",
    );
  });

  it("rejects conflicting clear flags and malformed list filters", async () => {
    mockOutreach.mockReturnValue({ toArray: vi.fn(async () => []) });
    const { contactsUpdate, outreachSet, outreachList, contactsList } =
      await import("../commands/contacts.js");
    await expect(contactsUpdate("partner@fund.vc", {
      name: "Partner",
      clearName: true,
    })).rejects.toThrow("process.exit");
    await expect(outreachSet("partner@fund.vc", {
      stage: "touch2",
      clearStage: true,
    })).rejects.toThrow("process.exit");
    await expect(outreachList({ replied: "maybe" })).rejects.toThrow("process.exit");
    await expect(outreachList({ nextActionBefore: "not-a-date" })).rejects.toThrow("process.exit");
    await expect(contactsList({ limit: "0" })).rejects.toThrow("process.exit");
  });

  it("accepts explicit RFC 3339 offsets on every contact time argument", async () => {
    mockList.mockReturnValue({ toArray: vi.fn(async () => []) });
    mockOutreach.mockReturnValue({ toArray: vi.fn(async () => []) });
    mockSetOutreach.mockResolvedValue({ address: "partner@fund.vc" });
    const { contactsList, outreachList, outreachSet } = await import("../commands/contacts.js");

    // Z, a negative offset, and a positive half-hour offset all name the same
    // kind of unambiguous instant and must reach the SDK as that exact Date.
    await contactsList({
      createdAfter: "2026-07-01T09:00:00-07:00",
      createdBefore: "2026-08-01T12:00:00+05:30",
    });
    expect(mockList).toHaveBeenCalledWith({
      source: undefined,
      importBatchId: undefined,
      createdAfter: new Date("2026-07-01T09:00:00-07:00"),
      createdBefore: new Date("2026-08-01T12:00:00+05:30"),
    });

    await outreachList({
      nextActionBefore: "2026-08-01T09:00:00-07:00",
      lastOutboundBefore: "2026-07-01T12:00:00+05:30",
    });
    expect(mockOutreach).toHaveBeenCalledWith("bot@agents.e2a.dev", {
      stage: undefined,
      replied: undefined,
      suppressed: undefined,
      nextActionBefore: new Date("2026-08-01T09:00:00-07:00"),
      lastOutboundBefore: new Date("2026-07-01T12:00:00+05:30"),
    });

    await outreachSet("partner@fund.vc", { nextAction: "2026-08-01T09:00:00+05:30" });
    expect(mockSetOutreach).toHaveBeenCalledWith(
      "bot@agents.e2a.dev",
      "partner@fund.vc",
      { stage: undefined, metadata: undefined, nextActionAt: new Date("2026-08-01T09:00:00+05:30") },
    );
  });

  it("rejects date-only and offsetless contact timestamps", async () => {
    const { contactsList, outreachList, outreachSet } = await import("../commands/contacts.js");
    // Without an explicit offset the instant is ambiguous: the JS Date
    // constructor reads a bare date-time in LOCAL time and a date-only value
    // as UTC midnight, so the filter would shift with the runner's timezone.
    for (const stamp of ["2026-08-01", "2026-08-01T09:00:00", "2026-02-30T09:00:00Z"]) {
      await expect(contactsList({ createdAfter: stamp })).rejects.toThrow("process.exit");
      await expect(contactsList({ createdBefore: stamp })).rejects.toThrow("process.exit");
      await expect(outreachList({ nextActionBefore: stamp })).rejects.toThrow("process.exit");
      await expect(outreachList({ lastOutboundBefore: stamp })).rejects.toThrow("process.exit");
      await expect(outreachSet("partner@fund.vc", { nextAction: stamp })).rejects.toThrow("process.exit");
    }
    expect(process.stderr.write).toHaveBeenCalledWith(
      expect.stringContaining("RFC 3339"),
    );
    expect(mockList).not.toHaveBeenCalled();
    expect(mockOutreach).not.toHaveBeenCalled();
    expect(mockSetOutreach).not.toHaveBeenCalled();
  });
  // An empty --if-match must never silently become an unconditional write.
  // The server rejects `If-Match:` with no value (400 invalid_request), but
  // before it did, `--if-match "$ETAG"` with ETAG unset performed exactly the
  // unguarded write the flag was there to prevent — and reported success.
  it("rejects an empty --if-match instead of writing unconditionally", async () => {
    const { contactsUpdate, outreachSet } = await import("../commands/contacts.js");

    await expect(contactsUpdate("partner@fund.vc", { name: "X", ifMatch: "" }))
      .rejects.toThrow("process.exit");
    await expect(contactsUpdate("partner@fund.vc", { name: "X", ifMatch: "   " }))
      .rejects.toThrow("process.exit");
    await expect(outreachSet("partner@fund.vc", { stage: "s", ifMatch: "" }))
      .rejects.toThrow("process.exit");

    expect(process.stderr.write).toHaveBeenCalledWith(
      expect.stringContaining("--if-match requires an ETag value"),
    );
    expect(mockUpdate).not.toHaveBeenCalled();
    expect(mockSetOutreach).not.toHaveBeenCalled();
  });

  it("still sends a real --if-match, and omits the option entirely without one", async () => {
    mockUpdate.mockResolvedValue({ address: "partner@fund.vc" });
    mockSetOutreach.mockResolvedValue({ address: "partner@fund.vc" });
    const { contactsUpdate, outreachSet } = await import("../commands/contacts.js");

    await contactsUpdate("partner@fund.vc", { name: "X", ifMatch: '"abc"' });
    expect(mockUpdate).toHaveBeenCalledWith(
      "partner@fund.vc", { displayName: "X", metadata: undefined }, { ifMatch: '"abc"' },
    );

    await contactsUpdate("partner@fund.vc", { name: "Y" });
    expect(mockUpdate).toHaveBeenLastCalledWith(
      "partner@fund.vc", { displayName: "Y", metadata: undefined },
    );

    await outreachSet("partner@fund.vc", { stage: "s", ifMatch: '"def"' });
    expect(mockSetOutreach).toHaveBeenCalledWith(
      "bot@agents.e2a.dev", "partner@fund.vc",
      { stage: "s", metadata: undefined }, { ifMatch: '"def"' },
    );
  });
});
