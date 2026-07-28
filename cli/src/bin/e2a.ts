#!/usr/bin/env node

// e2a CLI — the developer convenience surface plus the *scripting* surface.
// Interactive management (domains, webhooks, HITL queues) lives in the MCP
// tools, SDKs, and dashboard. The CLI covers what those can't:
//   - login   : browser auth → ~/.e2a/config.json
//   - listen  : real-time inbound over WebSocket, with --forward to bridge a
//               local HTTP handler (the `stripe listen --forward-to` pattern)
//   - config  : view/update the local config
//   - whoami / send / reply / messages : stateless primitives for shell-script
//     harnesses (skills, hooks, CI) with a documented exit-code contract —
//     see EXIT in ../exit.ts. Held sends (pending_review) exit 3, because an
//     HTTP-successful send that reached nobody must be distinguishable without
//     parsing JSON.
import { login } from "../commands/login.js";
import { config } from "../commands/config.js";
import { listen } from "../commands/listen.js";
import { whoami } from "../commands/whoami.js";
import { doctor } from "../commands/doctor.js";
import { send, reply } from "../commands/send.js";
import { messagesList, messagesGet, messagesLifecycle } from "../commands/messages.js";
import { agentsList, agentsCreate, agentsGet } from "../commands/agents.js";
import { protectionGet, protectionSet } from "../commands/protection.js";
import { keysCreate, keysList, keysDelete } from "../commands/keys.js";
import {
  contactsList, contactsGet, contactsCreate, contactsUpdate, contactsDelete,
  contactsImport, contactsDeleteImport, outreachList, outreachGet, outreachSet,
  outreachDelete,
} from "../commands/contacts.js";
import { EXIT, exitCodeForAPIError } from "../exit.js";
import { E2AError } from "@e2a/sdk/v1";
import { createRequire } from "module";

const require = createRequire(import.meta.url);
const pkg = require("../../package.json") as { version: string };

const USAGE = `e2a — email for AI agents

Scriptable primitives for agent harnesses (send/reply/messages/whoami, with a
stable exit-code contract) plus login and real-time listen. Interactive
management (domains, webhooks, review queues) lives in the MCP tools, the SDKs
(@e2a/sdk, e2a), and the dashboard.

Usage:
  e2a login                         Log in via browser (account-scoped key)
  e2a whoami [--json]               Show key identity: user, scope, bound agent, plan
  e2a doctor [options]              Read-only diagnostics: config, API, agent access,
                                   custom-domain DNS, MCP, webhooks, outbound SMTP.
                                   Never sends mail or changes DNS/webhooks.
        --agent <email>            Inbox to check (or config agent_email)
        --domain <domain>          Check one custom domain (default: all registered)
        --mcp-url <url>            MCP endpoint to probe (default: the hosted
                                   endpoint when using https://e2a.dev, else skipped)
        --json                     Versioned machine-readable report (e2a.doctor/v1)
  e2a agents list                   List owned inboxes (account key)
  e2a agents create <email> [--name <n>]   Create an inbox (account key)
  e2a agents get <email>            Show one inbox
  e2a keys create [--agent <inbox>] [--name <n>]   Mint a key; --agent = bound,
                                   least-privilege (plaintext printed once)
  e2a keys list / delete <id>       Inventory / revoke keys (account key)
  e2a protection get <email>        Show the protection (screening/review) config
  e2a protection set <email>        Flip review posture, only the named knobs
        --outbound-review on|off   off = sends go out unheld (gate=flag, scan=off)
        --inbound-review on|off    off = inbound delivered unheld
        --suppress-notifications on|off   silence or enable hold-review emails
  e2a contacts list [options]        List account contacts
        --source import|manual|inbound   Filter by provenance
        --import-batch <id>          Filter to one upload
        --created-after <ISO>        Added at or after timestamp
        --created-before <ISO>       Added before timestamp
        --limit <n> --json           Bound output; JSON emits NDJSON
  e2a contacts get <address>         Show one contact
  e2a contacts create <address>      Create one contact
        --idempotency-key <key>      Replay a timed-out create safely
  e2a contacts update <address>      Update --name/--clear-name and/or --metadata
        --if-match <etag>            Reject a stale edit
  e2a contacts delete <address>      Delete identity (suppression survives)
  e2a contacts import <csv>          Preview or import an RFC 4180 CSV
        --email-column <name>        Address column (default: email)
        --name-column <name>         Display-name column (default: name if present)
        --agent <email> --stage <s>  Enroll valid rows with an agent
        --on-conflict merge|skip     Existing-contact behavior (default: merge)
        --idempotency-key <key>       Replay the same upload safely after restart
        --dry-run                    Parse and preview without writing
  e2a contacts imports delete <id>   Reverse an import batch
  e2a contacts outreach list         List one agent's outreach
        --agent <email>              Inbox (or config agent_email)
        --stage <s>                  Exact opaque stage
        --replied true|false         Reply-state filter
        --suppressed true|false      Sendability filter
        --next-action-before <ISO>   Due before timestamp
        --last-outbound-before <ISO> Never contacted or stale before timestamp
        --limit <n> --json           Bound output; JSON emits NDJSON
  e2a contacts outreach get <address>
  e2a contacts outreach set <address>
        --stage <s>|--clear-stage --next-action <ISO|clear> --metadata <json>
        --if-match <etag>            Reject stale outreach state
  e2a contacts outreach delete <address>
  e2a send [options]                Send an email as the agent
        --to <email>               Recipient (repeatable)
        --subject <s>              Subject line
        --body <text>              Plain-text body (or --body-file <f>)
        --html-file <f>            HTML body; text fallback derived if no --body
        --attach <file>            Attach a file (repeatable; max 10 files, 10 MB each, 25 MB total)
        --conversation-id <id>     Thread id (alias: --conversation)
        --reply-to <email>         Reply-To header (where replies go; default: the agent)
        --send-at <rfc3339>        Schedule the send for a future RFC 3339 time with an explicit UTC offset;
                                   status=scheduled, sent "not before" then. Direct self-send is unsupported;
                                   cancel by trashing it (restoring before send time re-arms it)
        --idempotency-key <k>      Stable key so a retried invocation can't double-send
        --agent <email>            Sending inbox (or config agent_email / E2A_AGENT_EMAIL)
        --json                     Print the full send result as JSON
  e2a reply <message-id> [options]  Reply in-thread (same body options as send)
  e2a messages list [options]       List messages, oldest first
        --direction <d>            inbound|outbound|all
        --since <ISO>              Messages created AT or after this timestamp
                                   (inclusive — dedup by message id when cursoring)
        --conversation <id>        Filter to one conversation (alias: --conversation-id)
        --read-status <s>          unread|read|all (default all — safe for poll loops)
        --limit <n>                Stop after n messages
        --agent <email>            Inbox to list (or config agent_email)
        --json                     NDJSON instead of TSV (id, header_from, created_at)
  e2a messages get <id> [--text]    Fetch one message; marks it read
        --text                     Print body text only (parsed reply-text preferred)
        --agent <email>            Inbox to read (or config agent_email)
  e2a messages lifecycle <id>       Show observed lifecycle transitions (beta)
        --cursor <cursor>          Continue from a prior page
        --limit <n>                Page size, 1–100
        --agent <email>            Owning inbox (or config agent_email)
        --json                     Print the canonical lifecycle page as JSON
  e2a listen [options]              Stream inbound email over WebSocket
        --agent <email>            Agent inbox to listen on (or config agent_email)
        --forward <url>            POST each message to a local URL (dev webhook proxy)
        --forward-token <token>    Bearer token to send with --forward requests
        --conversation <id>        Only messages in this conversation
        --once                     Exit 0 after the first (matching) message
        --until <ISO>              With --once: deadline; prints TIMEOUT, exits 6
        --text                     With --once: print the message body text only
        --json                     Emit raw JSON notifications
  e2a config [list|get|set]         View config; set only api_key or agent_email

Options:
  --help, -h     Show this help (works after any subcommand too, e.g.
                 e2a send -h — always before any network call)
  --version, -v  Show version

Exit codes (stable scripting contract):
  0  success
  1  transient error (network / 5xx / rate limit) — retry may help
  2  usage error (bad flags or arguments)
  3  send HELD for review (pending_review) — not delivered
  4  bad credentials or wrong key scope
  5  permanent request error (not found / invalid / conflict) — do NOT retry
  6  bounded wait (listen --once --until) expired with no matching message
  7  failed or unrecognized persisted send outcome — do NOT retry; inspect its id
  8  diagnostics (doctor) completed with warnings only
  9  diagnostics (doctor) found a configuration failure — fix config, do NOT retry
`;

function parseArgs(argv: string[]): { command: string; args: string[] } {
  const args = argv.slice(2);
  const command = args[0] || "";
  return { command, args: args.slice(1) };
}

function getFlag(args: string[], flag: string): string | undefined {
  const idx = args.indexOf(flag);
  if (idx === -1) return undefined;
  const value = args[idx + 1];
  // Don't consume another flag as a value
  if (value === undefined || value.startsWith("--")) return undefined;
  return value;
}

function getFlags(args: string[], flag: string): string[] {
  const values: string[] = [];
  for (let i = 0; i < args.length; i++) {
    if (args[i] === flag) {
      const value = args[i + 1];
      if (value !== undefined && !value.startsWith("--")) {
        values.push(value);
      }
    }
  }
  return values;
}

function hasFlag(args: string[], flag: string): boolean {
  return args.includes(flag);
}

// Flags that take no value. Everything else starting with "--" consumes the
// next token, which getPositionals must skip to find bare arguments like a
// message id.
const BOOLEAN_FLAGS = new Set([
  "--json", "--text", "--once", "--dry-run", "--help", "--version",
  "--clear-name", "--clear-stage",
]);

function getPositionals(args: string[], exactCount?: number, usage?: string): string[] {
  const positionals: string[] = [];
  for (let i = 0; i < args.length; i++) {
    const arg = args[i];
    if (arg.startsWith("--")) {
      if (!BOOLEAN_FLAGS.has(arg)) i++; // skip the flag's value
      continue;
    }
    positionals.push(arg);
  }
  if (exactCount !== undefined && positionals.length !== exactCount) {
    process.stderr.write((usage || "invalid number of arguments") + "\n");
    process.exit(EXIT.USAGE);
  }
  return positionals;
}

/**
 * True when `flag` appears as an actual flag — not as the VALUE of a
 * preceding value-taking flag. Without this, `e2a send --body "--help"`
 * would print usage and exit 0 with nothing sent: a silent drop.
 */
function hasBareFlag(args: string[], flag: string): boolean {
  for (let i = 0; i < args.length; i++) {
    if (args[i] !== flag) continue;
    const prev = i > 0 ? args[i - 1] : undefined;
    const prevTakesValue = prev !== undefined && prev.startsWith("--") && !BOOLEAN_FLAGS.has(prev);
    if (!prevTakesValue) return true;
  }
  return false;
}

/**
 * getFlag, but a flag that is present with a missing or flag-like value is a
 * loud usage error instead of a silent "flag not given" — a dropped
 * --conversation-id or --limit must never quietly change what a send or list
 * does. (Values that legitimately start with "--" go via --body-file.)
 */
function getFlagChecked(args: string[], flag: string): string | undefined {
  const value = getFlag(args, flag);
  if (value === undefined && args.includes(flag)) {
    process.stderr.write(`${flag} requires a value\n`);
    process.exit(EXIT.USAGE);
  }
  // An empty string is never a meaningful flag value here, and treating it as
  // "flag not given" downstream silently DROPS filters — `--conversation ""`
  // used to widen a thread poll to the entire mailbox.
  if (value === "") {
    process.stderr.write(`${flag} requires a non-empty value\n`);
    process.exit(EXIT.USAGE);
  }
  return value;
}

/**
 * --conversation-id and --conversation are aliases for the same filter,
 * accepted on send/messages list/listen. FIX 3: precedence used to be
 * INVERTED between commands — send preferred --conversation-id over
 * --conversation, while messages list and listen preferred --conversation
 * over --conversation-id. Passing both with different values therefore
 * threaded a send onto one conversation while the "same" filter on
 * messages list showed another — silent, opposite winners for one flag
 * pair. Identical values are harmless and accepted; different values are
 * an ambiguous invocation the caller must resolve, not a coin flip that
 * lands differently per command.
 */
function getConversationId(args: string[]): string | undefined {
  const id = getFlagChecked(args, "--conversation-id");
  const alias = getFlagChecked(args, "--conversation");
  if (id !== undefined && alias !== undefined && id !== alias) {
    process.stderr.write(
      "--conversation-id and --conversation are aliases for the same flag but were given different values\n",
    );
    process.exit(EXIT.USAGE);
  }
  return id ?? alias;
}

function getFlagsChecked(args: string[], flag: string): string[] {
  const values = getFlags(args, flag);
  const occurrences = args.filter((a) => a === flag).length;
  if (values.length !== occurrences || values.some((v) => v === "")) {
    process.stderr.write(`${flag} requires a non-empty value\n`);
    process.exit(EXIT.USAGE);
  }
  return values;
}

/**
 * Reject flags a command doesn't know, loudly. Without this, a typo'd
 * `--conversation-id` on `messages list` silently widens the query to the
 * whole mailbox with exit 0 — the silent-corruption class the exit-code
 * contract exists to prevent. Also rejects `--flag=value` (unsupported form)
 * with a pointer at the space-separated syntax, and single-dash long-flag
 * typos like `-limit` or `-json` (FIX 1) — those used to fall straight
 * through `!arg.startsWith("--")` unvalidated and get silently dropped by
 * the command along with whatever they meant to set. `-h`/`-v` are
 * intercepted globally before any command's checkFlags runs (see main()),
 * so a bare `-h`/`-v` reaching here is always a genuine typo, not a dropped
 * help/version request.
 */
function checkFlags(args: string[], allowed: string[]): void {
  for (let i = 0; i < args.length; i++) {
    const arg = args[i];
    if (arg.startsWith("--")) {
      if (arg.includes("=")) {
        process.stderr.write(`${arg}: use space-separated values (--flag value), not --flag=value\n`);
        process.exit(EXIT.USAGE);
      }
      if (!allowed.includes(arg)) {
        process.stderr.write(`unknown flag: ${arg} (see e2a --help)\n`);
        process.exit(EXIT.USAGE);
      }
      // Skip the flag's value — including one that itself starts with "-"
      // (e.g. `--subject -weird`) — so it's never re-examined as its own
      // token by the single-dash check below.
      if (!BOOLEAN_FLAGS.has(arg)) i++;
      continue;
    }
    // Reaching here means `arg` is in FLAG POSITION, not consumed above as
    // a value. Positionals in this CLI (message ids, email addresses) never
    // start with "-", so any dash-leading token here can only be a mistyped
    // flag — reject it instead of silently ignoring it.
    if (arg.length > 1 && arg.startsWith("-")) {
      process.stderr.write(`unknown flag: ${arg} (see e2a --help)\n`);
      process.exit(EXIT.USAGE);
    }
  }
}

async function main() {
  const { command, args } = parseArgs(process.argv);

  // FIX 2: `-h`/`-v` must short-circuit on EVERY subcommand, exactly like
  // `--help`/`--version` — before any network call. Previously only the
  // bare command ("e2a -h") and `--help` anywhere in args were checked, so
  // `e2a whoami -h` silently ran a real authenticated API call and
  // `e2a send -h --to …` attempted an actual send.
  if (
    command === "" ||
    command === "help" ||
    command === "--help" ||
    command === "-h" ||
    hasBareFlag(args, "--help") ||
    hasBareFlag(args, "-h")
  ) {
    process.stdout.write(USAGE);
    return;
  }

  if (
    command === "--version" ||
    command === "-v" ||
    hasBareFlag(args, "--version") ||
    hasBareFlag(args, "-v")
  ) {
    process.stdout.write(`e2a ${pkg.version}\n`);
    return;
  }

  switch (command) {
    case "login":
      checkFlags(args, []);
      getPositionals(args, 0, "usage: e2a login");
      await login();
      break;
    case "agents": {
      const sub = args[0];
      const rest = args.slice(1);
      if (sub === "list") {
        checkFlags(rest, ["--json"]);
        getPositionals(rest, 0, "usage: e2a agents list [--json]");
        await agentsList({ json: hasFlag(rest, "--json") });
      } else if (sub === "create") {
        checkFlags(rest, ["--name", "--json"]);
        const [email] = getPositionals(
          rest,
          1,
          "usage: e2a agents create <email> [--name <n>] [--json]",
        );
        await agentsCreate(email, {
          name: getFlagChecked(rest, "--name"),
          json: hasFlag(rest, "--json"),
        });
      } else if (sub === "get") {
        checkFlags(rest, ["--json"]);
        const [email] = getPositionals(
          rest,
          1,
          "usage: e2a agents get <email> [--json]",
        );
        await agentsGet(email, { json: hasFlag(rest, "--json") });
      } else {
        process.stderr.write("Usage: e2a agents [list|create <email>|get <email>]\n");
        process.exit(EXIT.USAGE);
      }
      break;
    }
    case "keys": {
      const sub = args[0];
      const rest = args.slice(1);
      if (sub === "create") {
        checkFlags(rest, ["--name", "--agent", "--json"]);
        getPositionals(
          rest,
          0,
          "usage: e2a keys create [--agent <inbox>] [--name <n>] [--json]",
        );
        await keysCreate({
          name: getFlagChecked(rest, "--name"),
          agent: getFlagChecked(rest, "--agent"),
          json: hasFlag(rest, "--json"),
        });
      } else if (sub === "list") {
        checkFlags(rest, ["--json"]);
        getPositionals(rest, 0, "usage: e2a keys list [--json]");
        await keysList({ json: hasFlag(rest, "--json") });
      } else if (sub === "delete") {
        checkFlags(rest, []);
        await keysDelete(getPositionals(rest, 1, "usage: e2a keys delete <key-id>")[0]);
      } else {
        process.stderr.write("Usage: e2a keys [create [--agent <inbox>]|list|delete <id>]\n");
        process.exit(EXIT.USAGE);
      }
      break;
    }
    case "contacts": {
      const sub = args[0];
      const rest = args.slice(1);
      if (sub === "list") {
        checkFlags(rest, [
          "--source", "--import-batch", "--created-after", "--created-before", "--limit", "--json",
        ]);
        getPositionals(rest, 0, "usage: e2a contacts list [options]");
        await contactsList({
          source: getFlagChecked(rest, "--source"),
          importBatch: getFlagChecked(rest, "--import-batch"),
          createdAfter: getFlagChecked(rest, "--created-after"),
          createdBefore: getFlagChecked(rest, "--created-before"),
          limit: getFlagChecked(rest, "--limit"),
          json: hasFlag(rest, "--json"),
        });
      } else if (sub === "get") {
        checkFlags(rest, ["--json"]);
        const [address] = getPositionals(rest, 1, "usage: e2a contacts get <address> [--json]");
        await contactsGet(address, { json: hasFlag(rest, "--json") });
      } else if (sub === "create") {
        checkFlags(rest, ["--name", "--metadata", "--idempotency-key", "--json"]);
        const [address] = getPositionals(rest, 1, "usage: e2a contacts create <address> [options]");
        await contactsCreate(address, {
          name: getFlagChecked(rest, "--name"),
          metadata: getFlagChecked(rest, "--metadata"),
          idempotencyKey: getFlagChecked(rest, "--idempotency-key"),
          json: hasFlag(rest, "--json"),
        });
      } else if (sub === "update") {
        checkFlags(rest, ["--name", "--clear-name", "--metadata", "--if-match", "--json"]);
        const [address] = getPositionals(rest, 1, "usage: e2a contacts update <address> [options]");
        await contactsUpdate(address, {
          name: getFlagChecked(rest, "--name"),
          clearName: hasFlag(rest, "--clear-name"),
          metadata: getFlagChecked(rest, "--metadata"),
          ifMatch: getFlagChecked(rest, "--if-match"),
          json: hasFlag(rest, "--json"),
        });
      } else if (sub === "delete") {
        checkFlags(rest, ["--json"]);
        const [address] = getPositionals(rest, 1, "usage: e2a contacts delete <address> [--json]");
        await contactsDelete(address, { json: hasFlag(rest, "--json") });
      } else if (sub === "import") {
        checkFlags(rest, [
          "--email-column", "--name-column", "--agent", "--stage",
          "--on-conflict", "--idempotency-key", "--dry-run", "--json",
        ]);
        const [path] = getPositionals(rest, 1, "usage: e2a contacts import <csv> [options]");
        await contactsImport(path, {
          emailColumn: getFlagChecked(rest, "--email-column"),
          nameColumn: getFlagChecked(rest, "--name-column"),
          agent: getFlagChecked(rest, "--agent"),
          stage: getFlagChecked(rest, "--stage"),
          onConflict: getFlagChecked(rest, "--on-conflict"),
          idempotencyKey: getFlagChecked(rest, "--idempotency-key"),
          dryRun: hasFlag(rest, "--dry-run"),
          json: hasFlag(rest, "--json"),
        });
      } else if (sub === "imports" && rest[0] === "delete") {
        const tail = rest.slice(1);
        checkFlags(tail, ["--json"]);
        const [batch] = getPositionals(tail, 1, "usage: e2a contacts imports delete <batch-id>");
        await contactsDeleteImport(batch, { json: hasFlag(tail, "--json") });
      } else if (sub === "outreach") {
        const action = rest[0];
        const tail = rest.slice(1);
        if (action === "list") {
          checkFlags(tail, [
            "--agent", "--stage", "--replied", "--suppressed",
            "--next-action-before", "--last-outbound-before", "--limit", "--json",
          ]);
          getPositionals(tail, 0, "usage: e2a contacts outreach list [options]");
          await outreachList({
            agent: getFlagChecked(tail, "--agent"),
            stage: getFlagChecked(tail, "--stage"),
            replied: getFlagChecked(tail, "--replied"),
            suppressed: getFlagChecked(tail, "--suppressed"),
            nextActionBefore: getFlagChecked(tail, "--next-action-before"),
            lastOutboundBefore: getFlagChecked(tail, "--last-outbound-before"),
            limit: getFlagChecked(tail, "--limit"),
            json: hasFlag(tail, "--json"),
          });
        } else if (action === "get") {
          checkFlags(tail, ["--agent", "--json"]);
          const [address] = getPositionals(tail, 1, "usage: e2a contacts outreach get <address>");
          await outreachGet(address, {
            agent: getFlagChecked(tail, "--agent"),
            json: hasFlag(tail, "--json"),
          });
        } else if (action === "set") {
          checkFlags(tail, [
            "--agent", "--stage", "--clear-stage", "--next-action", "--metadata", "--if-match", "--json",
          ]);
          const [address] = getPositionals(tail, 1, "usage: e2a contacts outreach set <address> [options]");
          await outreachSet(address, {
            agent: getFlagChecked(tail, "--agent"),
            stage: getFlagChecked(tail, "--stage"),
            clearStage: hasFlag(tail, "--clear-stage"),
            nextAction: getFlagChecked(tail, "--next-action"),
            metadata: getFlagChecked(tail, "--metadata"),
            ifMatch: getFlagChecked(tail, "--if-match"),
            json: hasFlag(tail, "--json"),
          });
        } else if (action === "delete") {
          checkFlags(tail, ["--agent", "--json"]);
          const [address] = getPositionals(tail, 1, "usage: e2a contacts outreach delete <address>");
          await outreachDelete(address, {
            agent: getFlagChecked(tail, "--agent"),
            json: hasFlag(tail, "--json"),
          });
        } else {
          process.stderr.write("Usage: e2a contacts outreach [list|get <address>|set <address>|delete <address>]\n");
          process.exit(EXIT.USAGE);
        }
      } else {
        process.stderr.write("Usage: e2a contacts [list|get|create|update|delete|import|imports delete|outreach]\n");
        process.exit(EXIT.USAGE);
      }
      break;
    }
    case "protection": {
      const sub = args[0];
      const rest = args.slice(1);
      if (sub === "get") {
        checkFlags(rest, ["--json"]);
        const [email] = getPositionals(
          rest,
          1,
          "usage: e2a protection get <agent-email> [--json]",
        );
        await protectionGet(email, { json: hasFlag(rest, "--json") });
      } else if (sub === "set") {
        checkFlags(rest, ["--outbound-review", "--inbound-review", "--suppress-notifications", "--json"]);
        const [email] = getPositionals(
          rest,
          1,
          "usage: e2a protection set <agent-email> [options]",
        );
        await protectionSet(email, {
          outboundReview: getFlagChecked(rest, "--outbound-review"),
          inboundReview: getFlagChecked(rest, "--inbound-review"),
          suppressNotifications: getFlagChecked(rest, "--suppress-notifications"),
          json: hasFlag(rest, "--json"),
        });
      } else {
        process.stderr.write(
          "Usage: e2a protection [get <email>|set <email> [--outbound-review on|off] [--inbound-review on|off] [--suppress-notifications on|off]]\n",
        );
        process.exit(EXIT.USAGE);
      }
      break;
    }
    case "whoami":
      checkFlags(args, ["--json"]);
      getPositionals(args, 0, "usage: e2a whoami [--json]");
      await whoami({ json: hasFlag(args, "--json") });
      break;
    case "doctor":
      checkFlags(args, ["--agent", "--domain", "--mcp-url", "--json"]);
      getPositionals(args, 0, "usage: e2a doctor [options]");
      await doctor({
        agent: getFlagChecked(args, "--agent"),
        domain: getFlagChecked(args, "--domain"),
        mcpUrl: getFlagChecked(args, "--mcp-url"),
        json: hasFlag(args, "--json"),
      });
      break;
    case "send":
      checkFlags(args, [
        "--to", "--subject", "--body", "--body-file", "--html-file", "--attach",
        "--conversation-id", "--conversation", "--reply-to", "--send-at", "--agent", "--idempotency-key", "--json",
      ]);
      getPositionals(args, 0, "usage: e2a send [options]");
      await send({
        to: getFlagsChecked(args, "--to"),
        attach: getFlagsChecked(args, "--attach"),
        subject: getFlagChecked(args, "--subject"),
        body: getFlagChecked(args, "--body"),
        bodyFile: getFlagChecked(args, "--body-file"),
        htmlFile: getFlagChecked(args, "--html-file"),
        // --conversation accepted as an alias so send and messages list can't
        // trip each other's spelling. Precedence (and conflicting-value
        // rejection) is shared via getConversationId — see FIX 3.
        conversationId: getConversationId(args),
        replyTo: getFlagChecked(args, "--reply-to"),
        sendAt: getFlagChecked(args, "--send-at"),
        agent: getFlagChecked(args, "--agent"),
        idempotencyKey: getFlagChecked(args, "--idempotency-key"),
        json: hasFlag(args, "--json"),
      });
      break;
    case "reply":
      checkFlags(args, [
        "--body", "--body-file", "--html-file", "--attach", "--reply-to", "--send-at", "--agent", "--idempotency-key", "--json",
      ]);
      await reply(getPositionals(args, 1, "usage: e2a reply <message-id> [options]")[0], {
        attach: getFlagsChecked(args, "--attach"),
        body: getFlagChecked(args, "--body"),
        bodyFile: getFlagChecked(args, "--body-file"),
        htmlFile: getFlagChecked(args, "--html-file"),
        replyTo: getFlagChecked(args, "--reply-to"),
        sendAt: getFlagChecked(args, "--send-at"),
        agent: getFlagChecked(args, "--agent"),
        idempotencyKey: getFlagChecked(args, "--idempotency-key"),
        json: hasFlag(args, "--json"),
      });
      break;
    case "messages": {
      const sub = args[0];
      const rest = args.slice(1);
      if (sub === "list") {
        checkFlags(rest, [
          "--direction", "--since", "--conversation", "--conversation-id",
          "--read-status", "--limit", "--agent", "--json",
        ]);
        getPositionals(rest, 0, "usage: e2a messages list [options]");
        await messagesList({
          agent: getFlagChecked(rest, "--agent"),
          direction: getFlagChecked(rest, "--direction"),
          since: getFlagChecked(rest, "--since"),
          conversation: getConversationId(rest),
          readStatus: getFlagChecked(rest, "--read-status"),
          limit: getFlagChecked(rest, "--limit"),
          json: hasFlag(rest, "--json"),
        });
      } else if (sub === "get") {
        checkFlags(rest, ["--text", "--json", "--agent"]);
        await messagesGet(getPositionals(rest, 1, "usage: e2a messages get <id> [options]")[0], {
          agent: getFlagChecked(rest, "--agent"),
          text: hasFlag(rest, "--text"),
          json: hasFlag(rest, "--json"),
        });
      } else if (sub === "lifecycle") {
        checkFlags(rest, ["--cursor", "--limit", "--json", "--agent"]);
        await messagesLifecycle(
          getPositionals(rest, 1, "usage: e2a messages lifecycle <id> [options]")[0],
          {
            agent: getFlagChecked(rest, "--agent"),
            cursor: getFlagChecked(rest, "--cursor"),
            limit: getFlagChecked(rest, "--limit"),
            json: hasFlag(rest, "--json"),
          },
        );
      } else {
        process.stderr.write("Usage: e2a messages [list|get <id>|lifecycle <id>]\n");
        process.exit(EXIT.USAGE);
      }
      break;
    }
    case "listen":
      checkFlags(args, [
        "--agent", "--forward", "--forward-token", "--json",
        "--conversation", "--conversation-id", "--once", "--until", "--text",
      ]);
      getPositionals(args, 0, "usage: e2a listen [options]");
      await listen({
        agent: getFlagChecked(args, "--agent"),
        json: hasFlag(args, "--json"),
        // Checked variants: `listen --forward` with a missing value used to
        // silently listen WITHOUT forwarding — the silent-drop class again.
        forward: getFlagChecked(args, "--forward"),
        forwardToken: getFlagChecked(args, "--forward-token"),
        conversation: getConversationId(args),
        once: hasFlag(args, "--once"),
        until: getFlagChecked(args, "--until"),
        text: hasFlag(args, "--text"),
      });
      break;
    case "config":
      await config(args);
      break;
    default:
      process.stderr.write(`Unknown command: ${command}\n`);
      process.stderr.write(USAGE);
      process.exit(EXIT.USAGE);
  }
}

// Only skip main when imported for tests (vitest sets VITEST_WORKER_ID)
const isTestImport = typeof process !== "undefined" && !!process.env.VITEST_WORKER_ID;

if (!isTestImport) {
  main().catch((err) => {
    // Print the API error code when present so scripts can grep it even
    // without branching on exit codes.
    const code = err instanceof E2AError && err.code ? ` [${err.code}]` : "";
    process.stderr.write(`Error: ${err.message}${code}\n`);
    // Contract mapping: AUTH (4) = fix your key; REQUEST (5) = permanent
    // request error (404/409/422 — the SDK marks these non-retryable), do NOT
    // retry the identical invocation; ERROR (1) = transient, retry may help.
    process.exit(err instanceof E2AError ? exitCodeForAPIError(err) : EXIT.ERROR);
  });
}

export {
  getFlag,
  getFlags,
  hasFlag,
  parseArgs,
  getPositionals,
  hasBareFlag,
  checkFlags,
  getConversationId,
};
