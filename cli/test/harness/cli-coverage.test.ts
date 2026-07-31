/**
 * Pure unit test for parseHelpCommands — no live gating, no creds, no
 * binary spawn. Lives under test/** so it runs with vitest.e2e.config.ts
 * (offline-safe: passes with or without staging env); the default unit
 * config (src/**) never picks it up.
 */
import { describe, it, expect } from "vitest";
import { parseHelpCommands } from "./cli-coverage.js";

// Mirrors the real USAGE layout (cli/src/bin/e2a.ts): genuine top-level
// command lines are indented by exactly two spaces; option/continuation
// lines sit deeper, and prose mentions of `e2a` never lead a line.
const SAMPLE_HELP = [
  "e2a — email for AI agents",
  "",
  "Usage:",
  "  e2a login                         Log in via browser (account-scoped key)",
  "  e2a whoami [--json]               Show key identity: user, scope, bound agent, plan",
  "  e2a agents list                   List owned inboxes (account key)",
  "  e2a agents create <email> [--name <n>]   Create an inbox (account key)",
  "  e2a agents get <email>            Show one inbox",
  "  e2a contacts list [options]        List account contacts",
  "        --source import|manual|inbound   Filter by provenance",
  "  e2a contacts get <address>         Show one contact",
  "  e2a contacts create <address>      Create one contact",
  "  e2a contacts update <address>      Update --name/--clear-name and/or --metadata",
  "  e2a contacts delete <address>      Delete identity (suppression survives)",
  "  e2a contacts import <csv>          Preview or import an RFC 4180 CSV",
  "        --dry-run                    Parse and preview without writing",
  "  e2a contacts imports delete <id>   Reverse an import batch",
  "  e2a contacts outreach list         List one agent's outreach",
  "  e2a contacts outreach get <address>",
  "  e2a contacts outreach set <address>",
  "        --stage <s>|--clear-stage --next-action <ISO|clear> --metadata <json>",
  "  e2a contacts outreach delete <address>",
  "  e2a send [options]                Send an email as the agent",
  "  e2a reply <message-id> [options]  Reply in-thread (same body options as send)",
  "        --agent <email>            Sending inbox (or config agent_email)",
  "",
  "Options:",
  "  -h, --help                        Show help",
  "",
  "Exit codes:",
  "  0 ok · 1 transient · 2 usage — run e2a send -h for per-command help",
  "",
].join("\n");

describe("parseHelpCommands", () => {
  it("extracts the deduped top-level catalog — contacts included, no subcommand leakage", () => {
    // `contacts` appears on 11 lines (list/get/create/update/delete/import/
    // imports delete/outreach ×4); it must collapse to ONE token, and the
    // subcommand words (list, outreach, imports, …) must never surface.
    expect(parseHelpCommands(SAMPLE_HELP)).toEqual([
      "agents",
      "contacts",
      "login",
      "reply",
      "send",
      "whoami",
    ]);
  });

  it("ignores lines not indented by exactly two spaces", () => {
    const text = [
      "e2a nosep <id>              no indent",
      "    e2a deep <id>           four-space indent",
      "        e2a option <id>     eight-space indent",
      "  e2a real <id>             genuine entry",
    ].join("\n");
    expect(parseHelpCommands(text)).toEqual(["real"]);
  });

  it("returns an empty catalog when no command lines are present", () => {
    expect(parseHelpCommands("Usage: e2a <command> [options]\n")).toEqual([]);
  });
});
