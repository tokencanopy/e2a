"use client";

import { useState } from "react";
import { DNSRecord as DNSRecordField } from "../../../components/Field";
import { Chip, Dot } from "@e2a/ui";
import {
  verifyDomain,
  deleteDomain,
} from "../../../components/onboarding/api";
import type {
  DomainInfo,
  DomainCapabilityStatus,
  DNSRecord,
  DNSRecordPurpose,
  DNSRecordStatus,
  VerifyDomainResponse,
} from "../../../components/onboarding/types";
import {
  canReceive,
  outboundCapability,
} from "../../../components/onboarding/state";
import { track } from "../../../components/onboarding/analytics";

// Per-purpose display metadata. `group` decides whether a record lands in the
// inbound "DNS records" section or the "Outbound sending" section. Unknown
// purposes (open set) fall back to the inbound group + their raw purpose label,
// so a future record kind renders generically instead of disappearing.
const PURPOSE_META: Record<
  DNSRecordPurpose,
  { label: string; group: "inbound" | "sending" }
> = {
  ownership: {
    label: "Prove domain ownership (also drives SPF check)",
    group: "inbound",
  },
  inbound_mx: { label: "Route email to e2a", group: "inbound" },
  inbound_mx_wildcard: {
    label: "Route email for all subdomains",
    group: "inbound",
  },
  dkim: { label: "Authenticate outbound mail (DKIM)", group: "inbound" },
  mail_from_mx: {
    label: "Return path for bounces (MAIL FROM)",
    group: "sending",
  },
  mail_from_spf: { label: "Authorize sending (SPF)", group: "sending" },
};

// Maps a record purpose onto the matching field of the live verify probe so the
// probe's found/missing result can be overlaid onto that record's row. Only the
// inbound_mx + dkim purposes have a directly comparable live probe; the mail_from
// records are verified by SES as a unit (surfaced via sending_status), not by
// this DNS probe.
//
// ownership is deliberately NOT mapped: its probe counterpart is the SPF field,
// which checks for a `v=spf1` record — a SEPARATE, optional TXT from the
// `e2a-verify=` ownership token. Overlaying it made a verified domain whose owner
// hadn't published SPF show the ownership row as "Missing". The ownership row now
// falls back to its server-derived status, which already reflects whether the
// ownership TXT is present (verified once the domain verifies).
const PROBE_FIELD_BY_PURPOSE: Partial<
  Record<DNSRecordPurpose, "mx" | "spf" | "dkim">
> = {
  inbound_mx: "mx",
  dkim: "dkim",
};

function purposeMeta(purpose: string): { label: string; group: "inbound" | "sending" } {
  return (
    PURPOSE_META[purpose as DNSRecordPurpose] ?? {
      label: purpose,
      group: "inbound",
    }
  );
}

// Renders a found/missing/deferred chip from the most recent live verify probe.
// "deferred" only appears on legacy pre-migration domains with no stored DKIM
// keypair. Used to OVERLAY the per-record status when a probe result exists.
function ProbeStatusChip({
  status,
}: {
  status: "found" | "missing" | "deferred" | undefined;
}) {
  if (!status) return null;
  if (status === "found")
    return (
      <Chip tone="success">
        <Dot tone="success" />
        Found
      </Chip>
    );
  if (status === "deferred")
    return <Chip tone="neutral">No key registered</Chip>;
  return (
    <Chip tone="warn">
      <Dot tone="warn" />
      Missing
    </Chip>
  );
}

// Renders the per-record status carried by the unified dns_records array
// (purpose-tagged, server-derived). Open set — an unknown status falls through
// to a neutral chip showing the raw value rather than crashing.
function RecordStatusChip({ status }: { status: DNSRecordStatus }) {
  if (status === "verified")
    return (
      <Chip tone="success">
        <Dot tone="success" />
        Verified
      </Chip>
    );
  if (status === "failed")
    return (
      <Chip tone="danger">
        <Dot tone="danger" />
        Failed
      </Chip>
    );
  if (status === "missing")
    return (
      <Chip tone="warn">
        <Dot tone="warn" />
        Missing
      </Chip>
    );
  if (status === "pending") return <Chip tone="info">Pending</Chip>;
  return <Chip tone="neutral">{status}</Chip>;
}

// SendingStatusChip reflects the async SES sending-identity verification for the
// WHOLE domain (the rollup over the dkim + mail_from records), shown once in the
// section header. Mirrors the `sending_status` column. Unknown values fall
// through to the in-progress label (open set).
function SendingStatusChip({
  status,
}: {
  status: DomainCapabilityStatus | undefined;
}) {
  if (status === "verified")
    return (
      <Chip tone="success">
        <Dot tone="success" />
        Sending enabled
      </Chip>
    );
  if (status === "failed")
    return (
      <Chip tone="danger">
        <Dot tone="danger" />
        Failed
      </Chip>
    );
  return <Chip tone="info">Verifying…</Chip>;
}

// Header chip for the OUTBOUND axis, so a domain's send capability is legible
// without expanding the DNS section — the two axes are independent, and a domain
// can be inbound-verified while outbound is still pending or has failed.
//
// Deliberately worded differently from SendingStatusChip (the rollup chip inside
// the DNS section): both can be on screen at once, and distinct copy keeps them
// individually addressable rather than rendering the same string twice.
function OutboundCapabilityChip({
  status,
}: {
  status: DomainCapabilityStatus;
}) {
  if (status === "verified")
    return (
      <Chip tone="success">
        <Dot tone="success" />
        Outbound ready
      </Chip>
    );
  if (status === "failed")
    return (
      <Chip tone="danger">
        <Dot tone="danger" />
        Outbound failed
      </Chip>
    );
  return <Chip tone="info">Outbound pending</Chip>;
}

// One DNS record row: a type badge + purpose label + status chip, then the
// copyable field block. MX records split priority into its own field (the value
// is the bare mail-server host); TXT records show name + content. When a live
// probe result is supplied it overlays the server-derived per-record status.
//
// showStatus is false for the mail_from rows: SES verifies them as a unit
// (all-or-nothing), so there is no meaningful per-record signal — the section's
// rollup chip carries it instead, and a per-row chip would be redundant noise.
function RecordRow({
  record,
  probeStatus,
  showStatus = true,
}: {
  record: DNSRecord;
  probeStatus?: "found" | "missing" | "deferred";
  showStatus?: boolean;
}) {
  const isMX = record.type.toUpperCase() === "MX";
  return (
    <div className="space-y-2">
      <div className="flex items-center gap-2 flex-wrap">
        <span
          className="font-mono text-[11px] px-2 py-0.5"
          style={{
            background: "var(--bg-elev)",
            border: "1px solid var(--border-sub)",
            borderRadius: "var(--r-sm)",
            color: "var(--fg)",
            minWidth: 36,
            textAlign: "center",
          }}
        >
          {record.type}
        </span>
        <span className="text-[12px]" style={{ color: "var(--fg-muted)" }}>
          {purposeMeta(record.purpose).label}
        </span>
        <span className="flex-1" />
        {showStatus &&
          (probeStatus ? (
            <ProbeStatusChip status={probeStatus} />
          ) : (
            <RecordStatusChip status={record.status} />
          ))}
      </div>
      <DNSRecordField
        type=""
        label=""
        fields={
          isMX
            ? [
                { label: "Name", value: record.name },
                { label: "Mail server", value: record.value },
                { label: "Priority", value: String(record.priority ?? 10) },
              ]
            : [
                { label: "Name", value: record.name },
                { label: "Content", value: record.value },
              ]
        }
      />
    </div>
  );
}

export function DomainCard({
  domain,
  agentCount,
  onVerified,
  onDeleted,
}: {
  domain: DomainInfo;
  agentCount: number;
  onVerified: () => void;
  onDeleted: () => void;
}) {
  const [showDNS, setShowDNS] = useState(false);
  const [verifying, setVerifying] = useState(false);
  const [verifyError, setVerifyError] = useState("");
  const [deleting, setDeleting] = useState(false);
  const [deleteError, setDeleteError] = useState("");
  // Cached per-record diagnostic from the most recent verify probe. Until the
  // user re-checks, this is null and the rows fall back to the server-derived
  // per-record status carried in dns_records.
  const [probe, setProbe] = useState<VerifyDomainResponse | null>(null);

  // Split the unified records into the two display groups. mail_from records
  // are present only when the sending feature is enabled server-side, so the
  // sending section is naturally absent when the feature is off.
  const records = domain.dns_records ?? [];
  const inboundRecords = records.filter(
    (r) => purposeMeta(r.purpose).group === "inbound",
  );
  const sendingRecords = records.filter(
    (r) => purposeMeta(r.purpose).group === "sending",
  );

  // Both capability axes, read through the helpers so they prefer the backend's
  // `capabilities` object and fall back to the legacy verified/sending_status
  // pair. The outbound chip appears only when the sending feature is live
  // server-side — detected by the presence of the deterministic mail_from rows,
  // the same gate the sending section below uses — so a deployment with sending
  // off renders exactly as it did before.
  const inboundReady = canReceive(domain);
  const outbound = outboundCapability(domain);
  const sendingFeatureLive = sendingRecords.length > 0;

  const handleVerify = async () => {
    setVerifyError("");
    setVerifying(true);
    track("domain_verify_attempted", { domain: domain.domain });
    try {
      const result = await verifyDomain(domain.domain);
      setProbe(result);
      track("domain_verify_succeeded", { domain: domain.domain });
      onVerified();
    } catch (err) {
      track("domain_verify_failed", { domain: domain.domain });
      setVerifyError(
        err instanceof Error ? err.message : "Verification failed",
      );
    } finally {
      setVerifying(false);
    }
  };

  const handleDelete = async () => {
    if (!confirm(`Delete domain ${domain.domain}? This cannot be undone.`))
      return;
    setDeleteError("");
    setDeleting(true);
    try {
      await deleteDomain(domain.domain);
      onDeleted();
    } catch (err) {
      setDeleteError(
        err instanceof Error ? err.message : "Failed to delete domain",
      );
    } finally {
      setDeleting(false);
    }
  };

  // Overlay the live probe's found/missing onto an inbound/dkim record by
  // purpose. Returns undefined for records with no probe field (mail_from_*) or
  // when no probe has run yet — the row then shows its server-derived status.
  const probeFor = (
    record: DNSRecord,
  ): "found" | "missing" | "deferred" | undefined => {
    const field = PROBE_FIELD_BY_PURPOSE[record.purpose as DNSRecordPurpose];
    if (!field || !probe) return undefined;
    return probe[field];
  };

  return (
    <div
      style={{
        background: "var(--bg-panel)",
        border: "1px solid var(--border)",
        borderRadius: "var(--r-lg)",
        padding: "20px 22px",
      }}
    >
      {/* Header row */}
      <div className="flex items-start justify-between flex-wrap gap-3">
        <div className="min-w-0">
          <div className="flex items-center gap-2 mb-2 flex-wrap">
            <code
              className="font-mono text-[14px] font-semibold px-2 py-0.5 break-all"
              style={{
                color: "var(--fg)",
                background: "var(--bg-elev)",
                border: "1px solid var(--border-sub)",
                borderRadius: "var(--r-sm)",
                maxWidth: "100%",
                minWidth: 0,
              }}
            >
              {domain.domain}
            </code>
            <Chip tone={inboundReady ? "success" : "warn"}>
              <Dot tone={inboundReady ? "success" : "warn"} />
              {inboundReady ? "Verified" : "Unverified"}
            </Chip>
            {sendingFeatureLive && <OutboundCapabilityChip status={outbound} />}
          </div>
          <p
            className="text-[12px]"
            style={{ color: "var(--fg-muted)" }}
          >
            {agentCount === 0
              ? "No inboxes"
              : agentCount === 1
                ? "1 inbox"
                : `${agentCount} inboxes`}
          </p>
          <p
            className="font-mono text-[11px] mt-0.5"
            style={{
              color: "var(--fg-subtle)",
              letterSpacing: "0.02em",
            }}
          >
            Added {new Date(domain.created_at).toLocaleDateString()}
            {domain.verified_at && (
              <>
                {" · verified "}
                {new Date(domain.verified_at).toLocaleDateString()}
              </>
            )}
            {domain.last_checked_at && (
              <>
                {" · last checked "}
                {new Date(domain.last_checked_at).toLocaleDateString()}
              </>
            )}
          </p>
        </div>
        <div className="flex gap-2 shrink-0 flex-wrap">
          {inboundReady ? (
            <a
              href={`/get-started?domain=${encodeURIComponent(domain.domain)}`}
              className="text-[12px] px-3 py-1.5 font-medium transition"
              style={{
                background: "var(--accent-fill)",
                color: "var(--accent-fg)",
                borderRadius: "var(--r-md)",
              }}
            >
              Create inbox
            </a>
          ) : (
            <button
              onClick={handleVerify}
              disabled={verifying}
              className="text-[12px] px-3 py-1.5 font-medium transition disabled:opacity-50"
              style={{
                background: "var(--fg)",
                color: "var(--bg)",
                borderRadius: "var(--r-md)",
              }}
            >
              {verifying ? "Verifying..." : "Verify domain"}
            </button>
          )}
          <button
            onClick={handleDelete}
            disabled={deleting}
            className="text-[12px] px-3 py-1.5 transition disabled:opacity-50"
            style={{
              color: "var(--danger-strong)",
              border: "1px solid var(--danger-bg)",
              background: "transparent",
              borderRadius: "var(--r-md)",
            }}
          >
            Delete
          </button>
        </div>
      </div>

      {/* Verify error */}
      {verifyError && (
        <div
          className="mt-3 p-3 text-[13px]"
          style={{
            background: "var(--danger-bg)",
            color: "var(--danger-strong)",
            border: "1px solid var(--danger-bg)",
            borderRadius: "var(--r-md)",
          }}
        >
          {verifyError}
          <p className="mt-1 text-[11px]">
            DNS changes can take a few minutes to propagate. Wait a bit and
            try again.
          </p>
        </div>
      )}

      {deleteError && (
        <div
          className="mt-3 p-3 text-[13px]"
          role="alert"
          style={{
            background: "var(--danger-bg)",
            color: "var(--danger-strong)",
            border: "1px solid var(--danger-bg)",
            borderRadius: "var(--r-md)",
          }}
        >
          {deleteError}
        </div>
      )}

      {/* DNS toggle */}
      <div className="mt-3">
        <button
          onClick={() => setShowDNS(!showDNS)}
          className="text-[12px] transition flex items-center gap-1"
          style={{ color: "var(--fg-muted)" }}
        >
          {showDNS ? "Hide" : "View"} DNS records
          <span className="text-[10px]">
            {showDNS ? "▲" : "▼"}
          </span>
        </button>
      </div>

      {/* DNS records — rendered generically from the unified, purpose-tagged
          dns_records array. Inbound rows (ownership, inbound_mx, dkim) overlay
          the most recent live verify probe's found/missing onto their
          server-derived status; sending rows follow sending_status. */}
      {showDNS && (
        <div
          className="mt-3 pt-4 space-y-4"
          style={{ borderTop: "1px solid var(--border)" }}
        >
          <div className="flex items-center justify-between gap-2 flex-wrap">
            <p
              className="font-mono text-[10px] uppercase font-semibold"
              style={{ color: "var(--fg-subtle)", letterSpacing: "0.08em" }}
            >
              DNS records
              {(probe || domain.last_checked_at) && (
                <span
                  className="ml-2 font-normal normal-case"
                  style={{ color: "var(--fg-subtle)", letterSpacing: "0.02em" }}
                >
                  · last checked{" "}
                  {new Date(
                    domain.last_checked_at || Date.now(),
                  ).toLocaleString()}
                </span>
              )}
            </p>
            <button
              onClick={handleVerify}
              disabled={verifying}
              className="text-[11px] px-2 py-0.5 transition disabled:opacity-50"
              style={{
                color: "var(--fg-muted)",
                border: "1px solid var(--border-sub)",
                background: "var(--bg-elev)",
                borderRadius: "var(--r-sm)",
              }}
            >
              {verifying ? "Probing…" : "Re-check"}
            </button>
          </div>

          {inboundRecords.map((rec, i) => (
            <RecordRow
              key={`${rec.purpose}-${rec.name}-${i}`}
              record={rec}
              probeStatus={probeFor(rec)}
            />
          ))}

          {/* Outbound sending (decision 4 / Slice 4). Present only when the
              sending feature is enabled server-side (ses_region set), in which
              case the deterministic mail_from records arrive at register time —
              so when the feature is off, this whole block is absent and the
              card is unchanged. SES verifies these as a unit, surfaced by the
              single rollup chip; each row also shows its server-derived status. */}
          {sendingRecords.length > 0 && (
            <div
              className="space-y-3 pt-4"
              style={{ borderTop: "1px solid var(--border)" }}
            >
              <div className="flex items-center gap-2 flex-wrap">
                <p
                  className="font-mono text-[10px] uppercase font-semibold"
                  style={{
                    color: "var(--fg-subtle)",
                    letterSpacing: "0.08em",
                  }}
                >
                  Outbound sending
                </p>
                <SendingStatusChip status={outbound} />
              </div>
              <p className="text-[12px]" style={{ color: "var(--fg-muted)" }}>
                Publish these so mail sends as{" "}
                <code className="font-mono">@{domain.domain}</code> with no
                “via e2a”. The DKIM record above is also required.
              </p>

              {outbound === "failed" && domain.sending_error && (
                <div
                  className="p-3 text-[12px]"
                  style={{
                    background: "var(--danger-bg)",
                    color: "var(--danger-strong)",
                    border: "1px solid var(--danger-bg)",
                    borderRadius: "var(--r-md)",
                  }}
                >
                  {domain.sending_error}
                </div>
              )}

              {sendingRecords.map((rec, i) => (
                <RecordRow
                  key={`${rec.purpose}-${rec.name}-${i}`}
                  record={rec}
                  showStatus={false}
                />
              ))}
            </div>
          )}

          {!inboundReady && (
            <div
              className="p-3 text-[12px]"
              style={{
                background: "var(--warn-bg)",
                color: "var(--warn-strong)",
                border: "1px solid var(--warn-bg)",
                borderRadius: "var(--r-md)",
              }}
            >
              DNS changes can take a few minutes to propagate. Wait a bit
              before verifying.
            </div>
          )}
        </div>
      )}
    </div>
  );
}
