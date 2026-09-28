import type { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import type { McpClient } from "../client.js";
import { z } from "zod";
import { runTool, strictInputSchema } from "./util.js";

// External sending access (beta): the request/decision path that lifts the
// restriction on emailing external recipients. Both tools wrap
// /v1/account/sending-access/request and are ADMIN tier — the server requires
// an account-scoped credential. request_sending_access is a write (POST), so
// it is MUTATING: a read-only (abuse-paused) account is refused with
// account_read_only like every other write. Neither tool can grant access:
// approval is a local operator decision on the server, and the decision is
// emailed to the account owner.

const DECISION_NOTE =
  "The decision is made by an operator and emailed to the account owner — it does not arrive through this tool or any webhook; check back later with `get_sending_access_request`.";

export function registerSendingAccessTools(server: McpServer, client: McpClient): void {
  server.registerTool(
    "get_sending_access_request",
    {
      title: "Get the account's external sending access request (beta)",
      annotations: { readOnlyHint: true },
      description:
        "Read this account's most recent request for external sending access and its review `state` (`pending`, `approved`, `declined`; open set — treat unknown values as not approved). Fails with `not_found` (404) when the account has never filed one, and `not_implemented` (501) when the deployment does not restrict external sending at all. " +
        DECISION_NOTE +
        " `whoami`'s `sending_access.available_unlocks` explains what can lift the restriction on this deployment. BETA. Account scope only. Read-only.",
      inputSchema: strictInputSchema({}),
    },
    async () => runTool(() => client.getSendingAccessRequest()),
  );

  server.registerTool(
    "request_sending_access",
    {
      title: "Request external sending access (beta)",
      annotations: { destructiveHint: false },
      description:
        "Use when a send failed with `external_sending_not_enabled` (or `whoami`'s `sending_access` shows the account is restricted) and the account legitimately needs to email external recipients. Files ONE request for an operator to review; filing never grants access by itself. Call it ONCE: while a request is pending, calling again just returns the same pending request — do not retry or re-file. Do NOT retry on `rate_limited` (429; at most 3 requests per 30 days per account), `conflict` (409: the account is not restricted — already approved, or the control does not apply to it — so there is nothing to request) or any other error; tell the user instead. " +
        DECISION_NOTE +
        " Describe the real use case and recipients truthfully and specifically — vague or misleading requests are declined. `whoami`'s `sending_access.available_unlocks` lists every route this deployment accepts (`operator_approval` always; `verified_domain` / `paid_entitlement` only where listed). BETA. Account scope only.",
      inputSchema: strictInputSchema({
        use_case: z
          .string()
          .trim()
          .min(1)
          .max(2000)
          .describe("What you are building and why it needs to email external recipients (1-2000 chars)."),
        recipients: z
          .string()
          .trim()
          .min(1)
          .max(1000)
          .describe("Who you will email, e.g. 'customers who signed up on our site' (1-1000 chars)."),
        expected_daily_volume: z
          .number()
          .int()
          .min(1)
          .max(1_000_000)
          .describe("Expected recipients per day (1-1000000)."),
      }),
    },
    async (args) =>
      runTool(() =>
        client.requestSendingAccess({
          useCase: args.use_case,
          recipients: args.recipients,
          expectedDailyVolume: args.expected_daily_volume,
        }),
      ),
  );
}
