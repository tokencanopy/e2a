"use client";

import { useEffect, useState } from "react";
import { Chip, Eyebrow } from "@e2a/ui";
import { setProtection } from "../../../components/onboarding/api";
import type {
  ProtectionConfig,
  ProtectionGate,
  ProtectionScan,
} from "../../../components/onboarding/types";

// Beta protection editor for the inbox-settings page. Exposes the whole
// protection posture — the inbound/outbound trust gates (who may send +
// what a non-match does), the content-scan sensitivity, and the review-
// queue holds (TTL + on-expiry). The protection PUT is a wholesale
// replace, so this form edits every section and submits it as one body.

const MAX_TTL = 604800; // must match internal/identity HITLMaxTTLSeconds

const POLICY_OPTS = [
  { value: "open", label: "Open (anyone)" },
  { value: "domain", label: "Domains" },
  { value: "allowlist", label: "Addresses" },
] as const;

const ACTION_OPTS = [
  { value: "flag", label: "Flag (deliver)" },
  { value: "review", label: "Hold for review" },
  { value: "block", label: "Block" },
] as const;

const SCAN_OPTS = [
  { value: "off", label: "Off" },
  { value: "low", label: "Low" },
  { value: "medium", label: "Medium" },
  { value: "high", label: "High" },
] as const;

const TTL_PRESETS = [
  { label: "1 hour", seconds: 3600 },
  { label: "1 day", seconds: 86400 },
  { label: "7 days", seconds: 604800 },
] as const;

type Policy = "open" | "domain" | "allowlist";
type Action = "flag" | "review" | "block";
type Sensitivity = "off" | "low" | "medium" | "high";

// One direction's draft state (gate policy/action/allowlist + scan).
// allowlist is kept as raw textarea text; split into lines on save.
type DirectionDraft = {
  policy: Policy;
  action: Action;
  allowlist: string;
  scan: Sensitivity;
};

function directionFromConfig(d: {
  gate: ProtectionGate;
  scan: ProtectionScan;
}): DirectionDraft {
  return {
    policy: (d.gate.policy ?? "open") as Policy,
    action: (d.gate.action ?? "flag") as Action,
    allowlist: (d.gate.allowlist ?? []).join("\n"),
    scan: (d.scan.sensitivity ?? "off") as Sensitivity,
  };
}

function directionToConfig(d: DirectionDraft) {
  const entries = d.allowlist
    .split("\n")
    .map((s) => s.trim())
    .filter(Boolean);
  return {
    gate: {
      policy: d.policy,
      // Allowlist is ignored server-side for `open`; send [] to keep the
      // payload clean.
      allowlist: d.policy === "open" ? [] : entries,
      action: d.action,
    },
    scan: { sensitivity: d.scan },
  };
}

function isDirectionDirty(current: DirectionDraft, baseline: DirectionDraft): boolean {
  if (current.policy !== baseline.policy) return true;
  if (current.action !== baseline.action) return true;
  if (current.scan !== baseline.scan) return true;
  if (current.policy !== "open" && current.allowlist !== baseline.allowlist) {
    return true;
  }
  return false;
}

function Segmented<T extends string>({
  value,
  options,
  onChange,
  ariaLabel,
}: {
  value: T;
  options: ReadonlyArray<{ value: T; label: string }>;
  onChange: (v: T) => void;
  ariaLabel: string;
}) {
  return (
    <div className="flex items-center gap-1 flex-wrap" role="group" aria-label={ariaLabel}>
      {options.map((o) => (
        <button
          key={o.value}
          type="button"
          aria-pressed={value === o.value}
          onClick={() => onChange(o.value)}
          className={`text-xs px-2 py-1 rounded-md border transition ${
            value === o.value
              ? "bg-foreground text-background border-foreground"
              : "border-border hover:bg-surface"
          }`}
        >
          {o.label}
        </button>
      ))}
    </div>
  );
}

function DirectionFields({
  title,
  gateLabel,
  draft,
  onChange,
}: {
  title: string;
  gateLabel: string;
  draft: DirectionDraft;
  onChange: (next: DirectionDraft) => void;
}) {
  return (
    <div className="space-y-2 border border-border rounded-md p-3">
      <div className="text-xs font-semibold text-foreground">{title}</div>

      <div>
        <div className="text-xs text-muted mb-1">{gateLabel}</div>
        <Segmented
          ariaLabel={`${title} trust gate`}
          value={draft.policy}
          options={POLICY_OPTS}
          onChange={(policy) => onChange({ ...draft, policy })}
        />
      </div>

      {draft.policy !== "open" && (
        <div>
          <div className="text-xs text-muted mb-1">
            {draft.policy === "domain"
              ? "Allowed domains (one per line)"
              : "Allowed addresses (one per line)"}
          </div>
          <textarea
            value={draft.allowlist}
            onChange={(e) => onChange({ ...draft, allowlist: e.target.value })}
            rows={3}
            placeholder={draft.policy === "domain" ? "acme.com" : "alice@acme.com"}
            className="w-full text-xs font-mono px-2 py-1.5 border border-border rounded-md bg-surface"
            aria-label={`${title} allowlist`}
          />
        </div>
      )}

      <div>
        <div className="text-xs text-muted mb-1">On a non-match</div>
        <Segmented
          ariaLabel={`${title} non-match action`}
          value={draft.action}
          options={ACTION_OPTS}
          onChange={(action) => onChange({ ...draft, action })}
        />
      </div>

      <div>
        <div className="text-xs text-muted mb-1">Content scan sensitivity</div>
        <Segmented
          ariaLabel={`${title} scan sensitivity`}
          value={draft.scan}
          options={SCAN_OPTS}
          onChange={(scan) => onChange({ ...draft, scan })}
        />
      </div>
    </div>
  );
}

export function ProtectionEditor({
  email,
  config,
  onSaved,
}: {
  email: string;
  config: ProtectionConfig;
  onSaved: () => void;
}) {
  const [inbound, setInbound] = useState(() => directionFromConfig(config.inbound));
  const [outbound, setOutbound] = useState(() => directionFromConfig(config.outbound));
  const [ttl, setTTL] = useState(config.holds.ttl_seconds ?? 604800);
  const [onExpiry, setOnExpiry] = useState<"approve" | "reject">(
    config.holds.on_expiry ?? "reject",
  );
  const [baseline, setBaseline] = useState(() => ({
    inbound: directionFromConfig(config.inbound),
    outbound: directionFromConfig(config.outbound),
    ttl: config.holds.ttl_seconds ?? 604800,
    onExpiry: (config.holds.on_expiry ?? "reject") as "approve" | "reject",
  }));
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const [saved, setSaved] = useState(false);

  useEffect(() => {
    setBaseline({
      inbound: directionFromConfig(config.inbound),
      outbound: directionFromConfig(config.outbound),
      ttl: config.holds.ttl_seconds ?? 604800,
      onExpiry: (config.holds.on_expiry ?? "reject") as "approve" | "reject",
    });
  }, [config]);

  const hasEdits =
    isDirectionDirty(inbound, baseline.inbound) ||
    isDirectionDirty(outbound, baseline.outbound) ||
    ttl !== baseline.ttl ||
    onExpiry !== baseline.onExpiry;

  const ttlIsPreset = TTL_PRESETS.some((p) => p.seconds === ttl);

  const handleSave = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!hasEdits || saving) return;
    if (ttl <= 0 || ttl > MAX_TTL) {
      setError(`Approval window must be between 1 and ${MAX_TTL} seconds (7 days).`);
      return;
    }
    setSaving(true);
    setError("");
    setSaved(false);
    try {
      await setProtection(email, {
        inbound: directionToConfig(inbound),
        outbound: directionToConfig(outbound),
        holds: { ttl_seconds: ttl, on_expiry: onExpiry },
      });
      setBaseline({
        inbound,
        outbound,
        ttl,
        onExpiry,
      });
      setSaved(true);
      onSaved();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to update protection");
    } finally {
      setSaving(false);
    }
  };

  return (
    <form
      onSubmit={handleSave}
      className="mb-6 space-y-4"
      style={{
        background: "var(--bg-panel)",
        border: "1px solid var(--border)",
        borderRadius: "var(--r-lg)",
        padding: "20px 22px",
      }}
    >
      <div className="flex flex-col sm:flex-row sm:items-start sm:justify-between gap-4">
        <div>
          <div className="flex items-center gap-2">
            <Eyebrow>Protection</Eyebrow>
            <Chip tone="warn">Beta</Chip>
          </div>
          <p
            className="mt-2 text-[13px] leading-relaxed"
            style={{
              color: "var(--fg-muted)",
              maxWidth: 580,
            }}
          >
            Control who may send to and from this inbox, how aggressively content
            is scanned, and what happens to messages held for review.
          </p>
        </div>
        <div className="flex flex-col items-end gap-1.5 shrink-0 self-start">
          <div className="flex items-center gap-2">
            {saved && <span className="text-xs text-success font-medium">Saved ✓</span>}
            <button
              type="submit"
              disabled={saving || !hasEdits}
              className="text-xs px-3 py-1.5 bg-foreground text-background rounded-md hover:opacity-90 transition disabled:opacity-40 disabled:cursor-not-allowed cursor-pointer font-medium"
            >
              {saving ? "Saving…" : "Save"}
            </button>
          </div>
          {error && <p className="text-xs text-red-600 max-w-[280px] text-right">{error}</p>}
        </div>
      </div>

      <DirectionFields
        title="Inbound"
        gateLabel="Who may send to this inbox"
        draft={inbound}
        onChange={(d) => { setInbound(d); setSaved(false); }}
      />
      <DirectionFields
        title="Outbound"
        gateLabel="Who this inbox may send to"
        draft={outbound}
        onChange={(d) => { setOutbound(d); setSaved(false); }}
      />

      {/* Review queue (holds) — what happens to messages a gate or scan
          decides to hold. */}
      <div className="space-y-2 border border-border rounded-md p-3">
        <div className="text-xs font-semibold text-foreground">Review queue</div>
        <div>
          <div className="text-xs text-muted mb-1">Approval window</div>
          <div className="flex items-center gap-1 flex-wrap">
            {TTL_PRESETS.map((p) => (
              <button
                key={p.label}
                type="button"
                aria-pressed={ttl === p.seconds}
                onClick={() => { setTTL(p.seconds); setSaved(false); }}
                className={`text-xs px-2 py-1 rounded-md border transition ${
                  ttl === p.seconds
                    ? "bg-foreground text-background border-foreground"
                    : "border-border hover:bg-surface"
                }`}
              >
                {p.label}
              </button>
            ))}
            <label className="flex items-center gap-1 text-xs">
              <span
                className={`px-2 py-1 rounded-md border ${
                  ttlIsPreset ? "border-border" : "bg-foreground text-background border-foreground"
                }`}
              >
                custom
              </span>
              <input
                type="number"
                min={1}
                max={MAX_TTL}
                value={ttl}
                onChange={(e) => { setTTL(parseInt(e.target.value, 10) || 0); setSaved(false); }}
                className="w-24 text-xs px-2 py-1 border border-border rounded-md"
                aria-label="Approval window in seconds"
              />
              <span className="text-muted">sec</span>
            </label>
          </div>
        </div>

        <div>
          <div className="text-xs text-muted mb-1">
            If no action is taken before the window closes
          </div>
          <div className="flex items-center gap-2">
            <label className="flex items-center gap-1.5 text-xs">
              <input
                type="radio"
                name={`holds-on-expiry-${email}`}
                value="reject"
                checked={onExpiry === "reject"}
                onChange={() => { setOnExpiry("reject"); setSaved(false); }}
              />
              <span>Auto-reject (discard)</span>
            </label>
            <label className="flex items-center gap-1.5 text-xs">
              <input
                type="radio"
                name={`holds-on-expiry-${email}`}
                value="approve"
                checked={onExpiry === "approve"}
                onChange={() => { setOnExpiry("approve"); setSaved(false); }}
              />
              <span>Auto-approve (send)</span>
            </label>
          </div>
        </div>
      </div>

      <p className="text-[11px] text-muted leading-snug">
        Gates decide who may send; the scan flags risky content. A non-match
        or flagged message is delivered, held for review, or blocked per the
        action above. Held messages and their body + attachments remain in
        message history after the review reaches a terminal state.
      </p>
    </form>
  );
}
