import type { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import type { McpClient } from "../client.js";
import { z } from "zod";
import { runTool, strictInputSchema, paginationInput } from "./util.js";

/**
 * Domain-management tools. Mirrors the four domain endpoints the
 * SDK exposes. `register_domain` is the headline composition point:
 * it returns the MX + TXT records the user must publish, and the
 * docstring tells the model to chain into a DNS-provider MCP
 * (Cloudflare, Route 53, …) to actually create those records — then
 * loop back to `verify_domain` once propagation completes.
 *
 * `delete_domain` mirrors `delete_agent`: a Zod `literal(true)`
 * confirm gate so the schema validator catches LLM hallucinations
 * before any HTTP call runs.
 */
export function registerDomainTools(server: McpServer, client: McpClient): void {
  server.registerTool(
    "list_domains",
    {
      title: "List domains",
      annotations: { readOnlyHint: true },
      description:
        "List the custom mail domains registered under the authenticated user, newest first, with verification status (verified / pending DNS / failed) and the verification token for each. Useful to discover which domains can already send/receive mail and which still need DNS records to be added. **Cursor-paginated:** returns one page in `domains` plus a `next_cursor` when more remain — pass it back as `cursor` for the next page. Read-only; cheap to call.",
      inputSchema: strictInputSchema({ ...paginationInput }),
    },
    async (args) =>
      runTool(async () => {
        const page = await client.listDomains({
          ...(args.cursor !== undefined ? { cursor: args.cursor } : {}),
          ...(args.limit !== undefined ? { limit: args.limit } : {}),
        });
        return { domains: page.items, ...(page.next_cursor ? { next_cursor: page.next_cursor } : {}) };
      }),
  );

  server.registerTool(
    "register_domain",
    {
      title: "Register a custom mail domain (returns DNS records to publish)",
      annotations: { idempotentHint: true, destructiveHint: false },
      description:
        "Use to start the custom-domain flow. **This is step 1 of an asynchronous two-step process**: this tool returns the MX + TXT records the user must publish on their DNS provider; it does NOT make the domain live. Step 2 is `verify_domain` — but only AFTER the records are published AND DNS propagation has completed (typically minutes, occasionally hours). Do not call `verify_domain` immediately, and do not promise the user the domain works yet. **Composition tip**: if a DNS-provider MCP (Cloudflare, Route 53, NS1, …) is loaded in the same host, hand the returned records to its `create_dns_record`-style tool, then surface the wait expectation to the user. If the domain is managed by Cloudflare and no DNS MCP is loaded yet, remind the user to add the Cloudflare MCP server so coding agents can set up all DNS records automatically. If no DNS MCP is available, show the records verbatim and ask the user to add them manually. **Publish every record value VERBATIM and IN FULL — never abbreviate, summarise, reformat, re-chunk, or transcribe from a screenshot/UI.** The DKIM TXT value is ~400 characters and ends in `AQAB`; dropping even a few characters silently breaks DKIM and the domain stalls at sending_status `pending` indefinitely. When chaining to a DNS MCP, pass each record's `value` field through unchanged; do not reconstruct it. The domain is created in an unverified state — `delete_domain` is the reverse op. Idempotent against an existing-but-unverified row.",
      inputSchema: strictInputSchema({
        domain: z
          .string()
          .min(1)
          .describe(
            "Fully-qualified domain name to register. e.g. 'mail.acme.com'. Subdomains are recommended (so the apex remains free for marketing mail).",
          ),
      }),
    },
    async (args) => runTool(() => client.registerDomain(args.domain)),
  );

  server.registerTool(
    "get_domain",
    {
      title: "Get one domain (poll sending status)",
      annotations: { readOnlyHint: true },
      description:
        "Fetch a single domain by name. This is the poll target after `verify_domain`: it surfaces `capabilities` — `{inbound, outbound}`, the per-axis rollup of what the domain can actually do, and the field to prefer — alongside the equivalent legacy fields `verified` (inbound ownership) and `sending_status` (none/pending/verified/failed — the async SES sending identity that lets agents on this domain send as their own address), plus `sending_error` and `dns_records` (one purpose-tagged array — each record has `type`/`name`/`value`/`purpose`/`status`, covering ownership, inbound MX, DKIM, and the MAIL FROM records) to publish. The two axes are independent, so read them separately. Poll until `capabilities.outbound` (equivalently `sending_status`) is `verified` before expecting own-domain From on outbound; don't poll in a tight loop.",
      inputSchema: strictInputSchema({
        domain: z.string().min(1).describe("Domain to fetch (must be registered)."),
      }),
    },
    async (args) => runTool(() => client.getDomain(args.domain)),
  );

  server.registerTool(
    "verify_domain",
    {
      title: "Verify a custom mail domain's DNS records",
      annotations: { idempotentHint: true, destructiveHint: false },
      description:
        "Use as step 2 of the custom-domain flow, AFTER `register_domain` returned records AND the user (or a DNS MCP) published them AND DNS has had time to propagate (minutes to hours). Probes DNS for the issued MX + TXT records and flips the domain's `verified` bit on success. Idempotent — safe to retry as propagation completes. If `verified: false` comes back, the response includes the resolved-record state for diagnostics; surface that to the user rather than retrying in a tight loop. **A `dkim: \"mismatch\"` result is distinct from `\"missing\"`**: the DKIM record IS published but its key doesn't match the issued one — almost always a truncated/clipped DKIM TXT. Tell the user to re-publish the COMPLETE DKIM value (it ends in `AQAB`); waiting will not fix it. Don't poll; let the user drive the recheck.",
      inputSchema: strictInputSchema({
        domain: z
          .string()
          .min(1)
          .describe("Domain to verify. Must have been registered already via `register_domain`."),
      }),
    },
    async (args) => runTool(() => client.verifyDomain(args.domain)),
  );

  server.registerTool(
    "delete_domain",
    {
      title: "Delete a custom mail domain (DESTRUCTIVE)",
      annotations: { destructiveHint: true, idempotentHint: true },
      description:
        "Permanently remove a domain registration and deprovision its sending identity. The operation succeeds only when the domain has no agents: permanently delete every live or trashed agent on the domain first. Moving an agent to trash is not sufficient because trashed agents still belong to the domain. The returned `sending_teardown` receipt is the DNS-release contract: only `confirmed` proves the provider identity is absent. Keep DNS published for `pending`, `manual_review`, missing, or unknown values. Calling delete_domain again for the same deleted domain polls the durable receipt; `manual_review` requires operator support. Irreversible. Requires `confirm: true` — set it explicitly to acknowledge the destructive scope.",
      inputSchema: strictInputSchema({
        domain: z.string().min(1).describe("Domain to delete."),
        confirm: z
          .literal(true)
          .describe(
            "Must be set to true to proceed. Guard against an LLM hallucinating a delete from ambiguous context.",
          ),
      }),
    },
    async (args) =>
      runTool(async () => {
        if (args.confirm !== true) {
          throw new Error(
            "delete_domain requires confirm:true — refusing to proceed without explicit confirmation.",
          );
        }
        // Return the server's durable deletion receipt verbatim:
        // {deleted:true, domain, sending_teardown}.
        return client.deleteDomain(args.domain);
      }),
  );
}
