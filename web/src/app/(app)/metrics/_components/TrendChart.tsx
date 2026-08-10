"use client";

// Delivery-rate trend over the window, plus daily volume as context.
//
// Two measures of different scale, so they are TWO charts sharing one x-scale
// rather than one chart with two y-axes. A dual axis lets the author imply any
// correlation they like by choosing the scales.
//
// The rate line BREAKS on a day whose rate is null. Null means that day had no
// denominator — nothing was sent — and plotting it as 0% would draw a crash
// that never happened. This is the same null-vs-zero rule the tiles follow,
// applied to the mark.

import { useId, useState } from "react";

export type TrendPoint = {
  day: string;
  deliveredRate: number | null;
  accepted: number;
  delivered: number;
  bounced: number;
};

const W = 720;
const RATE_H = 132;
const VOL_H = 46;
const PAD = { left: 34, right: 8, top: 10, bottom: 6 };

function fmtDay(iso: string): string {
  const d = new Date(iso);
  return `${d.getUTCDate()} ${["Jan","Feb","Mar","Apr","May","Jun","Jul","Aug","Sep","Oct","Nov","Dec"][d.getUTCMonth()]}`;
}

export function TrendChart({ points }: { points: TrendPoint[] }) {
  const [hover, setHover] = useState<number | null>(null);
  const clipId = useId();
  if (points.length < 2) return null;

  const plotW = W - PAD.left - PAD.right;
  const x = (i: number) => PAD.left + (plotW * i) / Math.max(points.length - 1, 1);

  // The rate axis floors at the lowest observed value rounded down, never at
  // 0: a 97%–99% series plotted against a 0–100 axis is a flat line that hides
  // exactly the variation worth seeing.
  const rated = points.filter((p) => p.deliveredRate !== null).map((p) => p.deliveredRate as number);
  const lo = rated.length ? Math.max(0, Math.floor(Math.min(...rated) * 20) / 20 - 0.05) : 0;
  const hi = 1;
  const yRate = (v: number) => PAD.top + (RATE_H - PAD.top - PAD.bottom) * (1 - (v - lo) / Math.max(hi - lo, 0.01));

  const maxVol = Math.max(...points.map((p) => p.accepted), 1);
  const yVol = (v: number) => VOL_H - 4 - (VOL_H - 12) * (v / maxVol);

  // Build one path per unbroken run so gaps are real gaps, not interpolated.
  const runs: string[] = [];
  let current: string[] = [];
  points.forEach((p, i) => {
    if (p.deliveredRate === null) {
      if (current.length > 1) runs.push(current.join(" "));
      current = [];
      return;
    }
    current.push(`${current.length ? "L" : "M"}${x(i).toFixed(1)},${yRate(p.deliveredRate).toFixed(1)}`);
  });
  if (current.length > 1) runs.push(current.join(" "));

  const ticks = [lo, (lo + hi) / 2, hi];
  const active = hover === null ? null : points[hover];

  return (
    <div className="relative">
      <svg
        viewBox={`0 0 ${W} ${RATE_H}`}
        className="w-full"
        role="img"
        aria-label={`Delivery rate by day, ${fmtDay(points[0].day)} to ${fmtDay(points[points.length - 1].day)}`}
      >
        <defs>
          <clipPath id={clipId}>
            <rect x={PAD.left} y={0} width={plotW} height={RATE_H} />
          </clipPath>
        </defs>
        {ticks.map((t) => (
          <g key={t}>
            <line
              x1={PAD.left} x2={W - PAD.right} y1={yRate(t)} y2={yRate(t)}
              stroke="var(--border-sub)" strokeWidth="1"
            />
            <text x={4} y={yRate(t) + 3} fontSize="9" fill="var(--fg-subtle)">
              {(t * 100).toFixed(0)}%
            </text>
          </g>
        ))}
        <g clipPath={`url(#${clipId})`}>
          {runs.map((d) => (
            <path key={d.slice(0, 24)} d={d} fill="none" stroke="var(--accent)" strokeWidth="2"
              strokeLinecap="round" strokeLinejoin="round" />
          ))}
          {/* A lone day surrounded by gaps has no line to sit on, so it gets a
              dot — otherwise it would vanish from the chart entirely. */}
          {points.map((p, i) =>
            p.deliveredRate !== null &&
            (i === 0 || points[i - 1].deliveredRate === null) &&
            (i === points.length - 1 || points[i + 1].deliveredRate === null) ? (
              <circle key={p.day} cx={x(i)} cy={yRate(p.deliveredRate)} r="2.5" fill="var(--accent)" />
            ) : null,
          )}
        </g>
        {active && active.deliveredRate !== null && (
          <circle cx={x(hover as number)} cy={yRate(active.deliveredRate)} r="4"
            fill="var(--accent)" stroke="var(--bg-panel)" strokeWidth="2" />
        )}
        {hover !== null && (
          <line x1={x(hover)} x2={x(hover)} y1={PAD.top} y2={RATE_H - PAD.bottom}
            stroke="var(--border-strong)" strokeWidth="1" />
        )}
        {points.map((p, i) => (
          <rect
            key={p.day}
            x={x(i) - plotW / points.length / 2}
            y={0}
            width={plotW / points.length}
            height={RATE_H}
            fill="transparent"
            onMouseEnter={() => setHover(i)}
            onMouseLeave={() => setHover(null)}
          />
        ))}
      </svg>

      <svg viewBox={`0 0 ${W} ${VOL_H}`} className="w-full" role="img" aria-label="Messages accepted by day">
        {points.map((p, i) => {
          const barW = Math.max(plotW / points.length - 2, 1);
          // Clamp so the first and last bars stay inside the viewBox instead
          // of being clipped by half their width at the edges.
          const barX = Math.min(Math.max(x(i) - barW / 2, PAD.left), W - PAD.right - barW);
          return (
            <rect
              key={p.day}
              x={barX}
              y={yVol(p.accepted)}
              width={barW}
              height={Math.max(VOL_H - 4 - yVol(p.accepted), p.accepted > 0 ? 1 : 0)}
              rx="1.5"
              fill={hover === i ? "var(--accent)" : "var(--accent-soft)"}
              onMouseEnter={() => setHover(i)}
              onMouseLeave={() => setHover(null)}
            />
          );
        })}
        <text x={4} y={VOL_H - 5} fontSize="9" fill="var(--fg-subtle)">0</text>
        <text x={4} y={10} fontSize="9" fill="var(--fg-subtle)">{maxVol.toLocaleString()}</text>
      </svg>

      <div className="mt-1 flex justify-between text-[10px]" style={{ color: "var(--fg-subtle)" }}>
        <span>{fmtDay(points[0].day)}</span>
        <span>{fmtDay(points[points.length - 1].day)}</span>
      </div>

      {active && (
        <div
          className="pointer-events-none absolute -top-1 rounded-[var(--r-md)] border px-2.5 py-1.5 text-[11px]"
          style={{
            left: `min(calc(${((x(hover as number) / W) * 100).toFixed(1)}% + 8px), calc(100% - 190px))`,
            background: "var(--bg-panel)",
            borderColor: "var(--border)",
            color: "var(--fg)",
            boxShadow: "0 6px 20px rgb(0 0 0 / 0.14)",
          }}
        >
          <div style={{ color: "var(--fg-muted)" }}>{fmtDay(active.day)}</div>
          <div>
            delivered{" "}
            {active.deliveredRate === null ? (
              <span style={{ color: "var(--fg-subtle)" }}>— no sends</span>
            ) : (
              `${(active.deliveredRate * 100).toFixed(1)}%`
            )}
          </div>
          <div style={{ color: "var(--fg-muted)" }}>
            {active.accepted.toLocaleString()} accepted · {active.delivered.toLocaleString()} delivered
            {active.bounced > 0 ? ` · ${active.bounced.toLocaleString()} bounced` : ""}
          </div>
        </div>
      )}
    </div>
  );
}
