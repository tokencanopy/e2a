"use client";

// /account/restore — the interstitial a trashed account lands on after
// signing in again. The server issued a RESTRICTED session that can only read
// the deletion state and then restore or erase the account:
//
//   GET  /api/account/deletion → {email, deleted_at, purge_after, purge_in_progress}
//   POST /api/account/restore  → the same cookie becomes an ordinary session
//   POST /api/account/erase    → permanent deletion; the server clears the cookie
//
// Lives outside the (app) route group on purpose: the app shell would see no
// ordinary session and bounce to the sign-in wall.

import Link from "next/link";
import { useEffect, useRef, useState } from "react";
import { Button } from "@e2a/ui";
import { SignInLinks } from "../../components/SignInLinks";
import {
  type AccountDeletionView,
  daysUntil,
  formatLongDate,
  readApiError,
  trashWindowElapsed,
} from "../../../lib/accountDeletion";
import { hardNavigate } from "../../../lib/navigation";
import {
  AccountHeading,
  AccountShell,
  AccountText,
  actionClassName,
  dangerActionStyle,
  primaryActionStyle,
  secondaryActionStyle,
} from "../_components/AccountShell";

// Where a restored account continues. /dashboard is only a back-compat
// redirect to /inboxes, so go straight to the canonical home.
const AFTER_RESTORE_PATH = "/inboxes";

type Phase =
  | { kind: "loading" }
  | { kind: "load-error" }
  | { kind: "signed-out" }
  | { kind: "ready"; view: AccountDeletionView }
  | { kind: "purging" }
  | { kind: "not-in-trash" }
  | { kind: "erased" };

type Busy = null | "restore" | "erase";

export default function RestoreAccountPage() {
  const [phase, setPhase] = useState<Phase>({ kind: "loading" });
  const [attempt, setAttempt] = useState(0);

  useEffect(() => {
    let cancelled = false;
    fetch("/api/account/deletion", { credentials: "include" })
      .then(async (res) => {
        if (cancelled) return;
        if (res.status === 401) {
          setPhase({ kind: "signed-out" });
          return;
        }
        if (!res.ok) {
          setPhase({ kind: "load-error" });
          return;
        }
        const view = (await res.json()) as AccountDeletionView;
        if (cancelled) return;
        setPhase(view.purge_in_progress ? { kind: "purging" } : { kind: "ready", view });
      })
      .catch(() => {
        if (!cancelled) setPhase({ kind: "load-error" });
      });
    return () => {
      cancelled = true;
    };
  }, [attempt]);

  return (
    <AccountShell>
      <PhaseView
        phase={phase}
        onPhase={setPhase}
        onRetry={() => {
          setPhase({ kind: "loading" });
          setAttempt((a) => a + 1);
        }}
      />
    </AccountShell>
  );
}

function PhaseView({
  phase,
  onPhase,
  onRetry,
}: {
  phase: Phase;
  onPhase: (p: Phase) => void;
  onRetry: () => void;
}) {
  // Move focus to the heading whenever the page swaps to a terminal state,
  // so screen-reader and keyboard users land on the new content instead of
  // a button that no longer exists.
  const headingRef = useRef<HTMLHeadingElement>(null);
  const firstRender = useRef(true);
  useEffect(() => {
    if (firstRender.current) {
      firstRender.current = false;
      return;
    }
    headingRef.current?.focus();
  }, [phase.kind]);

  switch (phase.kind) {
    case "loading":
      return (
        // Reserve roughly the height of the loaded view so the page doesn't
        // jump when the account status arrives.
        <div role="status" aria-live="polite" style={{ minHeight: 360 }}>
          <p className="text-[15px]" style={{ color: "var(--fg-muted)" }}>
            Checking your account…
          </p>
        </div>
      );
    case "load-error":
      return (
        <>
          <AccountHeading headingRef={headingRef}>Account status didn&apos;t load</AccountHeading>
          <AccountText>The server didn&apos;t answer. Try again in a moment.</AccountText>
          <div className="mt-8">
            <button type="button" onClick={onRetry} className={actionClassName} style={secondaryActionStyle}>
              Try again
            </button>
          </div>
        </>
      );
    case "signed-out":
      return (
        <>
          <AccountHeading headingRef={headingRef}>Nothing to restore</AccountHeading>
          <AccountText>
            This browser isn&apos;t holding an account that&apos;s waiting to be restored. Sign in
            again to check the account you deleted.
          </AccountText>
          <div className="mt-8 flex flex-wrap items-center gap-x-5 gap-y-3">
            <SignInLinks
              primaryClassName={actionClassName}
              primaryStyle={primaryActionStyle}
              secondaryClassName="inline-flex items-center min-h-[44px] text-[14px] underline underline-offset-2"
              secondaryStyle={{ color: "var(--fg-muted)" }}
            />
          </div>
        </>
      );
    case "purging":
      return (
        <>
          <AccountHeading headingRef={headingRef}>This account is being erased</AccountHeading>
          <AccountText>
            Permanent deletion of this account has already started, so it can&apos;t be restored.
          </AccountText>
          <div className="mt-8">
            <Link href="/" className={actionClassName} style={secondaryActionStyle}>
              Back to e2a home
            </Link>
          </div>
        </>
      );
    case "not-in-trash":
      return (
        <>
          <AccountHeading headingRef={headingRef}>This account is already active</AccountHeading>
          <AccountText>
            It isn&apos;t in the trash anymore. It may have been restored from another tab.
          </AccountText>
          <div className="mt-8">
            {/* Plain anchor: the dashboard must re-read the session. */}
            <a href={AFTER_RESTORE_PATH} className={actionClassName} style={primaryActionStyle}>
              Open the dashboard
            </a>
          </div>
        </>
      );
    case "erased":
      return (
        <>
          <AccountHeading headingRef={headingRef}>Your account and its data were erased</AccountHeading>
          <AccountText>
            You&apos;re signed out. The identity you signed in with may be held for a while and
            can&apos;t register a new account until it&apos;s released.
          </AccountText>
          <div className="mt-8">
            <Link href="/" className={actionClassName} style={secondaryActionStyle}>
              Back to e2a home
            </Link>
          </div>
        </>
      );
    case "ready":
      return <TrashedAccount view={phase.view} onPhase={onPhase} />;
  }
}

function TrashedAccount({
  view,
  onPhase,
}: {
  view: AccountDeletionView;
  onPhase: (p: Phase) => void;
}) {
  const [busy, setBusy] = useState<Busy>(null);
  const [confirmingErase, setConfirmingErase] = useState(false);
  const [error, setError] = useState("");
  const [restoreRefused, setRestoreRefused] = useState(false);
  const [restored, setRestored] = useState(false);

  const confirmHeadingRef = useRef<HTMLHeadingElement>(null);
  const eraseTriggerRef = useRef<HTMLButtonElement>(null);
  const returnFocusToTrigger = useRef(false);

  useEffect(() => {
    if (confirmingErase) {
      confirmHeadingRef.current?.focus();
    } else if (returnFocusToTrigger.current) {
      returnFocusToTrigger.current = false;
      eraseTriggerRef.current?.focus();
    }
  }, [confirmingErase]);

  const deletedOn = formatLongDate(view.deleted_at);
  const purgeOn = formatLongDate(view.purge_after);

  const handleRestore = async () => {
    setBusy("restore");
    setError("");
    try {
      const res = await fetch("/api/account/restore", {
        method: "POST",
        credentials: "include",
      });
      if (res.ok) {
        setRestored(true);
        // Keep the buttons disabled through the navigation.
        hardNavigate(AFTER_RESTORE_PATH);
        return;
      }
      const err = await readApiError(res);
      if (res.status === 401) return onPhase({ kind: "signed-out" });
      if (err.code === "purge_in_progress") return onPhase({ kind: "purging" });
      if (err.code === "not_in_trash") return onPhase({ kind: "not-in-trash" });
      if (err.code === "registration_refused") {
        setRestoreRefused(true);
        setError(
          "This account can't be restored because the identity you signed in with has been closed.",
        );
      } else if (err.code === "rate_limited" || res.status === 429) {
        setError("This account was restored moments ago. Wait a few minutes before restoring it again.");
      } else if (res.status === 503) {
        setError("Restoring is temporarily unavailable. Your account is still in the trash. Try again in a few minutes.");
      } else {
        setError("Restoring didn't finish. Your account is still in the trash. Try again.");
      }
      setBusy(null);
    } catch {
      setError("Couldn't reach e2a. Check your connection and try again.");
      setBusy(null);
    }
  };

  const handleErase = async () => {
    setBusy("erase");
    setError("");
    try {
      const res = await fetch("/api/account/erase", {
        method: "POST",
        credentials: "include",
      });
      if (res.ok) return onPhase({ kind: "erased" });
      const err = await readApiError(res);
      if (res.status === 401) return onPhase({ kind: "signed-out" });
      if (err.code === "purge_in_progress") return onPhase({ kind: "purging" });
      if (err.code === "erase_held") {
        setError(
          "Sending is paused on this account, so it can't be erased right now. It stays in the trash and is deleted permanently when the trash window ends.",
        );
      } else if (res.status === 503) {
        setError("Erasing is temporarily unavailable. Your account stays in the trash. Try again later.");
      } else {
        setError("Erasing didn't finish. Your account stays in the trash. Try again.");
      }
      setBusy(null);
    } catch {
      setError("Couldn't reach e2a. Check your connection and try again.");
      setBusy(null);
    }
  };

  return (
    <>
      <AccountHeading>Your account is in the trash</AccountHeading>
      <AccountText>
        {view.email && (
          <>
            <span style={{ color: "var(--fg)" }}>{view.email}</span>{" "}
          </>
        )}
        {deletedOn ? <>was scheduled for deletion on {deletedOn}.</> : <>was scheduled for deletion.</>}{" "}
        {purgeOn && <>It will be permanently deleted after {purgeOn}.</>}
      </AccountText>

      {/* A refused restore has no restore window left to count down. */}
      {!restoreRefused && <TrashWindow deletedAt={view.deleted_at} purgeAfter={view.purge_after} />}

      {!restoreRefused && (
        <div
          className="mt-10 grid gap-8 sm:grid-cols-2 pt-8"
          style={{ borderTop: "1px solid var(--border)" }}
        >
          <div>
            <h2 className="text-[14px] font-semibold" style={{ color: "var(--fg)" }}>
              Restoring brings back
            </h2>
            <ul className="mt-2 space-y-1.5 text-[14px] leading-[1.55] list-disc pl-5" style={{ color: "var(--fg-muted)" }}>
              <li>Your inboxes and their messages</li>
              <li>Your domains and account settings</li>
            </ul>
          </div>
          <div>
            <h2 className="text-[14px] font-semibold" style={{ color: "var(--fg)" }}>
              Restoring doesn&apos;t bring back
            </h2>
            <ul className="mt-2 space-y-1.5 text-[14px] leading-[1.55] list-disc pl-5" style={{ color: "var(--fg-muted)" }}>
              <li>API keys. They stay revoked, so create new ones.</li>
              <li>Custom domain verification. Re-verify each domain before it sends.</li>
              <li>Connected apps. Reconnect them.</li>
            </ul>
          </div>
        </div>
      )}

      {error && (
        <p
          role="alert"
          className="mt-8 p-3 text-[14px] leading-[1.5]"
          style={{
            color: "var(--danger-strong)",
            background: "var(--danger-bg)",
            borderRadius: "var(--r-md)",
          }}
        >
          {error}
        </p>
      )}

      {restored && (
        <p role="status" className="mt-8 text-[14px]" style={{ color: "var(--fg-muted)" }}>
          Restored. Opening your dashboard…
        </p>
      )}

      {!confirmingErase ? (
        <div className="mt-8 flex flex-wrap items-center gap-3">
          {!restoreRefused && (
            <Button
              variant="primary"
              onClick={handleRestore}
              disabled={busy !== null || restored}
              className="min-h-[44px] min-w-[160px]"
            >
              {busy === "restore" || restored ? "Restoring…" : "Restore account"}
            </Button>
          )}
          <button
            ref={eraseTriggerRef}
            type="button"
            onClick={() => {
              setError("");
              setConfirmingErase(true);
            }}
            disabled={busy !== null || restored}
            className={actionClassName}
            style={{ ...secondaryActionStyle, color: "var(--danger-strong)" }}
          >
            Erase now
          </button>
        </div>
      ) : (
        <section
          aria-labelledby="erase-confirm-title"
          className="mt-8 p-5"
          style={{
            background: "var(--bg-panel)",
            border: "1px solid var(--danger)",
            borderRadius: "var(--r-lg)",
          }}
        >
          <h2
            id="erase-confirm-title"
            ref={confirmHeadingRef}
            tabIndex={-1}
            className="text-[16px] font-semibold"
            style={{ color: "var(--fg)" }}
          >
            Erase this account permanently?
          </h2>
          <p className="mt-2 text-[14px] leading-[1.6]" style={{ color: "var(--fg-muted)" }}>
            This can&apos;t be undone. Your inboxes, messages, domains, and settings are erased right
            away and can&apos;t be recovered.
          </p>
          <p className="mt-2 text-[14px] leading-[1.6]" style={{ color: "var(--fg-muted)" }}>
            The identity you signed in with may be held for a while afterwards and can&apos;t register
            a new account until it&apos;s released.
          </p>
          <div className="mt-5 flex flex-wrap items-center gap-3">
            <button
              type="button"
              onClick={handleErase}
              disabled={busy !== null}
              className={actionClassName}
              style={dangerActionStyle}
            >
              {busy === "erase" ? "Erasing…" : "Erase permanently"}
            </button>
            <button
              type="button"
              onClick={() => {
                returnFocusToTrigger.current = true;
                setConfirmingErase(false);
              }}
              disabled={busy !== null}
              className={actionClassName}
              style={secondaryActionStyle}
            >
              Cancel
            </button>
          </div>
        </section>
      )}
    </>
  );
}

// The trash window as a track: how much of the restore window has passed,
// with today marked. Purely visual — the sentence under it carries the same
// information for assistive tech.
function TrashWindow({
  deletedAt,
  purgeAfter,
}: {
  deletedAt: string | null;
  purgeAfter: string | null;
}) {
  const elapsed = trashWindowElapsed(deletedAt, purgeAfter);
  const days = daysUntil(purgeAfter);
  if (elapsed === null) return null;
  const pct = `${(elapsed * 100).toFixed(1)}%`;
  const label =
    days === 0 ? "Last day to restore" : days === 1 ? "1 day left to restore" : `${days} days left to restore`;

  return (
    <div className="mt-8">
      <div
        aria-hidden="true"
        className="flex justify-between text-[12px]"
        style={{ color: "var(--fg-muted)" }}
      >
        <span>{shortDate(deletedAt)}</span>
        <span>{shortDate(purgeAfter)}</span>
      </div>
      <div
        aria-hidden="true"
        data-testid="trash-window-track"
        className="relative mt-2"
        style={{ height: 8, borderRadius: 999, background: "var(--bg-sunken)" }}
      >
        <div
          style={{
            position: "absolute",
            inset: 0,
            width: pct,
            borderRadius: 999,
            background: "var(--accent)",
          }}
        />
        <div
          style={{
            position: "absolute",
            top: "50%",
            left: pct,
            width: 16,
            height: 16,
            transform: "translate(-50%, -50%)",
            borderRadius: 999,
            background: "var(--bg-panel)",
            border: "3px solid var(--accent-strong)",
          }}
        />
      </div>
      <p className="mt-3 text-[14px] font-semibold" style={{ color: "var(--accent-strong)" }}>
        {label}
      </p>
    </div>
  );
}

function shortDate(iso: string | null): string {
  if (!iso) return "";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "";
  try {
    return new Intl.DateTimeFormat(undefined, { month: "short", day: "numeric" }).format(d);
  } catch {
    return "";
  }
}
