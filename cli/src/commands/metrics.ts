import type { AgentMetricsView, AccountMetricsView, MetricsSummaryView, MetricsRatesView } from "@e2a/sdk/v1";
import { createClient } from "../sdk.js";
import { EXIT, fail } from "../exit.js";
import { parseRfc3339 } from "../time.js";

export interface MetricsOptions {
  start?: string;
  end?: string;
  byAgent?: boolean;
  json?: boolean;
}

export const METRICS_USAGE =
  "usage: e2a metrics [<agent-email>] [--start <rfc3339>] [--end <rfc3339>] [--by-agent] [--json]";

/**
 * Render a rate as a percentage, or "n/a" when the server sent null.
 *
 * The null is load-bearing and must survive to the terminal: it means the
 * denominator was zero, which is NOT the same as a rate of 0%. Printing "0%"
 * for an agent that simply sent nothing would read as total delivery failure.
 */
function pct(rate: number | null | undefined): string {
  if (rate === null || rate === undefined) return "n/a";
  return `${(rate * 100).toFixed(1)}%`;
}

function ratesLine(rates: MetricsRatesView): string {
  return (
    `  delivered ${pct(rates.deliveredRate)}   bounced ${pct(rates.bounceRate)}   ` +
    `complaints ${pct(rates.complaintRate)}   suppressed ${pct(rates.suppressionBlockRate)}\n`
  );
}

function summaryLines(s: MetricsSummaryView): string {
  const bounced = s.bouncedHard + s.bouncedSoft + s.bouncedUndetermined;
  return (
    `  outbound  accepted ${s.accepted}  submitted ${s.submitted}  delivered ${s.delivered}\n` +
    `            bounced ${bounced} (hard ${s.bouncedHard}/soft ${s.bouncedSoft}/undetermined ${s.bouncedUndetermined})  ` +
    `complained ${s.complained}\n` +
    `            suppressed ${s.suppressed}  failed ${s.sendFailed}\n` +
    `  inbound   received ${s.received}  dmarc pass ${s.dmarcPass}/fail ${s.dmarcFail}/none ${s.dmarcNone}/error ${s.dmarcError}\n` +
    `  review    held ${s.reviewHeld}  approved ${s.reviewApproved}  rejected ${s.reviewRejected}  ` +
    `expired ${s.reviewExpiredApproved + s.reviewExpiredRejected}\n`
  );
}

/**
 * Warn when the lifecycle ledger does not cover every message in the window.
 * Silence here would present an undercount as a delivery problem, which is
 * the single most misleading thing this command could do.
 */
function coverageNote(inWindow: number, withLifecycle: number): string {
  if (withLifecycle >= inWindow) return "";
  const missing = inWindow - withLifecycle;
  return (
    `  note: ${missing} of ${inWindow} messages have no lifecycle record, so the\n` +
    `        counters above undercount by up to that many. This is a ledger gap,\n` +
    `        not lost mail.\n`
  );
}

function windowLine(start: Date, end: Date): string {
  return `window ${start.toISOString()} .. ${end.toISOString()} (cohort by send time; last ~72h still settling)\n`;
}

function renderAgent(view: AgentMetricsView): string {
  return (
    `${view.agentEmail}\n` +
    windowLine(view.start, view.end) +
    ratesLine(view.rates) +
    summaryLines(view.summary) +
    coverageNote(view.messagesInWindow, view.messagesWithLifecycle)
  );
}

function renderAccount(view: AccountMetricsView): string {
  let out =
    `account totals\n` +
    windowLine(view.start, view.end) +
    ratesLine(view.rates) +
    summaryLines(view.summary) +
    coverageNote(view.messagesInWindow, view.messagesWithLifecycle);

  const agents = view.agents ?? [];
  if (agents.length > 0) {
    out += `\nby agent (busiest first)\n`;
    for (const a of agents) {
      out +=
        `  ${a.agentEmail}\n` +
        `    messages ${a.messagesInWindow}  accepted ${a.summary.accepted}  ` +
        `delivered ${a.summary.delivered}  received ${a.summary.received}  ` +
        `rate ${pct(a.rates.deliveredRate)}\n`;
    }
  }
  if (view.agentsTruncated) {
    out +=
      `  note: more agents have traffic than are listed; the totals above still\n` +
      `        cover every agent.\n`;
  }
  return out;
}

export async function metrics(email: string | undefined, opts: MetricsOptions): Promise<void> {
  // --by-agent breaks down an ACCOUNT rollup. Silently ignoring it on a
  // single-agent read would let a script believe it asked for a breakdown and
  // got one, so it is a usage error instead.
  if (email && opts.byAgent) {
    fail(
      EXIT.USAGE,
      `--by-agent applies to the account rollup, not a single inbox; drop the agent argument\n${METRICS_USAGE}`,
    );
  }

  const start = opts.start ? parseRfc3339(opts.start, "--start", METRICS_USAGE) : undefined;
  const end = opts.end ? parseRfc3339(opts.end, "--end", METRICS_USAGE) : undefined;
  if (start && end && start.getTime() >= end.getTime()) {
    fail(EXIT.USAGE, `--start must be before --end\n${METRICS_USAGE}`);
  }

  const client = createClient();
  if (email) {
    const view = await client.messages.getMetrics(email, { start, end });
    process.stdout.write(opts.json ? JSON.stringify(view) + "\n" : renderAgent(view));
    return;
  }
  const view = await client.account.metrics({
    start,
    end,
    groupBy: opts.byAgent ? "agent" : undefined,
  });
  process.stdout.write(opts.json ? JSON.stringify(view) + "\n" : renderAccount(view));
}
