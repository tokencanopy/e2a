"use client";

// InfoTip — a small "what does this mean?" affordance for a metric label.
//
// Deliberately a <button>, not a hover-only <span>. A metric definition is
// the kind of thing a keyboard or touch user needs most, and hover-only help
// is unreachable for both. The button toggles on click/Enter/Space, opens on
// pointer hover, and closes on Escape or an outside click.
//
// The tooltip text is linked with aria-describedby rather than announced as a
// live region: it describes the adjacent label, so a screen reader should read
// it as part of that label's description, not interrupt with it.

import { useEffect, useId, useRef, useState } from "react";

export function InfoTip({
  label,
  text,
  className = "",
}: {
  /** What this tip explains — used for the button's accessible name. */
  label: string;
  text: string;
  className?: string;
}) {
  // Hover and click are tracked separately on purpose. With one shared flag,
  // a mouse user hovers (opens), clicks (toggles closed) and the control reads
  // as broken. Pinning is what click controls; hovering is transient.
  const [hovered, setHovered] = useState(false);
  const [pinned, setPinned] = useState(false);
  const open = hovered || pinned;
  const id = useId();
  const wrapRef = useRef<HTMLSpanElement>(null);

  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        setPinned(false);
        setHovered(false);
      }
    };
    const onDown = (e: MouseEvent) => {
      if (!wrapRef.current?.contains(e.target as Node)) setPinned(false);
    };
    document.addEventListener("keydown", onKey);
    document.addEventListener("mousedown", onDown);
    return () => {
      document.removeEventListener("keydown", onKey);
      document.removeEventListener("mousedown", onDown);
    };
  }, [open]);

  return (
    <span
      ref={wrapRef}
      className={`relative inline-flex items-center ${className}`}
      onMouseEnter={() => setHovered(true)}
      onMouseLeave={() => setHovered(false)}
    >
      <button
        type="button"
        aria-label={`What is ${label}?`}
        aria-expanded={open}
        aria-describedby={open ? id : undefined}
        onClick={() => setPinned((v) => !v)}
        onFocus={() => setHovered(true)}
        onBlur={() => setHovered(false)}
        className="ml-1 inline-flex h-[14px] w-[14px] shrink-0 items-center justify-center rounded-full border text-[9px] leading-none"
        style={{
          borderColor: "var(--border)",
          color: "var(--fg-subtle)",
          background: "transparent",
        }}
      >
        <span aria-hidden="true">?</span>
      </button>
      {open && (
        <span
          id={id}
          role="tooltip"
          className="absolute left-0 top-[calc(100%+6px)] z-20 w-[260px] rounded-[var(--r-md)] border p-2.5 text-[11px] font-normal leading-[1.45] normal-case tracking-normal"
          style={{
            background: "var(--bg-panel)",
            borderColor: "var(--border)",
            color: "var(--fg)",
            boxShadow: "0 6px 20px rgb(0 0 0 / 0.14)",
          }}
        >
          {text}
        </span>
      )}
    </span>
  );
}
