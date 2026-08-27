"use client";

import { useEffect, useRef } from "react";
import { DNSRecord as DNSRecordField } from "../../../components/DnsRecord";
import type {
  DomainInfo,
  DNSRecord,
  DNSRecordPurpose,
} from "../../../components/onboarding/types";
import { track } from "../../../components/onboarding/analytics";

// Human label per record purpose for the onboarding paste screen. Open set —
// an unknown purpose falls back to its raw value rather than disappearing.
const PURPOSE_LABEL: Record<DNSRecordPurpose, string> = {
  ownership: "Prove domain ownership",
  inbound_mx: "Route email to e2a",
  inbound_mx_wildcard: "Route email for all subdomains",
  dkim: "Authenticate outbound mail (DKIM)",
  mail_from_mx: "Return path for bounces (MAIL FROM)",
  mail_from_spf: "Authorize sending (SPF)",
};

function recordFields(rec: DNSRecord) {
  if (rec.type.toUpperCase() === "MX") {
    return [
      { label: "Name", value: rec.name },
      { label: "Mail server", value: rec.value },
      { label: "Priority", value: String(rec.priority ?? 10) },
    ];
  }
  return [
    { label: "Name", value: rec.name },
    { label: "Content", value: rec.value },
  ];
}

export function DNSSetupCard({
  domain,
}: {
  domain: DomainInfo;
}) {
  const tracked = useRef(false);
  useEffect(() => {
    if (!tracked.current) {
      track("dns_instructions_viewed", { domain: domain.domain });
      tracked.current = true;
    }
  }, [domain.domain]);

  return (
    <div>
      <h2
        className="mb-2"
        style={{
          fontFamily: "var(--f-ui)",
          fontWeight: 600,
          fontSize: 28,
          letterSpacing: "-0.01em",
          color: "var(--fg)",
        }}
      >
        Configure DNS records
      </h2>
      <p className="mb-7 text-[14px]" style={{ color: "var(--fg-muted)" }}>
        Add these records to{" "}
        <code
          className="font-mono text-[12px] px-1.5 py-0.5 break-all"
          style={{
            background: "var(--bg-elev)",
            border: "1px solid var(--border-sub)",
            borderRadius: "var(--r-sm)",
            color: "var(--fg)",
          }}
        >
          {domain.domain}
        </code>
        &apos;s DNS to prove ownership and route email to e2a.
      </p>
      <div className="space-y-6">
        {(domain.dns_records ?? []).map((rec, i) => (
          <DNSRecordField
            key={`${rec.purpose}-${rec.name}-${i}`}
            type={rec.type}
            label={
              PURPOSE_LABEL[rec.purpose as DNSRecordPurpose] ?? rec.purpose
            }
            fields={recordFields(rec)}
          />
        ))}
      </div>
      <div
        className="mt-6 p-4 text-[13px]"
        style={{
          background: "var(--warn-bg)",
          color: "var(--warn-strong)",
          border: "1px solid var(--warn-bg)",
          borderRadius: "var(--r-md)",
        }}
      >
        DNS changes can take a few minutes to propagate. Wait a bit before verifying.
      </div>
      <div
        className="mt-4 p-4 text-[13px]"
        style={{
          background: "var(--bg-elev)",
          color: "var(--fg-muted)",
          border: "1px solid var(--border-sub)",
          borderRadius: "var(--r-md)",
        }}
      >
        <strong style={{ color: "var(--fg)" }}>Cloudflare managed?</strong> Add Cloudflare MCP to your AI coding agent so it can set up all DNS records automatically.
      </div>
    </div>
  );
}
