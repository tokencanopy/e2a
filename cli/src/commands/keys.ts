import type { CreateAPIKeyRequest } from "@e2a/sdk/v1";
import { hostname } from "node:os";
import { createClient } from "../sdk.js";
import { loadConfig, expandBareAddress } from "../config.js";
import { EXIT, fail } from "../exit.js";
import { sanitizeTsvField } from "./messages.js";

export interface KeysCreateOptions {
  name?: string;
  /** Bind the key to this inbox (scope=agent). Omit for an account-scoped key. */
  agent?: string;
  json?: boolean;
}

export interface KeysListOptions {
  json?: boolean;
}

const DELETE_USAGE = "usage: e2a keys delete <key-id>";

// Account-scope only. `create --agent` is the non-interactive path to a
// least-privilege agent key — what a harness bootstrap (tether setup) runs
// when the account credential already exists and no browser is available.
export async function keysCreate(opts: KeysCreateOptions): Promise<void> {
  const config = loadConfig();
  const client = createClient({}, config);
  // Bare agent names expand on the shared domain: `--agent myname` means
  // myname@<shared_domain>, discovered live via GET /v1/info when not
  // already known — matching the expansion in agents.ts (expandBareAddress).
  const agentEmail = opts.agent ? await expandBareAddress(opts.agent, config) : undefined;
  const created = await client.account.apiKeys.create({
    name: opts.name || (opts.agent ? `agent key @${hostname()}` : `account key @${hostname()}`),
    ...(agentEmail ? { scope: "agent" as CreateAPIKeyRequest["scope"], agentEmail } : {}),
  });

  if (opts.json) {
    process.stdout.write(JSON.stringify(created) + "\n");
    return;
  }
  // Plaintext key alone on stdout (script-friendly: KEY=$(e2a keys create …));
  // the shown-once warning goes to stderr so it can't pollute the capture.
  process.stdout.write(created.key + "\n");
  // Report the EXPANDED address, not the raw slug: `--agent mybot` binds the
  // key to mybot@<shared_domain>, and echoing "mybot" would misreport what was
  // actually created.
  process.stderr.write(
    `Key ${created.id} created (${agentEmail ? `agent-scoped: ${agentEmail}` : "account-scoped"}). Shown once — store it now.\n`,
  );
}

export async function keysList(opts: KeysListOptions): Promise<void> {
  const client = createClient();
  for await (const k of client.account.apiKeys.list()) {
    if (opts.json) {
      process.stdout.write(JSON.stringify(k) + "\n");
    } else {
      process.stdout.write(
        `${k.id}\t${k.keyPrefix}\t${k.scope}${k.agentEmail ? `\t${k.agentEmail}` : "\t"}\t${sanitizeTsvField(k.name || "")}\n`,
      );
    }
  }
}

export async function keysDelete(id: string | undefined): Promise<void> {
  if (!id) fail(EXIT.USAGE, DELETE_USAGE);
  const client = createClient();
  // The API confirms deletes with a 200 + deletion object ({deleted, id});
  // echo the server's confirmation rather than the caller's input.
  const res = await client.account.apiKeys.delete(id);
  process.stdout.write(`revoked ${res.id}\n`);
}
