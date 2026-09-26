"use client";

import { useState } from "react";
import { useAuth } from "../../components/AuthProvider";
import { PageShell } from "../../components/loft/PageShell";
import { readApiError } from "../../../lib/accountDeletion";
import { hardNavigate } from "../../../lib/navigation";

export default function SettingsPage() {
  const { user } = useAuth();

  if (!user) return null;

  return (
    <PageShell
      eyebrow="Account"
      title={<>Settings</>}
      subtitle="Account profile, data export, and account deletion."
      maxWidth={920}
    >
      <div className="space-y-12">
        <ProfileSection user={user} />
        <ExportSection />
        <DangerZone />
      </div>
    </PageShell>
  );
}

function SectionHeading({
  title,
  subtitle,
  tone = "default",
}: {
  title: string;
  subtitle?: React.ReactNode;
  tone?: "default" | "danger";
}) {
  return (
    <div className="mb-4">
      <h2
        className="mb-1"
        style={{
          fontFamily: "var(--f-ui)",
          fontWeight: 600,
          fontSize: 18,
          letterSpacing: "-0.01em",
          color: tone === "danger" ? "var(--danger-strong)" : "var(--fg)",
        }}
      >
        {title}
      </h2>
      {subtitle && (
        <p
          className="text-[13px] leading-[1.6] max-w-2xl"
          style={{ color: "var(--fg-muted)" }}
        >
          {subtitle}
        </p>
      )}
    </div>
  );
}

function ProfileSection({
  user,
}: {
  user: { id: string; email: string; name: string; created_at: string };
}) {
  const { setUser } = useAuth();
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState(user.name);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");

  const handleSave = async () => {
    setError("");
    setSaving(true);
    try {
      const res = await fetch("/api/auth/me", {
        method: "PATCH",
        headers: { "Content-Type": "application/json" },
        credentials: "include",
        body: JSON.stringify({ name: draft }),
      });
      if (!res.ok) {
        setError((await res.text()) || `Failed (${res.status})`);
        setSaving(false);
        return;
      }
      const updated = await res.json();
      setUser(updated);
      setEditing(false);
    } catch {
      setError("Network error");
    } finally {
      setSaving(false);
    }
  };

  return (
    <section>
      <SectionHeading title="Profile" />
      <div
        className="p-5"
        style={{
          background: "var(--bg-panel)",
          border: "1px solid var(--border)",
          borderRadius: "var(--r-lg)",
        }}
      >
        <dl className="grid grid-cols-1 sm:grid-cols-[140px_1fr] gap-y-3 sm:gap-x-6 text-[13px]">
          <dt style={{ color: "var(--fg-muted)" }}>Name</dt>
          <dd className="flex items-center gap-2 flex-wrap" style={{ color: "var(--fg)" }}>
            {editing ? (
              <>
                <input
                  type="text"
                  value={draft}
                  onChange={(e) => setDraft(e.target.value)}
                  disabled={saving}
                  maxLength={80}
                  className="text-[13px] px-2 py-1 flex-1 sm:flex-none sm:min-w-[200px]"
                  style={{
                    background: "var(--bg-elev)",
                    border: "1px solid var(--border)",
                    borderRadius: "var(--r-sm)",
                    color: "var(--fg)",
                  }}
                />
                <button
                  type="button"
                  onClick={handleSave}
                  disabled={saving || draft.trim().length === 0 || draft !== draft.trim()}
                  className="text-[11px] px-2 py-0.5 disabled:opacity-50 disabled:cursor-not-allowed"
                  style={{
                    color: "var(--accent-fg)",
                    background: "var(--accent-fill)",
                    border: "1px solid var(--accent-fill)",
                    borderRadius: "var(--r-sm)",
                  }}
                >
                  {saving ? "Saving…" : "Save"}
                </button>
                <button
                  type="button"
                  onClick={() => {
                    setEditing(false);
                    setDraft(user.name);
                    setError("");
                  }}
                  disabled={saving}
                  className="text-[11px] px-2 py-0.5"
                  style={{
                    color: "var(--fg-muted)",
                    border: "1px solid var(--border-sub)",
                    background: "var(--bg-elev)",
                    borderRadius: "var(--r-sm)",
                  }}
                >
                  Cancel
                </button>
                {error && (
                  <span className="text-[11px]" style={{ color: "var(--danger-strong)" }}>
                    {error}
                  </span>
                )}
              </>
            ) : (
              <>
                <span>{user.name || "—"}</span>
                <button
                  type="button"
                  onClick={() => {
                    setDraft(user.name);
                    setEditing(true);
                  }}
                  className="text-[11px] px-2 py-0.5"
                  style={{
                    color: "var(--fg-muted)",
                    border: "1px solid var(--border-sub)",
                    background: "var(--bg-elev)",
                    borderRadius: "var(--r-sm)",
                  }}
                >
                  Edit
                </button>
              </>
            )}
          </dd>
          <dt style={{ color: "var(--fg-muted)" }}>Email</dt>
          <dd style={{ color: "var(--fg)" }}>{user.email}</dd>
          <dt style={{ color: "var(--fg-muted)" }}>User ID</dt>
          <dd className="font-mono text-[12px]" style={{ color: "var(--fg)" }}>
            {user.id}
          </dd>
          <dt style={{ color: "var(--fg-muted)" }}>Member since</dt>
          <dd style={{ color: "var(--fg)" }}>{formatDate(user.created_at)}</dd>
        </dl>
      </div>
    </section>
  );
}

function ExportSection() {
  return (
    <section>
      <SectionHeading
        title="Your data"
        subtitle="Download a JSON dump of everything we store about you: profile, inboxes, domains, API key metadata, all messages with bodies, and usage events. Internal identifiers (Google subject, key hashes, session tokens) are excluded. Right of access — GDPR Article 15 / CCPA equivalent."
      />
      <a
        href="/v1/account/export"
        className="inline-flex items-center gap-2 px-4 py-2 text-[13px] font-medium transition"
        style={{
          background: "var(--fg)",
          color: "var(--bg)",
          borderRadius: "var(--r-md)",
        }}
      >
        <svg
          width="14"
          height="14"
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          strokeWidth="2"
          strokeLinecap="round"
          strokeLinejoin="round"
          aria-hidden="true"
        >
          <path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4" />
          <polyline points="7 10 12 15 17 10" />
          <line x1="12" y1="15" x2="12" y2="3" />
        </svg>
        Download export
      </a>
    </section>
  );
}

type DeleteState = "idle" | "deleting" | "error";
type DeleteMode = "trash" | "permanent";

// Delete account. DELETE /v1/account?confirm=DELETE moves the account to the
// trash (restorable by signing in again for the trash window); adding
// permanent=true erases it immediately. Erasing is a separate choice with its
// own acknowledgement, never the default.
function DangerZone() {
  const [open, setOpen] = useState(false);
  const [confirmText, setConfirmText] = useState("");
  const [mode, setMode] = useState<DeleteMode>("trash");
  const [acknowledged, setAcknowledged] = useState(false);
  const [state, setState] = useState<DeleteState>("idle");
  const [errorMessage, setErrorMessage] = useState("");

  const permanent = mode === "permanent";
  const ready = confirmText === "DELETE" && (!permanent || acknowledged);

  const reset = () => {
    setOpen(false);
    setConfirmText("");
    setMode("trash");
    setAcknowledged(false);
    setState("idle");
    setErrorMessage("");
  };

  const handleDelete = async () => {
    if (!ready) return;
    setState("deleting");
    setErrorMessage("");
    const url = permanent
      ? "/v1/account?confirm=DELETE&permanent=true"
      : "/v1/account?confirm=DELETE";
    try {
      const res = await fetch(url, {
        method: "DELETE",
        credentials: "include",
      });
      if (!res.ok) {
        const err = await readApiError(res);
        setState("error");
        setErrorMessage(err.message || `HTTP ${res.status}`);
        return;
      }
      // Every session is revoked server-side; a full navigation makes the
      // site re-read that and land signed out.
      hardNavigate("/?account_deleted=1");
    } catch (err) {
      setState("error");
      setErrorMessage(err instanceof Error ? err.message : String(err));
    }
  };

  const busy = state === "deleting";
  const radioLabel = "flex items-start gap-3 p-3 cursor-pointer";

  return (
    <section>
      <SectionHeading title="Danger zone" tone="danger" />
      <div
        className="p-5"
        style={{
          background: "var(--bg-panel)",
          border: "1px solid var(--danger-bg)",
          borderRadius: "var(--r-lg)",
        }}
      >
        <h3
          className="text-[14px] font-semibold mb-1"
          style={{ color: "var(--fg)" }}
        >
          Delete account
        </h3>
        <p
          className="max-w-2xl text-[13px] leading-[1.6]"
          style={{ color: "var(--fg-muted)" }}
        >
          Deleting moves your account to the trash. To restore it, sign in
          again before the trash window ends (30 days by default). Right of
          deletion — GDPR Article 17 / CCPA equivalent.
        </p>
        <ul
          className="mt-3 mb-4 max-w-2xl list-disc pl-5 space-y-1 text-[13px] leading-[1.6]"
          style={{ color: "var(--fg-muted)" }}
        >
          <li>
            Every API key, connected app, and dashboard session is revoked
            right away. Restoring doesn&apos;t bring them back.
          </li>
          <li>Your inboxes move to the trash and stop sending.</li>
          <li>
            Custom domains are unverified. You&apos;ll re-verify them after
            restoring.
          </li>
          <li>
            When the trash window ends, everything is erased for good. The
            identity you signed in with may then be held for a while and
            can&apos;t register a new account until it&apos;s released.
          </li>
        </ul>
        {!open ? (
          <button
            onClick={() => setOpen(true)}
            className="px-4 py-2 text-[13px] font-medium transition"
            style={{
              background: "var(--bg-panel)",
              color: "var(--danger-strong)",
              border: "1px solid var(--danger-bg)",
              borderRadius: "var(--r-md)",
            }}
          >
            Delete account…
          </button>
        ) : (
          <div className="space-y-4">
            <fieldset className="max-w-2xl" disabled={busy}>
              <legend className="text-[13px] font-medium mb-2" style={{ color: "var(--fg)" }}>
                How to delete
              </legend>
              <div
                className="overflow-hidden"
                style={{ border: "1px solid var(--border)", borderRadius: "var(--r-md)" }}
              >
                <label
                  className={radioLabel}
                  style={{ background: mode === "trash" ? "var(--bg-elev)" : "transparent" }}
                >
                  <input
                    type="radio"
                    name="delete-mode"
                    value="trash"
                    checked={mode === "trash"}
                    onChange={() => {
                      setMode("trash");
                      setAcknowledged(false);
                    }}
                    className="mt-0.5"
                    style={{ accentColor: "var(--accent-fill)" }}
                  />
                  <span className="text-[13px] leading-[1.5]">
                    <span className="block font-medium" style={{ color: "var(--fg)" }}>
                      Move to trash
                    </span>
                    <span style={{ color: "var(--fg-muted)" }}>
                      Restorable by signing in again until the trash window ends.
                    </span>
                  </span>
                </label>
                <label
                  className={radioLabel}
                  style={{
                    borderTop: "1px solid var(--border)",
                    background: permanent ? "var(--danger-bg)" : "transparent",
                  }}
                >
                  <input
                    type="radio"
                    name="delete-mode"
                    value="permanent"
                    checked={permanent}
                    onChange={() => setMode("permanent")}
                    className="mt-0.5"
                    style={{ accentColor: "var(--accent-fill)" }}
                  />
                  <span className="text-[13px] leading-[1.5]">
                    <span className="block font-medium" style={{ color: "var(--danger-strong)" }}>
                      Erase permanently now
                    </span>
                    <span style={{ color: "var(--fg-muted)" }}>
                      Skips the trash. Nothing can be restored.
                    </span>
                  </span>
                </label>
              </div>
            </fieldset>
            {permanent && (
              <label className="flex items-start gap-3 max-w-2xl cursor-pointer">
                <input
                  type="checkbox"
                  checked={acknowledged}
                  onChange={(e) => setAcknowledged(e.target.checked)}
                  disabled={busy}
                  className="mt-0.5"
                  style={{ accentColor: "var(--danger)" }}
                />
                <span className="text-[13px] leading-[1.5]" style={{ color: "var(--fg)" }}>
                  I understand my inboxes, messages, and domains are erased
                  immediately and can&apos;t be recovered, and that this sign-in
                  may be unable to register a new account for a while.
                </span>
              </label>
            )}
            <label className="block">
              <span className="text-[13px]" style={{ color: "var(--fg)" }}>
                Type{" "}
                <code
                  className="font-mono text-[12px] px-1.5 py-0.5"
                  style={{
                    background: "var(--bg-elev)",
                    border: "1px solid var(--border-sub)",
                    borderRadius: "var(--r-sm)",
                    color: "var(--fg)",
                  }}
                >
                  DELETE
                </code>{" "}
                to confirm:
              </span>
              <input
                autoFocus
                type="text"
                value={confirmText}
                onChange={(e) => setConfirmText(e.target.value)}
                disabled={busy}
                placeholder="DELETE"
                autoComplete="off"
                className="mt-1 w-full max-w-xs px-3 py-2 text-[13px] font-mono"
                style={{
                  background: "var(--bg-panel)",
                  border: "1px solid var(--border)",
                  borderRadius: "var(--r-md)",
                  color: "var(--fg)",
                }}
              />
            </label>
            {state === "error" && (
              <p
                role="alert"
                className="text-[13px]"
                style={{ color: "var(--danger-strong)" }}
              >
                Deleting didn&apos;t finish: {errorMessage || "unknown error"}
              </p>
            )}
            <div className="flex flex-wrap gap-2">
              <button
                onClick={handleDelete}
                disabled={!ready || busy}
                className="px-4 py-2 text-[13px] font-medium transition disabled:opacity-50 disabled:cursor-not-allowed"
                style={{
                  background: "var(--danger)",
                  color: "#fff",
                  borderRadius: "var(--r-md)",
                }}
              >
                {busy
                  ? permanent
                    ? "Erasing…"
                    : "Deleting…"
                  : permanent
                    ? "Erase my account permanently"
                    : "Delete my account"}
              </button>
              <button
                onClick={reset}
                disabled={busy}
                className="px-4 py-2 text-[13px] transition"
                style={{
                  background: "var(--bg-panel)",
                  color: "var(--fg)",
                  border: "1px solid var(--border)",
                  borderRadius: "var(--r-md)",
                }}
              >
                Cancel
              </button>
            </div>
          </div>
        )}
      </div>
    </section>
  );
}

function formatDate(iso: string): string {
  try {
    return new Intl.DateTimeFormat(undefined, {
      year: "numeric",
      month: "short",
      day: "numeric",
    }).format(new Date(iso));
  } catch {
    return iso;
  }
}
