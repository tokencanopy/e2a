import { createClient } from "../sdk.js";
import { loadConfig, expandBareAddress } from "../config.js";
import { EXIT, fail } from "../exit.js";
import { sanitizeTsvField } from "./messages.js";

export interface AgentsListOptions {
  json?: boolean;
}

export interface AgentsCreateOptions {
  name?: string;
  json?: boolean;
}

export interface AgentsGetOptions {
  json?: boolean;
}

const CREATE_USAGE = "usage: e2a agents create <email> [--name <n>] [--json]";
const GET_USAGE = "usage: e2a agents get <email> [--json]";

// Account-scope only (agent-scoped keys exit 4 with the server's message).
// `list` closes the discovery gap: before it, nothing on the CLI could answer
// "which inboxes do I own?" after login.
export async function agentsList(opts: AgentsListOptions): Promise<void> {
  const client = createClient();
  for await (const agent of client.agents.list()) {
    if (opts.json) {
      process.stdout.write(JSON.stringify(agent) + "\n");
    } else {
      process.stdout.write(
        `${agent.email}\t${sanitizeTsvField(agent.name || "")}\t${agent.domainVerified ? "verified" : "unverified"}\n`,
      );
    }
  }
}

export async function agentsCreate(
  email: string | undefined,
  opts: AgentsCreateOptions,
): Promise<void> {
  if (!email) fail(EXIT.USAGE, CREATE_USAGE);
  // Bare names expand on the shared domain: `create myname` means
  // myname@<shared_domain>, discovered live via GET /v1/info when not
  // already known (see expandBareAddress). Both `agents create` and `keys
  // create --agent` apply this expansion, so `agents create mybot` and
  // `keys create --agent mybot` use the same address.
  const config = loadConfig();
  const address = await expandBareAddress(email, config);

  const client = createClient({}, config);
  const agent = await client.agents.create({ email: address, name: opts.name });
  if (opts.json) {
    process.stdout.write(JSON.stringify(agent) + "\n");
  } else {
    process.stdout.write(agent.email + "\n");
  }
}

export async function agentsGet(email: string | undefined, opts: AgentsGetOptions): Promise<void> {
  if (!email) fail(EXIT.USAGE, GET_USAGE);

  const client = createClient();
  const agent = await client.agents.get(email);
  if (opts.json) {
    process.stdout.write(JSON.stringify(agent) + "\n");
    return;
  }
  process.stdout.write(`email:    ${agent.email}\n`);
  if (agent.name) process.stdout.write(`name:     ${agent.name}\n`);
  process.stdout.write(`domain:   ${agent.domain} (${agent.domainVerified ? "verified" : "unverified"})\n`);
  process.stdout.write(`created:  ${agent.createdAt.toISOString()}\n`);
}
