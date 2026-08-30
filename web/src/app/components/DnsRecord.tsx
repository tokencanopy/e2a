"use client";

import { useState } from "react";

export function DNSRecord({
  type,
  label,
  fields,
}: {
  type: string;
  label: string;
  fields: { label: string; value: string }[];
}) {
  return (
    <div className="border border-border rounded-lg p-4">
      <div className="flex items-center gap-2 mb-4">
        <span className="inline-flex items-center px-2 py-0.5 rounded text-xs font-mono font-bold bg-accent/10 text-accent">
          {type}
        </span>
        <span className="text-sm font-medium">{label}</span>
      </div>
      <div className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-2 items-center">
        {fields.map((f) => (
          <DNSField key={f.label} label={f.label} value={f.value} />
        ))}
      </div>
    </div>
  );
}

function DNSField({ label, value }: { label: string; value: string }) {
  const [copied, setCopied] = useState(false);

  const copy = () => {
    navigator.clipboard.writeText(value);
    setCopied(true);
    setTimeout(() => setCopied(false), 2000);
  };

  return (
    <>
      <span className="text-xs text-muted">{label}</span>
      <div className="flex items-center gap-2 min-w-0">
        <code className="text-sm font-mono bg-background rounded px-2 py-1 border border-border flex-1 min-w-0 break-all">
          {value}
        </code>
        <button
          onClick={copy}
          className="text-xs text-muted hover:text-foreground transition shrink-0"
        >
          {copied ? "Copied!" : "Copy"}
        </button>
      </div>
    </>
  );
}
