"use client";

import { useState } from "react";
import { Field } from "../../../components/Field";
import { registerDomain } from "../../../components/onboarding/api";
import { canReceive, isValidDomain } from "../../../components/onboarding/state";
import { track } from "../../../components/onboarding/analytics";
import type { DomainInfo } from "../../../components/onboarding/types";

export function DomainSelector({
  existingDomains,
  onSelected,
}: {
  existingDomains: DomainInfo[];
  onSelected: (domain: DomainInfo) => void;
}) {
  const [showNewForm, setShowNewForm] = useState(existingDomains.length === 0);
  const [newDomain, setNewDomain] = useState("");
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");

  const handleRegister = async (e: React.FormEvent) => {
    e.preventDefault();
    setError("");

    if (!isValidDomain(newDomain)) {
      setError("Enter a valid domain (e.g. mail.yourcompany.com)");
      return;
    }

    setLoading(true);
    track("domain_registration_started", { domain: newDomain });
    try {
      const domain = await registerDomain(newDomain);
      track("domain_registration_succeeded", { domain: newDomain });
      onSelected(domain);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Registration failed");
    } finally {
      setLoading(false);
    }
  };

  return (
    <div>
      <h2
        className="mb-2"
        style={{
          fontFamily: "var(--f-ui)",
          fontWeight: 600,
          fontSize: 30,
          letterSpacing: "-0.01em",
          color: "var(--fg)",
        }}
      >
        Choose a domain
      </h2>
      <p className="mb-7 text-[14px]" style={{ color: "var(--fg-muted)" }}>
        Select an existing domain or add a new one.
      </p>

      {/* Existing domains */}
      {existingDomains.length > 0 && (
        <div className="space-y-2 mb-6">
          {existingDomains.map((d) => (
            <button
              key={d.domain}
              type="button"
              onClick={() => onSelected(d)}
              className="w-full text-left p-4 rounded-lg border border-border hover:border-foreground/20 transition flex flex-col sm:flex-row sm:items-center sm:justify-between gap-2"
            >
              <div className="min-w-0">
                <code className="text-sm font-mono font-medium break-all">{d.domain}</code>
                <span
                  className={`ml-2 inline-flex items-center px-1.5 py-0.5 rounded text-[10px] font-medium ${
                    canReceive(d)
                      ? "bg-green-100 text-green-700"
                      : "bg-amber-100 text-amber-700"
                  }`}
                >
                  {canReceive(d) ? "Verified" : "Unverified"}
                </span>
              </div>
              <span className="text-xs text-muted shrink-0">Select</span>
            </button>
          ))}
        </div>
      )}

      {/* Toggle new domain form */}
      {existingDomains.length > 0 && !showNewForm && (
        <button
          type="button"
          onClick={() => setShowNewForm(true)}
          className="text-sm text-accent hover:underline mb-6"
        >
          + Add a new domain
        </button>
      )}

      {/* New domain form */}
      {showNewForm && (
        <>
          {error && (
            <div className="mb-4 p-3 bg-red-50 border border-red-200 rounded-lg text-sm text-red-700">
              {error}
            </div>
          )}
          <form onSubmit={handleRegister} className="space-y-4">
            <Field
              label="Domain"
              placeholder="mail.yourcompany.com"
              value={newDomain}
              onChange={(v) => setNewDomain(v.toLowerCase())}
              hint="Use a subdomain you control. All emails to *@this-domain will be routed to e2a."
            />
            <button
              type="submit"
              disabled={loading || !newDomain}
              className="w-full bg-foreground text-background py-2.5 rounded-lg text-sm font-medium hover:opacity-90 transition disabled:opacity-50 disabled:cursor-not-allowed"
            >
              {loading ? "Registering..." : "Register domain"}
            </button>
          </form>
        </>
      )}
    </div>
  );
}
