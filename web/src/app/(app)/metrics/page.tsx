"use client";

// Account-wide delivery metrics.
//
// One GET /v1/metrics?group_by=agent feeds this whole page: the headline
// tiles, the funnel, and the by-inbox table all read the same aggregate, so
// nothing on screen can disagree with anything else. A second call for the
// immediately preceding window of equal length supplies the deltas — the
// endpoint returns one aggregate rather than a time series, so a comparison
// window is how we show direction without a bucketing parameter.

import { useMemo, useState } from "react";
import useSWR from "swr";
import Link from "next/link";
import { PageShell } from "../../components/loft/PageShell";
import { InfoTip } from "../../components/loft/InfoTip";
import { metricsKey } from "../../../lib/swrKeys";
import { METRIC_HELP } from "../../../lib/metricDefinitions";
import {
  getAccountMetrics,
  type MetricsSummary,
} from "../../components/onboarding/api";

const RANGES = [
  { days: 7, label: "7 days" },
  { days: 30, label: "30 days" },
  { days: 90, label: "90 days" },
] as const;

// Provider thresholds that put a sending account under review. Drawn on the
// two reputation tiles so a customer sees the danger zone before we have to
// intervene by hand — on a shared sending reputation, their number is our
// number too.
const BOUNCE_LIMIT = 0.05;
const COMPLAINT_LIMIT = 0.001;

/** A rate is null when its denominator was zero. Never render that as 0%. */
function pct(rate: number | null | undefined, digits = 1): string {
  if (rate === null || rate === undefined) return "—";
  return `${(rate * 100).toFixed(digits)}%`;
}

function num(value: number): string {
  return value.toLocaleString();
}

function bounced(s: MetricsSummary): number {
  return s.bounced_hard + s.bounced_soft + s.bounced_undetermined;
}

function delta(current: number | null, previous: number | null): string | null {
  if (current === null || previous === null) return null;
  const diff = (current - previous) * 100;
  if (Math.abs(diff) < 0.05) return "no change";
  return `${diff > 0 ? "+" : "−"}${Math.abs(diff).toFixed(1)} pts`;
}

type TileTone = "neutral" | "good" | "warn" | "danger";

function toneColor(tone: TileTone): string {
  if (tone === "good") return "var(--success)";
  if (tone === "warn") return "var(--warn)";
  if (tone === "danger") return "var(--danger)";
  return "var(--fg-strong)";
}

/**
 * Headline stat tile. The denominator is printed under every value on
 * purpose: delivered_rate is over accepted while bounce_rate is over
 * submitted, and unexplained non-reconciling numbers destroy trust in the
 * whole page.
 */
function Tile({
  label,
  helpLabel,
  help,
  value,
  denominator,
  trend,
  tone = "neutral",
  limitNote,
}: {
  label: string;
  /** Accessible name for the tooltip. Tiles show rates while funnel rows show
   *  counts, so "Delivered" appears twice on the page — these must differ. */
  helpLabel: string;
  help: string;
  value: string;
  denominator: string;
  trend: string | null;
  tone?: TileTone;
  limitNote?: string;
}) {
  return (
    <div
      className="rounded-[var(--r-lg)] border p-4"
      style={{ borderColor: "var(--border)", background: "var(--bg-panel)" }}
    >
      <div
        className="flex items-center text-[11px] uppercase tracking-[0.08em]"
        style={{ color: "var(--fg-muted)" }}
      >
        {label}
        <InfoTip label={helpLabel} text={help} />
      </div>
      <div className="mt-2 text-[28px] font-semibold leading-none" style={{ color: toneColor(tone) }}>
        {value}
      </div>
      <div className="mt-1.5 text-[11px]" style={{ color: "var(--fg-subtle)" }}>
        {denominator}
      </div>
      {trend && (
        <div className="mt-1 text-[11px]" style={{ color: "var(--fg-subtle)" }}>
          {trend} vs previous period
        </div>
      )}
      {limitNote && (
        <div className="mt-1.5 text-[11px]" style={{ color: "var(--fg-muted)" }}>
          {limitNote}
        </div>
      )}
    </div>
  );
}

/**
 * Funnel row. Bars are thin with a rounded data-end anchored to a common
 * baseline, and every row carries its own direct label — the shape is a
 * secondary cue, never the only one.
 */
function FunnelRow({
  label,
  help,
  value,
  max,
  lost,
}: {
  label: string;
  help: string;
  value: number;
  max: number;
  lost?: string;
}) {
  // The bar draws the SHORTFALL against the stage above, not just the
  // magnitude. At a healthy 97.8% three magnitude bars are pixel-identical
  // and the mark carries nothing — but the losses are the whole reason to
  // look. The shortfall segment gets a floor width so a real loss is never
  // invisible, and it is always accompanied by the count on the right.
  // With no baseline there is no shortfall to draw — 100% of nothing is not a
  // total loss. Computing it anyway painted the whole track red for an account
  // that simply had not sent, contradicting the tiles, which correctly render
  // an em dash. Same null-vs-zero rule as the rates, applied to the mark.
  const hasBaseline = max > 0;
  const kept = hasBaseline ? (value / max) * 100 : 0;
  const shortfall = hasBaseline ? Math.max(0, 100 - kept) : 0;
  const shortfallWidth = shortfall > 0 ? Math.max(shortfall, 0.8) : 0;
  const keptWidth = hasBaseline ? 100 - shortfallWidth : 0;
  return (
    <div className="flex items-center gap-3 py-1.5">
      <div className="flex w-[132px] shrink-0 items-center text-[12px]" style={{ color: "var(--fg-muted)" }}>
        {label}
        <InfoTip label={label} text={help} />
      </div>
      <div className="w-[72px] shrink-0 text-right text-[13px] tabular-nums" style={{ color: "var(--fg-strong)" }}>
        {num(value)}
      </div>
      <div
        className="flex h-[10px] min-w-0 flex-1 gap-[2px] overflow-hidden rounded-full"
        style={{ background: "var(--bg-sunken)" }}
      >
        <div className="h-full rounded-full" style={{ width: `${keptWidth}%`, background: "var(--accent)" }} />
        {shortfallWidth > 0 && (
          <div className="h-full rounded-full" style={{ width: `${shortfallWidth}%`, background: "var(--danger)" }} />
        )}
      </div>
      <div className="w-[190px] shrink-0 text-[11px]" style={{ color: "var(--fg-subtle)" }}>
        {lost ?? ""}
      </div>
    </div>
  );
}

/** A labelled segment of the inbound-authentication bar. */
type AuthSegment = { key: string; label: string; value: number; color: string };

export default function MetricsPage() {
  const [days, setDays] = useState<number>(30);

  const { data, error, isLoading } = useSWR(metricsKey(days, true), () =>
    getAccountMetrics({ groupByAgent: true, start: startOf(days), end: new Date() }),
  );
  // Comparison window: the equal-length period immediately before this one.
  const { data: prior } = useSWR(["metrics-prior", days], () =>
    getAccountMetrics({ start: startOf(days * 2), end: startOf(days) }),
  );

  const summary = data?.summary;
  const rates = data?.rates;
  const agents = useMemo(
    () => (data?.agents ?? []).slice().sort((a, b) => b.messages_in_window - a.messages_in_window),
    [data],
  );

  return (
    <PageShell
      title="Metrics"
      subtitle="Delivery health across every inbox on this account."
    >
      <div className="mb-4 flex flex-wrap items-center gap-2">
        {RANGES.map((r) => (
          <button
            key={r.days}
            type="button"
            onClick={() => setDays(r.days)}
            aria-pressed={days === r.days}
            className="rounded-[var(--r-md)] border px-2.5 py-1 text-[12px]"
            style={{
              borderColor: days === r.days ? "var(--accent)" : "var(--border)",
              color: days === r.days ? "var(--accent)" : "var(--fg-muted)",
              background: "var(--bg-panel)",
            }}
          >
            {r.label}
          </button>
        ))}
        <span className="ml-1 flex items-center text-[11px]" style={{ color: "var(--fg-subtle)" }}>
          Last 72 hours provisional
          <InfoTip label="the reporting window" text={METRIC_HELP.window} />
        </span>
      </div>

      {error && (
        <p className="text-[13px]" style={{ color: "var(--danger)" }}>
          Couldn&apos;t load metrics. {error instanceof Error ? error.message : ""}
        </p>
      )}

      {isLoading && !data && (
        <p className="text-[13px]" style={{ color: "var(--fg-muted)" }}>
          Loading…
        </p>
      )}

      {data && summary && rates && data.messages_in_window === 0 && (
        <EmptyState />
      )}

      {data && summary && rates && data.messages_in_window > 0 && (
        <>
          <section className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
            <Tile
              label="Delivered"
              helpLabel="the delivered rate"
              help={METRIC_HELP.deliveredRate}
              value={pct(rates.delivered_rate)}
              denominator={`${num(summary.delivered)} of ${num(summary.accepted)} accepted`}
              trend={delta(rates.delivered_rate, prior?.rates.delivered_rate ?? null)}
              tone="neutral"
            />
            <Tile
              label="Bounced"
              helpLabel="the bounce rate"
              help={METRIC_HELP.bounceRate}
              value={pct(rates.bounce_rate)}
              denominator={`${num(bounced(summary))} of ${num(summary.submitted)} submitted`}
              trend={delta(rates.bounce_rate, prior?.rates.bounce_rate ?? null)}
              tone={rates.bounce_rate !== null && rates.bounce_rate >= BOUNCE_LIMIT ? "danger" : "neutral"}
              limitNote="Review threshold 5%"
            />
            <Tile
              label="Complaints"
              helpLabel="the complaint rate"
              help={METRIC_HELP.complaintRate}
              value={pct(rates.complaint_rate, 2)}
              denominator={`${num(summary.complained)} of ${num(summary.delivered)} delivered`}
              trend={delta(rates.complaint_rate, prior?.rates.complaint_rate ?? null)}
              tone={
                rates.complaint_rate !== null && rates.complaint_rate >= COMPLAINT_LIMIT ? "danger" : "neutral"
              }
              limitNote="Review threshold 0.1%"
            />
            <Tile
              label="Suppressed"
              helpLabel="the suppression rate"
              help={METRIC_HELP.suppressionBlockRate}
              value={pct(rates.suppression_block_rate)}
              denominator={`${num(summary.suppressed)} of ${num(summary.accepted)} accepted`}
              trend={delta(rates.suppression_block_rate, prior?.rates.suppression_block_rate ?? null)}
              tone={summary.suppressed > 0 ? "warn" : "neutral"}
            />
          </section>

          <CoverageNote
            inWindow={data.messages_in_window}
            withLifecycle={data.messages_with_lifecycle}
          />

          <Panel title="Where mail went">
            {summary.accepted === 0 && (
              <p className="mb-2 text-[12px]" style={{ color: "var(--fg-subtle)" }}>
                No outbound mail with a lifecycle record in this window.
              </p>
            )}
            <FunnelRow
              label="Accepted"
              help={METRIC_HELP.accepted}
              value={summary.accepted}
              max={summary.accepted}
              lost={
                summary.review_held > 0
                  ? `${num(summary.review_held)} held for review`
                  : undefined
              }
            />
            <FunnelRow
              label="Submitted"
              help={METRIC_HELP.submitted}
              value={summary.submitted}
              max={summary.accepted}
              lost={[
                summary.suppressed > 0 ? `${num(summary.suppressed)} suppressed` : null,
                summary.send_failed > 0 ? `${num(summary.send_failed)} failed` : null,
              ]
                .filter(Boolean)
                .join(" · ")}
            />
            <FunnelRow
              label="Delivered"
              help={METRIC_HELP.delivered}
              value={summary.delivered}
              max={summary.accepted}
              lost={bounced(summary) > 0 ? `${num(bounced(summary))} bounced` : undefined}
            />
          </Panel>

          <Panel title="By inbox" help="Busiest first. Each row links to that inbox's messages.">
            {data.agents_truncated && (
              <p className="mb-2 text-[11px]" style={{ color: "var(--warn)" }}>
                More inboxes have traffic than are listed here. The account totals above still
                cover every one.
              </p>
            )}
            {agents.length === 0 ? (
              <p className="text-[12px]" style={{ color: "var(--fg-subtle)" }}>
                No per-inbox activity in this window.
              </p>
            ) : (
              <div className="overflow-x-auto">
                <table className="w-full text-[12px]">
                  <thead>
                    <tr style={{ color: "var(--fg-muted)" }}>
                      <th className="py-1.5 text-left font-normal">Inbox</th>
                      <th className="py-1.5 text-right font-normal">Messages</th>
                      <th className="py-1.5 text-right font-normal">Accepted</th>
                      <th className="py-1.5 text-right font-normal">Delivered</th>
                      <th className="py-1.5 text-right font-normal">Rate</th>
                      <th className="py-1.5 text-right font-normal">Bounced</th>
                    </tr>
                  </thead>
                  <tbody>
                    {agents.map((a) => (
                      <tr key={a.agent_email} style={{ borderTop: "1px solid var(--border-sub)" }}>
                        <td className="py-1.5">
                          <Link
                            href={`/inboxes/messages?agent=${encodeURIComponent(a.agent_email)}`}
                            style={{ color: "var(--accent)" }}
                          >
                            {a.agent_email}
                          </Link>
                        </td>
                        <td className="py-1.5 text-right tabular-nums">{num(a.messages_in_window)}</td>
                        <td className="py-1.5 text-right tabular-nums">{num(a.summary.accepted)}</td>
                        <td className="py-1.5 text-right tabular-nums">{num(a.summary.delivered)}</td>
                        <td className="py-1.5 text-right tabular-nums">{pct(a.rates.delivered_rate)}</td>
                        <td className="py-1.5 text-right tabular-nums">{num(bounced(a.summary))}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </Panel>

          <div className="grid gap-4 lg:grid-cols-2">
            <Panel title="Inbound & authentication" help={METRIC_HELP.dmarc}>
              <p className="mb-2 text-[12px]" style={{ color: "var(--fg-muted)" }}>
                {num(summary.received)} received
              </p>
              <AuthBar
                segments={[
                  { key: "pass", label: "Pass", value: summary.dmarc_pass, color: "var(--success)" },
                  { key: "none", label: "No policy", value: summary.dmarc_none, color: "var(--fg-subtle)" },
                  { key: "error", label: "Error", value: summary.dmarc_error, color: "var(--warn)" },
                  { key: "fail", label: "Fail", value: summary.dmarc_fail, color: "var(--danger)" },
                ]}
              />
            </Panel>

            <Panel title="Review queue" help={METRIC_HELP.review}>
              <dl className="grid grid-cols-2 gap-y-1.5 text-[12px]">
                <Row label="Held" value={summary.review_held} />
                <Row label="Approved" value={summary.review_approved} />
                <Row label="Rejected" value={summary.review_rejected} />
                <Row
                  label="Expired"
                  value={summary.review_expired_approved + summary.review_expired_rejected}
                  warn
                />
              </dl>
            </Panel>
          </div>

          <ReasonCodeDetail counters={data.counters ?? []} />
        </>
      )}
    </PageShell>
  );
}

function startOf(daysAgo: number): Date {
  return new Date(Date.now() - daysAgo * 24 * 60 * 60 * 1000);
}

function Panel({
  title,
  help,
  children,
}: {
  title: string;
  help?: string;
  children: React.ReactNode;
}) {
  return (
    <section
      className="mt-4 rounded-[var(--r-lg)] border p-4"
      style={{ borderColor: "var(--border)", background: "var(--bg-panel)" }}
    >
      <h2
        className="mb-3 flex items-center text-[11px] uppercase tracking-[0.08em]"
        style={{ color: "var(--fg-muted)" }}
      >
        {title}
        {help && <InfoTip label={title} text={help} />}
      </h2>
      {children}
    </section>
  );
}

function Row({ label, value, warn }: { label: string; value: number; warn?: boolean }) {
  return (
    <>
      <dt style={{ color: "var(--fg-muted)" }}>{label}</dt>
      <dd
        className="text-right tabular-nums"
        style={{ color: warn && value > 0 ? "var(--warn)" : "var(--fg-strong)" }}
      >
        {num(value)}
      </dd>
    </>
  );
}

/**
 * Stacked composition bar with a 2px surface gap between segments and a text
 * label on every one — identity never rests on color alone, which also covers
 * the colorblind and forced-colors cases.
 */
function AuthBar({ segments }: { segments: AuthSegment[] }) {
  const total = segments.reduce((sum, s) => sum + s.value, 0);
  if (total === 0) {
    return (
      <p className="text-[12px]" style={{ color: "var(--fg-subtle)" }}>
        No inbound mail in this window.
      </p>
    );
  }
  return (
    <div>
      <div className="flex h-[10px] w-full gap-[2px] overflow-hidden rounded-full">
        {segments
          .filter((s) => s.value > 0)
          .map((s) => (
            <div
              key={s.key}
              style={{ width: `${(s.value / total) * 100}%`, background: s.color }}
            />
          ))}
      </div>
      <ul className="mt-2 flex flex-wrap gap-x-4 gap-y-1 text-[11px]">
        {segments.map((s) => (
          <li key={s.key} className="flex items-center gap-1.5" style={{ color: "var(--fg-muted)" }}>
            <span
              aria-hidden="true"
              className="inline-block h-[8px] w-[8px] rounded-full"
              style={{ background: s.color }}
            />
            {s.label} {num(s.value)}
          </li>
        ))}
      </ul>
    </div>
  );
}

function CoverageNote({ inWindow, withLifecycle }: { inWindow: number; withLifecycle: number }) {
  if (withLifecycle >= inWindow) return null;
  return (
    <p
      className="mt-3 flex items-start text-[11px]"
      style={{ color: "var(--fg-muted)" }}
    >
      <span>
        {num(inWindow - withLifecycle)} of {num(inWindow)} messages have no lifecycle record, so the
        counters below undercount by up to that many. This is a recording gap, not lost mail.
      </span>
      <InfoTip label="ledger coverage" text={METRIC_HELP.coverage} />
    </p>
  );
}

function ReasonCodeDetail({ counters }: { counters: { reason_code: string; stage: string; outcome: string; observations: number; messages: number }[] }) {
  const [open, setOpen] = useState(false);
  if (counters.length === 0) return null;
  return (
    <section className="mt-4">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        aria-expanded={open}
        className="text-[12px]"
        style={{ color: "var(--accent)" }}
      >
        {open ? "Hide" : "Show"} reason-code detail ({counters.length})
      </button>
      {open && (
        <div
          className="mt-2 overflow-x-auto rounded-[var(--r-lg)] border p-4"
          style={{ borderColor: "var(--border)", background: "var(--bg-panel)" }}
        >
          <table className="w-full text-[12px]">
            <thead>
              <tr style={{ color: "var(--fg-muted)" }}>
                <th className="py-1.5 text-left font-normal">Reason code</th>
                <th className="py-1.5 text-left font-normal">Stage</th>
                <th className="py-1.5 text-left font-normal">Outcome</th>
                <th className="py-1.5 text-right font-normal">
                  <span className="inline-flex items-center">
                    Observations
                    <InfoTip label="observations" text={METRIC_HELP.observations} />
                  </span>
                </th>
                <th className="py-1.5 text-right font-normal">Messages</th>
              </tr>
            </thead>
            <tbody>
              {counters.map((c) => (
                <tr key={c.reason_code} style={{ borderTop: "1px solid var(--border-sub)" }}>
                  <td className="py-1.5 font-mono text-[11px]">{c.reason_code}</td>
                  <td className="py-1.5">{c.stage}</td>
                  <td className="py-1.5">{c.outcome}</td>
                  <td className="py-1.5 text-right tabular-nums">{num(c.observations)}</td>
                  <td className="py-1.5 text-right tabular-nums">{num(c.messages)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}

function EmptyState() {
  return (
    <div
      className="rounded-[var(--r-lg)] border p-6 text-center"
      style={{ borderColor: "var(--border)", background: "var(--bg-panel)" }}
    >
      <p className="text-[13px]" style={{ color: "var(--fg-strong)" }}>
        No mail in this window yet.
      </p>
      <p className="mx-auto mt-1.5 max-w-[420px] text-[12px]" style={{ color: "var(--fg-muted)" }}>
        Delivery, bounce, and complaint rates appear here once your agents start sending and
        receiving. Rates read “—” rather than 0% until there is something to divide by.
      </p>
      <Link
        href="/get-started"
        className="mt-3 inline-block text-[12px]"
        style={{ color: "var(--accent)" }}
      >
        Get started →
      </Link>
    </div>
  );
}
