"use client";

import { useEffect, useState } from "react";
import { Button, Chip } from "@e2a/ui";
import type { StarterTemplateDetail } from "../_lib/types";
import { exampleData, substituteVars } from "../_lib/preview";
import { TemplatePreview } from "./TemplatePreview";
import { UseStarterButton } from "./UseStarterButton";

// Per-starter preview modal. Fetches the detail endpoint (which carries
// the VERBATIM master body sources) and renders a client-side preview by
// substituting each variable's catalog `example` — the layout source is
// always the master from the API, never a re-implementation.

export function StarterPreviewModal({
  alias,
  onClose,
}: {
  alias: string;
  onClose: () => void;
}) {
  const [detail, setDetail] = useState<StarterTemplateDetail | null>(null);
  const [error, setError] = useState("");

  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const res = await fetch(
          `/v1/starter-templates/${encodeURIComponent(alias)}`,
          { credentials: "include" },
        );
        if (!res.ok) {
          if (!cancelled) setError(`Failed to load starter (HTTP ${res.status})`);
          return;
        }
        const body: StarterTemplateDetail = await res.json();
        if (!cancelled) setDetail(body);
      } catch (err) {
        if (!cancelled)
          setError(err instanceof Error ? err.message : String(err));
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [alias]);

  // Escape closes the modal (mirrors the app layout's drawer behavior).
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [onClose]);

  const vars = detail ? exampleData(detail.variables) : {};

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center p-4 md:p-8">
      <button
        type="button"
        aria-label="Close preview"
        onClick={onClose}
        className="absolute inset-0"
        style={{ background: "rgba(26,23,20,0.5)" }}
      />
      <div
        role="dialog"
        aria-modal="true"
        aria-label={`Preview of the ${alias} starter template`}
        className="relative w-full max-w-[760px] max-h-full overflow-y-auto p-5 md:p-6"
        style={{
          background: "var(--bg)",
          border: "1px solid var(--border)",
          borderRadius: "var(--r-lg)",
        }}
      >
        <div className="flex items-center gap-2 mb-4 flex-wrap">
          <h2
            className="text-[16px] font-semibold"
            style={{ color: "var(--fg)", margin: 0 }}
          >
            {detail?.name ?? alias}
          </h2>
          <Chip tone="neutral" mono>
            {alias}
          </Chip>
          {detail && <Chip tone="neutral">v{detail.version}</Chip>}
          <span className="flex-1" />
          <Button variant="ghost" onClick={onClose}>
            Close
          </Button>
        </div>

        {error ? (
          <p className="text-[13px]" style={{ color: "var(--danger-strong)" }}>
            {error}
          </p>
        ) : !detail ? (
          <p className="text-[13px]" style={{ color: "var(--fg-muted)" }}>
            Loading preview…
          </p>
        ) : (
          <>
            <p
              className="text-[12px] mb-4 leading-[1.6]"
              style={{ color: "var(--fg-muted)" }}
            >
              Rendered with each variable&apos;s example value from the
              catalog.
            </p>
            <TemplatePreview
              subject={substituteVars(detail.subject, vars)}
              textBody={substituteVars(detail.text, vars)}
              htmlBody={substituteVars(detail.html, vars, {
                escape: true,
              })}
            />
            <div className="mt-5 flex justify-end">
              <UseStarterButton starterAlias={alias} />
            </div>
          </>
        )}
      </div>
    </div>
  );
}
