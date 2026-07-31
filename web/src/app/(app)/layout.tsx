"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import Link from "next/link";
import { useAuth } from "../components/AuthProvider";
import { SWRProvider } from "../components/swr/SWRProvider";
import { PendingPollingOwner } from "../components/swr/PendingPollingOwner";
import { SignInLink } from "../components/SignInLink";
import { Sidebar } from "../components/loft/Sidebar";
import { SIGN_IN_LABEL } from "../../lib/site";

const FOCUSABLE_SELECTOR =
  'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';
const APP_LOGIN_RETURN_PATHS = ["/reviews", "/inboxes/messages"] as const;

export default function AppLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  const { user, loading } = useAuth();
  const [mobileNavOpen, setMobileNavOpen] = useState(false);
  // The hamburger button is the open trigger; we stash a ref so the
  // drawer can restore focus to it on close (otherwise focus would
  // land on document.body — keyboard users would lose their place).
  const triggerRef = useRef<HTMLButtonElement | null>(null);
  const drawerRef = useRef<HTMLDivElement | null>(null);

  const closeMobileNav = useCallback(() => setMobileNavOpen(false), []);

  useEffect(() => {
    if (!mobileNavOpen) return;

    // Focus the first focusable element inside the drawer when it opens
    // so keyboard users start inside the modal context, not on a stale
    // background element. requestAnimationFrame defers until the drawer
    // has actually painted.
    const raf = requestAnimationFrame(() => {
      const first = drawerRef.current?.querySelector<HTMLElement>(FOCUSABLE_SELECTOR);
      first?.focus();
    });

    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        closeMobileNav();
        return;
      }
      // Focus trap — wrap Tab/Shift+Tab inside the drawer so keyboard
      // users can't move focus to elements behind the backdrop.
      if (e.key !== "Tab" || !drawerRef.current) return;
      const focusables = drawerRef.current.querySelectorAll<HTMLElement>(FOCUSABLE_SELECTOR);
      if (focusables.length === 0) return;
      const first = focusables[0];
      const last = focusables[focusables.length - 1];
      const active = document.activeElement as HTMLElement | null;
      if (e.shiftKey && active === first) {
        e.preventDefault();
        last.focus();
      } else if (!e.shiftKey && active === last) {
        e.preventDefault();
        first.focus();
      }
    };
    document.addEventListener("keydown", onKey);
    const prev = document.body.style.overflow;
    document.body.style.overflow = "hidden";

    return () => {
      cancelAnimationFrame(raf);
      document.removeEventListener("keydown", onKey);
      document.body.style.overflow = prev;
      // Restore focus to the trigger so the user picks up where they
      // left off. Reading .current at cleanup time is intentional: if
      // the user resized to desktop while the drawer was open, the
      // trigger has unmounted and we should focus nothing rather than
      // a stale captured node. The lint rule's "capture at setup time"
      // advice is the wrong pattern here.
      // eslint-disable-next-line react-hooks/exhaustive-deps
      triggerRef.current?.focus();
    };
  }, [mobileNavOpen, closeMobileNav]);

  if (loading) {
    return (
      <div
        className="min-h-screen flex items-center justify-center"
        style={{ background: "var(--bg)", color: "var(--fg)" }}
      >
        <p className="text-[13px]" style={{ color: "var(--fg-muted)" }}>
          Loading...
        </p>
      </div>
    );
  }

  if (!user) {
    return (
      <div
        className="min-h-screen flex items-center justify-center px-6"
        style={{ background: "var(--bg)", color: "var(--fg)" }}
      >
        <div className="text-center">
          <p className="mb-4 text-[14px]" style={{ color: "var(--fg-muted)" }}>
            Sign in to access this page.
          </p>
          <SignInLink
            preserveCurrentPaths={APP_LOGIN_RETURN_PATHS}
            className="inline-block px-4 py-2 text-[13px] font-medium transition"
            style={{
              background: "var(--accent-fill)",
              color: "var(--accent-fg)",
              borderRadius: "var(--r-md)",
            }}
          >
            {SIGN_IN_LABEL}
          </SignInLink>
        </div>
      </div>
    );
  }

  return (
    <SWRProvider>
    <div
      className="flex min-h-screen"
      style={{ background: "var(--bg)" }}
      // Scope the 44px tap-target rule in globals.css to the
      // authenticated app surface only. Marketing/blog/docs/api-docs
      // pages keep their own visual density.
      data-app-surface=""
    >
      <PendingPollingOwner />
      {/* Desktop sidebar */}
      <Sidebar />

      {/* Mobile slide-in sidebar.
          role=dialog + aria-modal=true announce the drawer as a modal
          context to assistive tech. Focus is trapped by the keydown
          handler in the effect above and restored to the trigger on
          close. */}
      {mobileNavOpen && (
        <>
          <button
            type="button"
            aria-label="Close menu"
            onClick={closeMobileNav}
            className="md:hidden fixed inset-0 z-40"
            style={{ background: "rgba(26,23,20,0.4)" }}
          />
          {/*
            Delegated click — close the sheet when the user taps any nav
            link inside it. We can't reach into the Sidebar primitive for
            per-link onClick handlers, so this catches every <a> in the
            tree on the way up.
          */}
          <div
            ref={drawerRef}
            role="dialog"
            aria-modal="true"
            aria-label="Navigation"
            className="md:hidden fixed inset-y-0 left-0 z-50 w-[280px]"
            onClick={(e) => {
              if ((e.target as HTMLElement).closest("a")) closeMobileNav();
            }}
          >
            <Sidebar className="flex flex-col" />
          </div>
        </>
      )}

      <main className="flex-1 min-w-0 flex flex-col">
        {/* Mobile header */}
        <div
          className="md:hidden flex items-center justify-between px-4 py-3 sticky top-0 z-30"
          style={{
            background: "var(--bg-panel)",
            borderBottom: "1px solid var(--border)",
          }}
        >
          <Link
            href="/"
            className="flex items-center gap-2"
            style={{ color: "var(--fg)" }}
          >
            <span
              className="flex items-center justify-center font-mono font-bold text-[12px]"
              style={{
                width: 28,
                height: 28,
                borderRadius: 6,
                background: "var(--fg)",
                color: "var(--bg)",
                letterSpacing: "-0.04em",
              }}
            >
              e2a
            </span>
          </Link>
          <button
            ref={triggerRef}
            type="button"
            onClick={() => setMobileNavOpen(true)}
            aria-label="Open menu"
            aria-expanded={mobileNavOpen}
            aria-haspopup="dialog"
            className="inline-flex items-center justify-center"
            style={{
              width: 36,
              height: 36,
              borderRadius: "var(--r-md)",
              border: "1px solid var(--border)",
              background: "var(--bg-panel)",
              color: "var(--fg)",
            }}
          >
            <svg
              width="18"
              height="18"
              viewBox="0 0 24 24"
              fill="none"
              stroke="currentColor"
              strokeWidth="2"
              strokeLinecap="round"
              strokeLinejoin="round"
              aria-hidden
            >
              <line x1="3" y1="6" x2="21" y2="6" />
              <line x1="3" y1="12" x2="21" y2="12" />
              <line x1="3" y1="18" x2="21" y2="18" />
            </svg>
          </button>
        </div>

        <div className="flex-1 overflow-auto">{children}</div>
      </main>
    </div>
    </SWRProvider>
  );
}
