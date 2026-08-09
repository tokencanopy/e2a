/**
 * Live ergonomic coverage: client.contacts.* — account-level contacts plus the
 * per-agent outreach surface. See test/e2e.test.ts's header for the
 * coverage-gate contract this suite participates in.
 *
 * This file owns: contacts.create, contacts.get, contacts.getWithETag,
 * contacts.update, contacts.list, contacts.import, contacts.deleteImport,
 * contacts.delete, contacts.setOutreach, contacts.getOutreach,
 * contacts.getOutreachWithETag, contacts.outreach, contacts.deleteOutreach.
 *
 * Contact fixtures are unique <slug>@example.com addresses per run, cleaned up
 * in afterAll (a suppression surviving its contact's delete is by design —
 * per-run uniqueness makes that harmless).
 */
import { describe, it, expect, beforeAll, afterAll } from "vitest";
import { E2AClient, E2ANotFoundError } from "../src/v1/index.js";
import { walkErgonomicSurface } from "./coverage/introspect.js";
import { recordSurface, recordCovered, flushCoverage } from "./coverage/recorder.js";
import { loadLiveEnv, uniqueSlug } from "./coverage/helpers.js";

const env = loadLiveEnv();

describe.skipIf(!env)("ts sdk live e2e: contacts", () => {
  let client: E2AClient;
  const cleanup: Array<() => Promise<unknown>> = [];

  const contactAddress = (prefix: string) => `${uniqueSlug(prefix)}@example.com`;
  const trackContact = (address: string) => cleanup.push(() => client.contacts.delete(address));

  beforeAll(() => {
    client = new E2AClient({ apiKey: env!.apiKey, baseUrl: env!.baseUrl });
    recordSurface(walkErgonomicSurface(client));
  });

  afterAll(async () => {
    for (const fn of cleanup.splice(0)) {
      await fn().catch(() => {});
    }
    flushCoverage();
  });

  it("create / get / getWithETag / update round-trip on a fresh contact", async () => {
    const address = contactAddress("ctc");
    trackContact(address);

    const created = await client.contacts.create({
      address: `Coverage Contact <${address}>`,
      displayName: "Coverage Contact",
      metadata: { suite: "contacts" },
    });
    // The address is canonicalized: the display-name form resolves to the bare address.
    expect(created.address).toBe(address);
    expect(created.displayName).toBe("Coverage Contact");
    expect(created.metadata.suite).toBe("contacts");
    recordCovered("contacts.create");

    const got = await client.contacts.get(address);
    expect(got.address).toBe(address);
    expect(got.displayName).toBe("Coverage Contact");
    recordCovered("contacts.get");

    const withETag = await client.contacts.getWithETag(address);
    expect(withETag.data.address).toBe(address);
    expect(typeof withETag.etag).toBe("string");
    expect(withETag.etag!.length).toBeGreaterThan(0);
    recordCovered("contacts.getWithETag");

    const renamed = `coverage renamed ${Date.now()}`;
    const updated = await client.contacts.update(
      address,
      { displayName: renamed },
      { ifMatch: withETag.etag },
    );
    expect(updated.address).toBe(address);
    expect(updated.displayName).toBe(renamed);
    recordCovered("contacts.update");

    // Confirm the rename actually persisted (not just echoed back).
    const reread = await client.contacts.get(address);
    expect(reread.displayName).toBe(renamed);
  });

  it("list paginates at limit 2 and filters by source", async () => {
    const addresses = [contactAddress("lst"), contactAddress("lst"), contactAddress("lst")];
    for (const address of addresses) {
      await client.contacts.create({ address });
      trackContact(address);
    }

    // Three fresh contacts at limit 2 forces at least one cursor hop — walk the
    // pages manually until every fixture address has been seen.
    const seen = new Set<string>();
    let cursor: string | undefined;
    let pages = 0;
    const pager = client.contacts.list({ limit: 2 });
    while (seen.size < addresses.length && pages < 50) {
      const page = await pager.page(cursor);
      pages += 1;
      for (const contact of page.items) {
        if (addresses.includes(contact.address)) seen.add(contact.address);
      }
      if (!page.next_cursor) break;
      cursor = page.next_cursor;
    }
    expect(pages).toBeGreaterThan(1);
    expect(seen.size).toBe(addresses.length);

    // The source filter narrows by provenance: manual creates are source=manual.
    const manual = await client.contacts.list({ source: "manual", limit: 50 }).toArray({ limit: 50 });
    for (const address of addresses) {
      expect(
        manual.some((contact) => contact.address === address),
        `${address} must appear under source=manual`,
      ).toBe(true);
    }
    recordCovered("contacts.list");
  });

  // Carries an explicit timeout: this test does a multi-contact import and then
  // a batch delete that reverses it, and both scale with the batch rather than
  // being a single round trip. It has been running at ~4.9s against staging —
  // inside vitest's 5s default by ~100ms — so ordinary run-to-run variance
  // fails it, which is exactly what happened on v1.7.3. Do not "tidy" this back
  // to the default; it will pass locally and fail the release pipeline.
  it("import records a batch (with an in-batch duplicate) and deleteImport reverses it", async () => {
    const first = contactAddress("imp");
    const second = contactAddress("imp");
    trackContact(first);
    trackContact(second);

    const result = await client.contacts.import({
      contacts: [
        { address: first, displayName: "Import One" },
        { address: second, displayName: "Import Two" },
        { address: first }, // in-batch duplicate of row 0
      ],
    });
    expect(result.batchId).toBeTruthy();
    expect(result.results.length).toBe(3);
    expect(result.created).toBe(2);
    expect(result.skipped).toBe(1);
    expect(result.failed).toBe(0);
    expect(result.results[2].status).toBe("skipped");
    recordCovered("contacts.import");

    const imported = await client.contacts.get(first);
    expect(imported.source).toBe("import");
    expect(imported.importBatchId).toBe(result.batchId);

    const reversed = await client.contacts.deleteImport(result.batchId);
    expect(reversed.deleted).toBe(true);
    expect(reversed.batchId).toBe(result.batchId);
    expect(reversed.contactsDeleted).toBe(2);
    recordCovered("contacts.deleteImport");

    // The reversal removed the contacts — a follow-up read is a typed not-found.
    await expect(client.contacts.get(first)).rejects.toBeInstanceOf(E2ANotFoundError);
  }, 30_000);

  it("delete removes a contact; a follow-up get is a typed not-found", async () => {
    const address = contactAddress("del");
    await client.contacts.create({ address });
    trackContact(address);

    const deleted = await client.contacts.delete(address);
    expect(deleted.deleted).toBe(true);
    expect(deleted.address).toBe(address);
    recordCovered("contacts.delete");

    await expect(client.contacts.get(address)).rejects.toBeInstanceOf(E2ANotFoundError);
  });

  it("setOutreach / getOutreach / getOutreachWithETag / outreach / deleteOutreach round-trip", async () => {
    const agent = `${uniqueSlug("outcov")}@${env!.sharedDomain}`;
    await client.agents.create({ email: agent, name: "coverage outreach" });
    cleanup.push(() => client.agents.delete(agent));

    const address = contactAddress("out");
    await client.contacts.create({ address });
    trackContact(address);

    // First setOutreach enrols: stage + a next-action schedule.
    const stage = uniqueSlug("stage");
    const nextActionAt = new Date(Date.now() + 3_600_000);
    const enrolled = await client.contacts.setOutreach(agent, address, { stage, nextActionAt });
    expect(enrolled.agentEmail).toBe(agent);
    expect(enrolled.address).toBe(address);
    expect(enrolled.stage).toBe(stage);
    expect(enrolled.nextActionAt).toBeTruthy();
    recordCovered("contacts.setOutreach");

    const fetched = await client.contacts.getOutreach(agent, address);
    expect(fetched.agentEmail).toBe(agent);
    expect(fetched.address).toBe(address);
    expect(fetched.stage).toBe(stage);
    recordCovered("contacts.getOutreach");

    const withETag = await client.contacts.getOutreachWithETag(agent, address);
    expect(withETag.data.address).toBe(address);
    expect(withETag.data.stage).toBe(stage);
    expect(typeof withETag.etag).toBe("string");
    expect(withETag.etag!.length).toBeGreaterThan(0);
    recordCovered("contacts.getOutreachWithETag");

    // Guarded read-modify-write: advance the stage with the validator just read;
    // the omitted schedule must survive (setOutreach is partial).
    const advancedStage = `${stage}-advanced`;
    const advanced = await client.contacts.setOutreach(
      agent,
      address,
      { stage: advancedStage },
      { ifMatch: withETag.etag },
    );
    expect(advanced.stage).toBe(advancedStage);
    expect(advanced.nextActionAt).toBeTruthy();

    // The stage-filtered list surfaces the engagement, and the AutoPager yields it.
    const listed = await client.contacts.outreach(agent, { stage: advancedStage, limit: 20 }).toArray({ limit: 20 });
    const mine = listed.find((e) => e.address === address);
    expect(mine, `engagement for ${address} must appear under stage=${advancedStage}`).toBeTruthy();
    expect(mine!.stage).toBe(advancedStage);
    recordCovered("contacts.outreach");

    let yielded: string | undefined;
    for await (const engagement of client.contacts.outreach(agent, { stage: advancedStage })) {
      if (engagement.address === address) {
        yielded = engagement.address;
        break;
      }
    }
    expect(yielded).toBe(address);

    const unenrolled = await client.contacts.deleteOutreach(agent, address);
    expect(unenrolled.deleted).toBe(true);
    expect(unenrolled.address).toBe(address);
    recordCovered("contacts.deleteOutreach");

    // The contact itself survives un-enrolment; only the engagement is gone.
    await expect(client.contacts.getOutreach(agent, address)).rejects.toBeInstanceOf(E2ANotFoundError);
  });
});
