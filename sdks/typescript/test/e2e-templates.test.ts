/**
 * Live ergonomic coverage: client.templates.*. See test/e2e.test.ts's header
 * for the coverage-gate contract this suite participates in.
 *
 * This file owns: templates.create, templates.get, templates.list,
 * templates.update, templates.delete, templates.validate,
 * templates.listStarters, templates.getStarter.
 */
import { describe, it, expect, beforeAll, afterAll } from "vitest";
import { E2AClient } from "../src/v1/index.js";
import { walkErgonomicSurface } from "./coverage/introspect.js";
import { recordSurface, recordCovered, flushCoverage } from "./coverage/recorder.js";
import { loadLiveEnv, uniqueSlug } from "./coverage/helpers.js";

const env = loadLiveEnv();

describe.skipIf(!env)("ts sdk live e2e: templates", () => {
  let client: E2AClient;

  beforeAll(() => {
    client = new E2AClient({ apiKey: env!.apiKey, baseUrl: env!.baseUrl });
    recordSurface(walkErgonomicSurface(client));
  });

  afterAll(() => {
    flushCoverage();
  });

  it(
    "create → get → list → update → delete lifecycle",
    async () => {
      const alias = uniqueSlug("tmplcov");
      const created = await client.templates.create({
        name: `sdk coverage ${alias}`,
        subject: "Hi {{name}}",
        text: "Hello {{name}}, welcome to {{company}}.",
        alias,
      });
      expect(created.id.startsWith("tmpl_")).toBe(true);
      expect(created.subject).toBe("Hi {{name}}");
      expect(created.alias).toBe(alias);
      recordCovered("templates.create");
      const id = created.id;
      let deleted = false;

      try {
        const got = await client.templates.get(id);
        expect(got.id).toBe(id);
        expect(got.text).toBe("Hello {{name}}, welcome to {{company}}.");
        recordCovered("templates.get");

        const list = await client.templates.list({ limit: 50 }).toArray({ limit: 200 });
        expect(list.some((t) => t.id === id)).toBe(true);
        recordCovered("templates.list");

        const updated = await client.templates.update(id, { name: "sdk coverage renamed", subject: "Hi2 {{name}}" });
        expect(updated.name).toBe("sdk coverage renamed");
        expect(updated.subject).toBe("Hi2 {{name}}");
        expect(updated.text).toBe("Hello {{name}}, welcome to {{company}}.");
        recordCovered("templates.update");

        const del = await client.templates.delete(id);
        expect(del.deleted).toBe(true);
        expect(del.id).toBe(id);
        deleted = true;
        recordCovered("templates.delete");
      } finally {
        if (!deleted) await client.templates.delete(id).catch(() => {});
      }
    },
    20_000,
  );

  it("validate reports valid:true with a rendered preview for valid source", async () => {
    const result = await client.templates.validate({
      subject: "Hi {{name}}",
      text: "Welcome {{name}} to {{company}}",
      testData: { name: "Ada", company: "Acme" },
    });
    expect(result.valid).toBe(true);
    expect(result.errors).toEqual([]);
    expect(result.rendered?.subject).toBe("Hi Ada");
    expect(result.rendered?.text).toBe("Welcome Ada to Acme");
    expect(result.suggestedData).toBeTruthy();
    recordCovered("templates.validate");
  });

  it("listStarters / getStarter surface the deployment's starter catalog", async () => {
    const starters = await client.templates.listStarters({ limit: 50 }).toArray({ limit: 200 });
    expect(starters.length).toBeGreaterThan(0);
    for (const s of starters) {
      expect(s.alias).toBeTruthy();
      expect(s.name).toBeTruthy();
    }
    recordCovered("templates.listStarters");

    const alias = starters[0]!.alias;
    const detail = await client.templates.getStarter(alias);
    expect(detail.alias).toBe(alias);
    expect(typeof detail.text).toBe("string");
    expect(typeof detail.html).toBe("string");
    recordCovered("templates.getStarter");
  });
});
