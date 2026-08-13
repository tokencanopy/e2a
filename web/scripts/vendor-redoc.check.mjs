import assert from "node:assert/strict";
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { pathToFileURL } from "node:url";
import { afterEach, test } from "node:test";

import { EXPECTED_BYTES, SOURCE, update } from "./vendor-redoc.mjs";

const roots = [];

afterEach(async () => {
  await Promise.all(roots.splice(0).map((root) => rm(root, { recursive: true })));
});

async function sentinelTarget() {
  const root = await mkdtemp(join(tmpdir(), "e2a-redoc-"));
  roots.push(root);
  const target = pathToFileURL(join(root, "redoc.js"));
  await writeFile(target, "existing reviewed artifact");
  return target;
}

test("update pins the fixed request and preserves an existing target when validation rejects", async () => {
  const failures = [
    {
      name: "a non-200 response",
      response: { status: 404, arrayBuffer: async () => new ArrayBuffer(0) },
      error: /HTTP 404/,
    },
    {
      name: "a wrong-size response",
      response: { status: 200, arrayBuffer: async () => new ArrayBuffer(1) },
      error: /has 1 bytes; expected 910994/,
    },
    {
      name: "a wrong-hash response",
      response: {
        status: 200,
        arrayBuffer: async () => new ArrayBuffer(EXPECTED_BYTES),
      },
      error: /SHA-256 does not match/,
    },
  ];

  for (const failure of failures) {
    const target = await sentinelTarget();
    const requests = [];
    const fetchImpl = async (...request) => {
      requests.push(request);
      return failure.response;
    };

    await assert.rejects(update({ fetchImpl, target }), failure.error, failure.name);
    assert.deepEqual(requests, [[SOURCE, { redirect: "error" }]], failure.name);
    assert.equal(
      await readFile(target, "utf8"),
      "existing reviewed artifact",
      failure.name,
    );
  }
});
