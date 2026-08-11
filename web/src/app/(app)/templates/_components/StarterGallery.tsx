"use client";

import { Chip } from "../../../components/loft/Chip";
import { Button } from "../../../components/loft/Button";
import type { StarterTemplateView } from "../_lib/types";
import { UseStarterButton } from "./UseStarterButton";

// "Start from a starter" gallery — one card per starter from
// GET /v1/starter-templates: name, description, and the variable
// catalog (name + required/raw badges + description). Preview opens the
// modal (verbatim master rendered with example data); Use copies the
// starter into the user's library via from_starter.

export function StarterGallery({
  starters,
  onPreview,
}: {
  starters: StarterTemplateView[];
  onPreview: (alias: string) => void;
}) {
  if (starters.length === 0) {
    return (
      <p className="text-[13px]" style={{ color: "var(--fg-muted)" }}>
        No starter templates are available on this deployment.
      </p>
    );
  }
  return (
    <div className="grid gap-4 md:grid-cols-2">
      {starters.map((s) => (
        <StarterCard key={s.alias} starter={s} onPreview={onPreview} />
      ))}
    </div>
  );
}

function StarterCard({
  starter,
  onPreview,
}: {
  starter: StarterTemplateView;
  onPreview: (alias: string) => void;
}) {
  return (
    <div
      className="flex flex-col p-5"
      style={{
        background: "var(--bg-panel)",
        border: "1px solid var(--border)",
        borderRadius: "var(--r-lg)",
      }}
    >
      <div className="flex items-center gap-2 flex-wrap">
        <h3
          className="text-[15px] font-semibold"
          style={{ color: "var(--fg)", margin: 0 }}
        >
          {starter.name}
        </h3>
        <Chip tone="neutral" mono>
          {starter.alias}
        </Chip>
      </div>
      <p
        className="mt-1.5 text-[12px] leading-[1.6]"
        style={{ color: "var(--fg-muted)" }}
      >
        {starter.description}
      </p>

      <div className="mt-3 flex-1">
        <div
          className="font-mono text-[10px] uppercase mb-1.5"
          style={{ color: "var(--fg-subtle)", letterSpacing: "0.08em" }}
        >
          Variables
        </div>
        <ul className="flex flex-col gap-1 m-0 p-0" style={{ listStyle: "none" }}>
          {starter.variables.map((v) => (
            <li key={v.name} className="flex items-baseline gap-2 min-w-0">
              <code
                className="font-mono text-[11px] shrink-0"
                style={{ color: "var(--fg)" }}
              >
                {v.name}
              </code>
              {v.required && <Chip tone="info">required</Chip>}
              {v.raw && <Chip tone="warn">raw</Chip>}
              {/* Truncate + title only where a hover pointer exists to open
                  the tooltip; on touch devices the full text wraps instead. */}
              <span
                className="text-[11px] min-w-0 [@media(hover:hover)]:truncate"
                style={{ color: "var(--fg-muted)" }}
                title={v.description}
              >
                {v.description}
              </span>
            </li>
          ))}
        </ul>
      </div>

      <div className="mt-4 flex items-start gap-2 flex-wrap">
        <Button variant="ghost" onClick={() => onPreview(starter.alias)}>
          Preview
        </Button>
        <UseStarterButton starterAlias={starter.alias} />
      </div>
    </div>
  );
}
