// Unit test for the coverage-gate denominator walker, test/coverage/introspect.ts.
// Runs offline in the default `npm test` (test:unit globs test/v1): the
// E2AClient constructor only wires generated transport handles — it performs no
// I/O — so walking a dummy-configured client is pure reflection.
//
// This is the "missing coverage demonstrably fails" regression net: the walker
// must SEE every ergonomic method (contacts.* included), because whatever it
// advertises becomes the denominator gate.mjs demands coverage for.
import { describe, it, expect } from "vitest";
import { E2AClient } from "../../src/v1/index.js";
import { walkErgonomicSurface } from "../coverage/introspect.js";

const CONTACTS_IDS = [
  "contacts.create",
  "contacts.get",
  "contacts.getWithETag",
  "contacts.update",
  "contacts.list",
  "contacts.import",
  "contacts.deleteImport",
  "contacts.delete",
  "contacts.setOutreach",
  "contacts.getOutreach",
  "contacts.getOutreachWithETag",
  "contacts.outreach",
  "contacts.deleteOutreach",
];

describe("walkErgonomicSurface", () => {
  const client = new E2AClient({ apiKey: "test-key", baseUrl: "http://localhost" });
  const ids = walkErgonomicSurface(client);

  it("advertises the full contacts.* surface (so the gate demands coverage for it)", () => {
    for (const id of CONTACTS_IDS) {
      expect(ids, `walker must advertise ${id}`).toContain(id);
    }
  });

  it("advertises known pre-existing ergonomic ids", () => {
    for (const id of ["info", "agents.create", "messages.send", "account.apiKeys.create"]) {
      expect(ids, `walker must advertise ${id}`).toContain(id);
    }
  });

  it("never double-counts (aliased resources resolve to the same instance)", () => {
    // client.messages / client.inbound's facade / client.webhooks share one
    // MessagesResource instance; the visited-set must keep ids unique.
    expect(new Set(ids).size).toBe(ids.length);
  });

  it("excludes the generated transport layer (Promise<Tag>Api internals)", () => {
    // client.meta (a raw PromiseMetaApi) and every resource's private `.api`
    // handle are the generated layer — internal by construction, excluded from
    // the denominator rather than allowlisted. Nothing reachable only through
    // them (e.g. getInfo, *WithHttpInfo) may surface.
    expect(ids.some((id) => id === "meta" || id.startsWith("meta."))).toBe(false);
    expect(ids.some((id) => id.split(".").some((seg) => /^Promise[A-Za-z0-9]*Api$/.test(seg)))).toBe(false);
    expect(ids.some((id) => id === "api" || id.includes(".api."))).toBe(false);
    expect(ids.some((id) => id.endsWith("WithHttpInfo"))).toBe(false);
  });
});
