import type { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import type { McpClient } from "../client.js";
import { z } from "zod";
import { emailSelector, runTool, strictInputSchema } from "./util.js";

// Delivery counter metrics, split by scope exactly as the REST surface is:
//   - get_agent_metrics   (runtime): one agent. An agent-scoped credential can
//     read its OWN numbers, which is the point — an agent that can see its own
//     bounce and suppression rates can correct its behavior without a human in
//     the loop.
//   - get_account_metrics (admin): every agent on the account. Account scope
//     only, mirroring the handler's requireAccountUser.
//
// Both are beta and both read the same lifecycle ledger, so their counters and
// rate denominators are identical by construction.

// The shared caveats. Repeated into each description on purpose: a model
// reading one tool's schema in isolation must not have to infer either the
// settling window or the meaning of a null rate, because both mislead in the
// direction of "something is broken" when nothing is.
const WINDOW_NOTE =
  "Messages join the window by their own send time, not by when each observation landed. " +
  "Bounce and complaint feedback keeps arriving for up to 72 hours, so treat the most recent " +
  "days as provisional rather than final. Defaults to the last 30 days; the window may not exceed 92 days.";

const RATES_NOTE =
  "Every value in `rates` is null — never 0 — when its denominator is zero, so 'no traffic' stays " +
  "distinguishable from 'everything failed'. Formulas are fixed by the server: " +
  "`delivered_rate = delivered / (accepted - loopback)`; " +
  "`bounce_rate = (bounced_hard + bounced_soft + bounced_undetermined) / (submitted - loopback)`; " +
  "`complaint_rate = complained / delivered`; " +
  "`suppression_block_rate = suppressed / (accepted - loopback)`. " +
  "`delivered` means a recipient server accepted the message; it does NOT claim inbox placement.";

const COVERAGE_NOTE =
  "Compare `messages_with_lifecycle` against `messages_in_window`: when it is lower, part of the window " +
  "predates the lifecycle ledger and every counter undercounts by that difference. That is a recording " +
  "gap, not lost mail — do not report it as a delivery failure.";

const startInput = z
  .string()
  .optional()
  .describe("Inclusive start of the window, RFC 3339 with an explicit offset (e.g. 2026-08-01T00:00:00Z). Defaults to 30 days before end.");

const endInput = z
  .string()
  .optional()
  .describe("Exclusive end of the window, RFC 3339 with an explicit offset. Defaults to now.");

/**
 * Parse an optional RFC 3339 tool argument. A bad value is rejected here
 * rather than forwarded, because `new Date("last tuesday")` yields an Invalid
 * Date that serializes to null and would silently widen the window to the
 * 30-day default instead of failing.
 */
function parseWindowArg(value: string | undefined, field: string): Date | undefined {
  if (value === undefined) return undefined;
  const parsed = new Date(value);
  if (Number.isNaN(parsed.getTime())) {
    throw new Error(`${field} is not a valid RFC 3339 date-time: ${JSON.stringify(value)}`);
  }
  return parsed;
}

export function registerMetricsTools(server: McpServer, client: McpClient): void {
  server.registerTool(
    "get_agent_metrics",
    {
      title: "Get an agent's delivery metrics (beta)",
      annotations: { readOnlyHint: true, idempotentHint: true },
      description:
        "Beta: delivery counter metrics for ONE agent, aggregated from the message lifecycle ledger — " +
        "how much mail this agent got accepted, submitted, delivered, bounced, complained about, or had " +
        "blocked by a suppression, plus inbound DMARC results and human-review outcomes. " +
        "Use this to check an inbox's own sending health before or after a campaign. " +
        `${WINDOW_NOTE} ${RATES_NOTE} ${COVERAGE_NOTE} ` +
        "Returns `summary` (counters at message grain), `rates`, and `counters` (per lifecycle reason code, " +
        "with `observations` counting retries and `messages` counting distinct messages).",
      inputSchema: strictInputSchema({
        email: emailSelector,
        start: startInput,
        end: endInput,
      }),
    },
    async (args) =>
      runTool(() =>
        client.getAgentMetrics(
          {
            ...(args.start !== undefined ? { start: parseWindowArg(args.start, "start")! } : {}),
            ...(args.end !== undefined ? { end: parseWindowArg(args.end, "end")! } : {}),
          },
          args.email,
        ),
      ),
  );

  server.registerTool(
    "get_account_metrics",
    {
      title: "Get account-wide delivery metrics (beta)",
      annotations: { readOnlyHint: true, idempotentHint: true },
      description:
        "Beta: delivery counter metrics across EVERY agent on the account, on the same counters and rate " +
        "denominators as get_agent_metrics — so an account total and the per-agent numbers under it cannot " +
        "disagree. Use this for an account-level health check; use get_agent_metrics to drill into one inbox. " +
        "Set group_by:'agent' to also receive a per-agent breakdown in `agents`, busiest first — it is capped " +
        "at 200 agents and sets `agents_truncated` when it cuts, while the account totals stay complete either way. " +
        "Set bucket:'day' to also receive `buckets`: one entry per UTC calendar day, gap-filled so a silent day is present with zeroes rather than missing. " +
        "Bucket counters sum to the window totals; bucket RATES do not average to the window rate, because a rate of rates is not the rate. " +
        "Also returns a `webhooks` block: whether YOUR CODE received the events, which the email counters cannot answer. " +
        "Its grain is one row per event per subscriber, so those counts legitimately exceed message counts. " +
        "`webhooks.endpoints_auto_disabled` above zero is the most urgent signal here — e2a has stopped delivering to " +
        "an endpoint after sustained failure, so events are being DROPPED rather than retried. " +
        `${WINDOW_NOTE} ${RATES_NOTE} ${COVERAGE_NOTE} ` +
        "Account scope only; an agent-scoped credential reads its own inbox with get_agent_metrics instead.",
      inputSchema: strictInputSchema({
        start: startInput,
        end: endInput,
        bucket: z
          .literal("day")
          .optional()
          .describe("Set to 'day' to include per-day buckets for trend analysis. Omit for window totals only, which is the cheaper read."),
        group_by: z
          .literal("agent")
          .optional()
          .describe("Set to 'agent' to include the per-agent breakdown. Omit for account totals only, which is the cheaper read."),
      }),
    },
    async (args) =>
      runTool(() =>
        client.getAccountMetrics({
          ...(args.start !== undefined ? { start: parseWindowArg(args.start, "start")! } : {}),
          ...(args.end !== undefined ? { end: parseWindowArg(args.end, "end")! } : {}),
          ...(args.bucket !== undefined ? { bucket: args.bucket } : {}),
          ...(args.group_by !== undefined ? { groupBy: args.group_by } : {}),
        }),
      ),
  );
}
