import { createClient } from "../sdk.js";
import { EXIT, fail } from "../exit.js";
import { sanitizeTsvField } from "./messages.js";

// Suppression management, two scopes routed on --agent:
//   - without --agent: the ACCOUNT-wide list (auto-populated by hard bounces
//     and complaints; enforced for every agent at send time).
//   - with --agent:    the beta per-agent list (unsubscribe/manual blocks for
//     one exact sending agent).
// `add` is agent-only by design — the API has no account-level create.

export interface OutputOptions { json?: boolean }

// The SDK interpolates the address as a URL path segment, and
// `encodeURIComponent` does NOT encode "." — so an all-dots address survives
// encoding and the URL parser then removes it as a relative path segment
// BEFORE the request is sent, collapsing DELETE .../suppressions/.. onto the
// parent resource (/v1/account or /v1/agents/{email}) — both real DELETE
// endpoints that accept the very same ?confirm=DELETE token. Reject it here;
// no valid email address is all dots anyway.
function checkedAddress(raw: string): string {
  const address = raw.trim();
  if (!address || /^\.+$/.test(address)) {
    fail(EXIT.USAGE, `"${raw}" is not a valid recipient address`);
  }
  return address;
}

function positiveLimit(raw: string | undefined): number {
  if (raw === undefined) return 100;
  const value = Number(raw);
  if (!Number.isInteger(value) || value < 1 || value > 10_000) {
    fail(EXIT.USAGE, "--limit must be an integer from 1 to 10000");
  }
  return value;
}

export async function suppressionsList(opts: {
  agent?: string;
  limit?: string;
  json?: boolean;
}): Promise<void> {
  const limit = positiveLimit(opts.limit);
  const client = createClient();
  if (opts.agent) {
    const rows = await client.agents.listSuppressions(opts.agent, {}).toArray({ limit });
    for (const row of rows) {
      if (opts.json) process.stdout.write(JSON.stringify(row) + "\n");
      else process.stdout.write(
        `${row.address}\t${row.source}\t${sanitizeTsvField(row.reason ?? "")}\t${row.createdAt.toISOString()}\n`,
      );
    }
    return;
  }
  const rows = await client.account.suppressions.list({}).toArray({ limit });
  for (const row of rows) {
    if (opts.json) process.stdout.write(JSON.stringify(row) + "\n");
    else process.stdout.write(
      `${row.address}\t${row.source}\t${sanitizeTsvField(row.reason ?? "")}\t${row.createdAt.toISOString()}\n`,
    );
  }
}

export async function suppressionsAdd(address: string, opts: {
  agent?: string;
  reason?: string;
  json?: boolean;
}): Promise<void> {
  if (!opts.agent) {
    fail(EXIT.USAGE, "suppressions add requires --agent <email>: manual blocks are per-agent (there is no account-level create; account entries come from bounces/complaints)");
  }
  const row = await createClient().agents.createSuppression(opts.agent, {
    address: checkedAddress(address),
    ...(opts.reason !== undefined ? { reason: opts.reason } : {}),
  });
  process.stdout.write(opts.json ? JSON.stringify(row) + "\n" : `suppressed ${row.address} for ${row.agentEmail}\n`);
}

export async function suppressionsRemove(address: string, opts: {
  agent?: string;
  json?: boolean;
}): Promise<void> {
  const checked = checkedAddress(address);
  const client = createClient();
  const result = opts.agent
    ? await client.agents.deleteSuppression(opts.agent, checked)
    : await client.account.suppressions.delete(checked);
  process.stdout.write(opts.json ? JSON.stringify(result) + "\n" : `deleted ${result.address}\n`);
}
