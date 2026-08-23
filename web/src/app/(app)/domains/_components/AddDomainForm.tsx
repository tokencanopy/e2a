"use client";

import { useState } from "react";
import { Field } from "../../../components/Field";
import { isValidDomain } from "../../../components/onboarding/state";
import { registerDomain } from "../../../components/onboarding/api";
import { track } from "../../../components/onboarding/analytics";

export function AddDomainForm({
  onRegistered,
}: {
  onRegistered: () => void;
}) {
  const [domain, setDomain] = useState("");
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setError("");

    if (!isValidDomain(domain)) {
      setError("Enter a valid domain (e.g. mail.yourcompany.com)");
      return;
    }

    setLoading(true);
    track("domain_registration_started", { domain });
    try {
      await registerDomain(domain);
      track("domain_registration_succeeded", { domain });
      setDomain("");
      onRegistered();
    } catch (err) {
      setError(
        err instanceof Error ? err.message : "Failed to register domain",
      );
    } finally {
      setLoading(false);
    }
  };

  return (
    <form
      onSubmit={handleSubmit}
      className="p-5 space-y-4"
      style={{
        background: "var(--bg-panel)",
        border: "1px solid var(--border)",
        borderRadius: "var(--r-lg)",
      }}
    >
      <p
        className="text-[14px] font-semibold"
        style={{ color: "var(--fg)" }}
      >
        Add a new domain
      </p>
      {error && (
        <div
          className="p-3 text-[13px]"
          style={{
            background: "var(--danger-bg)",
            color: "var(--danger-strong)",
            border: "1px solid var(--danger-bg)",
            borderRadius: "var(--r-md)",
          }}
        >
          {error}
        </div>
      )}
      <Field
        label="Domain"
        placeholder="mail.yourcompany.com"
        value={domain}
        onChange={(v) => setDomain(v.toLowerCase())}
        hint="Use a subdomain you control. All emails to *@this-domain will be routed to e2a."
      />
      <button
        type="submit"
        disabled={loading || !domain}
        className="w-full py-2.5 text-[13px] font-medium transition disabled:opacity-50 disabled:cursor-not-allowed"
        style={{
          background: "var(--accent-fill)",
          color: "var(--accent-fg)",
          borderRadius: "var(--r-md)",
        }}
      >
        {loading ? "Registering..." : "Register domain"}
      </button>
    </form>
  );
}
