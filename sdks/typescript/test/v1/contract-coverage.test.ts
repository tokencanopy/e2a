/**
 * Per-PR live-contract coverage ratchet.
 *
 * The staging conformance suite is the only place most SDK methods meet a
 * real server — and that gate runs post-merge, per release. That is how the
 * `If-Match: undefined` bug (fixed in #774) shipped: the method existed, unit
 * CI was green, and the first LIVE exercise of the path was staging run
 * 30612956986. This gate gives the per-PR ts-contract job a coverage
 * DENOMINATOR: every method on the ergonomic client must either be exercised
 * by contract-client.test.ts (against cmd/e2a-contract-server) or carry an
 * explicit entry in NOT_CONTRACT_TESTED below. A NEW SDK method therefore
 * cannot land silently untested-against-a-live-server — adding it forces
 * either a contract test or a visible, reviewable allowlist entry.
 *
 * Mechanics (deliberately cheap):
 *  - Denominator: runtime introspection of the built client
 *    (test/coverage/introspect.ts — the same walker the staging-side
 *    coverage gate uses), so hand-listing can't drift.
 *  - Numerator: static call sites in contract-client.test.ts —
 *    `client.<path>.<method>(...)` chains, comments stripped. Static scan of
 *    the suite's source, not runtime recording, so it needs no server and no
 *    ordering guarantees; the flip side is that only DIRECT chains off
 *    `client` are seen. Write contract tests that way (the whole file already
 *    does), or the method counts as uncovered.
 *  - The allowlist is pruned both ways: an entry whose method disappears, or
 *    whose method gains a contract test, fails the gate until removed.
 *
 * This is a RATCHET, not a proof: a seeded entry means "known hole, decided
 * at gate introduction" (full inventory below), and coverage claims nothing
 * about assertion quality. Shrink the list by moving methods into
 * contract-client.test.ts; never grow it without a reason that names why the
 * contract server cannot exercise the method.
 */
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, it, expect } from "vitest";
import { E2AClient } from "../../src/v1/client.js";
import { walkErgonomicSurface } from "../coverage/introspect.js";

const HERE = dirname(fileURLToPath(import.meta.url));

/**
 * Methods with no live per-PR contract test, each with the reason. Two
 * legitimate kinds of entry:
 *  - "contract server can't": the path needs infrastructure the shared test
 *    server does not run (real SMTP egress/ingress, SES feedback, external
 *    webhook targets, WebSocket lifecycles under vitest).
 *  - "seeded": covered today only by the post-merge staging conformance
 *    suite; carried over as-is when this gate was introduced. Removing seeds
 *    by writing contract tests is welcome — the gate fails if an entry goes
 *    stale, so the list can only shrink truthfully.
 */
const SEEDED =
  "seeded at gate introduction — exercised only by the post-merge staging " +
  "conformance suite (tests/e2e-prod + the SDK live-coverage gate); add a " +
  "contract-client test to shrink this list";

const NOT_CONTRACT_TESTED: Record<string, string> = {
  // -- paths the contract server genuinely cannot exercise ------------------
  "account.delete":
    "irreversibly cascades the ONE shared contract-server account every suite " +
    "in this vitest run depends on; mirrors the live gate's identical entry",
  "account.suppressions.delete":
    "an account-level suppression is only created by real SES bounce/complaint " +
    "feedback, which the contract server does not run — nothing can exist to delete",
  "domains.verify":
    "verification checks real DNS records; a contract-server domain can never " +
    "have them, so only the error path would be reachable",
  "inbound.fromEvent":
    "local webhook-payload adapter — no HTTP round trip, so there is no live " +
    "contract to test; covered by offline inbound.test.ts fixtures",

  // -- seeded inventory (known holes, decided when the gate landed) ---------
  "account.apiKeys.list": SEEDED,
  "account.export": SEEDED,
  "account.get": SEEDED,
  "account.suppressions.list": SEEDED,
  "agents.createSuppression": SEEDED,
  "agents.deleteSuppression": SEEDED,
  "agents.getProtection": SEEDED,
  "agents.list": SEEDED,
  "agents.listSuppressions": SEEDED,
  "agents.replaceProtection": SEEDED,
  "agents.restore": SEEDED,
  "agents.test": SEEDED,
  "agents.update": SEEDED,
  "contacts.deleteImport": SEEDED,
  "contacts.deleteOutreach": SEEDED,
  "contacts.get": SEEDED,
  "contacts.getOutreach": SEEDED,
  "contacts.getOutreachWithETag": SEEDED,
  "contacts.import": SEEDED,
  "contacts.list": SEEDED,
  "contacts.outreach": SEEDED,
  "conversations.get": SEEDED,
  "conversations.list": SEEDED,
  "domains.create": SEEDED,
  "domains.delete": SEEDED,
  "domains.get": SEEDED,
  "domains.list": SEEDED,
  "events.get": SEEDED,
  "events.list": SEEDED,
  "events.redeliver": SEEDED,
  "info": SEEDED,
  "listen": SEEDED,
  "messages.delete": SEEDED,
  "messages.forward": SEEDED,
  "messages.get": SEEDED,
  "messages.getAttachment": SEEDED,
  "messages.getLifecycle": SEEDED,
  "messages.list": SEEDED,
  "messages.reply": SEEDED,
  "messages.restore": SEEDED,
  "messages.updateLabels": SEEDED,
  "reviews.approve": SEEDED,
  "reviews.get": SEEDED,
  "reviews.list": SEEDED,
  "reviews.reject": SEEDED,
  "templates.create": SEEDED,
  "templates.delete": SEEDED,
  "templates.get": SEEDED,
  "templates.getStarter": SEEDED,
  "templates.list": SEEDED,
  "templates.listStarters": SEEDED,
  "templates.update": SEEDED,
  "templates.validate": SEEDED,
  "webhooks.create": SEEDED,
  "webhooks.delete": SEEDED,
  "webhooks.deliveries": SEEDED,
  "webhooks.fetchMessage": SEEDED,
  "webhooks.get": SEEDED,
  "webhooks.list": SEEDED,
  "webhooks.rotateSecret": SEEDED,
  "webhooks.test": SEEDED,
  "webhooks.update": SEEDED,
};

function coveredMethodIds(source: string): Set<string> {
  const stripped = source
    .replace(/\/\*[\s\S]*?\*\//g, "")
    .replace(/^\s*\/\/.*$/gm, "");
  const covered = new Set<string>();
  for (const match of stripped.matchAll(/\bclient((?:\.\w+)+)\(/g)) {
    covered.add(match[1].slice(1));
  }
  return covered;
}

describe("live-contract coverage ratchet", () => {
  // Pure reflection over the built client — no network, runs even when the
  // contract-server env vars are absent (the check is static either way).
  const surface = walkErgonomicSurface(
    new E2AClient({ apiKey: "unused", baseUrl: "http://contract.invalid" }),
  );
  const covered = coveredMethodIds(
    readFileSync(join(HERE, "contract-client.test.ts"), "utf8"),
  );

  it("every ergonomic method has a contract-client test or an explicit allowlist entry", () => {
    const missing = surface.filter(
      (id) => !covered.has(id) && !(id in NOT_CONTRACT_TESTED),
    );
    expect(
      missing,
      "these client methods have no live per-PR contract coverage — add a test " +
        "to contract-client.test.ts (a direct `client.x.y(...)` call with result " +
        "assertions) or an explicit NOT_CONTRACT_TESTED entry with the reason",
    ).toEqual([]);
  });

  it("the allowlist stays the exact inventory of uncovered methods", () => {
    const phantom = Object.keys(NOT_CONTRACT_TESTED).filter(
      (id) => !surface.includes(id),
    );
    expect(
      phantom,
      "allowlist entries for methods the client no longer exposes — remove them",
    ).toEqual([]);

    const stale = Object.keys(NOT_CONTRACT_TESTED).filter((id) =>
      covered.has(id),
    );
    expect(
      stale,
      "allowlisted methods now covered by contract-client.test.ts — remove the entries",
    ).toEqual([]);
  });

  it("the scan itself is non-vacuous", () => {
    // If the walker or the call-site regex drifts, fail loudly instead of
    // passing over an empty denominator/numerator.
    expect(surface.length).toBeGreaterThan(30);
    expect(covered.has("agents.create")).toBe(true);
    expect(covered.has("contacts.setOutreach")).toBe(true);
  });
});
