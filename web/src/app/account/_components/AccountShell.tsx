import Link from "next/link";
import { Logo } from "@e2a/ui";
import type { ReactNode } from "react";

// Frame for the /account/* pages. These render OUTSIDE the authenticated
// (app) layout (a restricted or refused session must not bounce to the
// sign-in wall), so they carry their own chrome: the wordmark home link and a
// single left-aligned reading column. Deliberately quiet — the content is a
// decision about someone's account, not a marketing moment.
export function AccountShell({ children }: { children: ReactNode }) {
  return (
    <main
      className="flex-1 min-h-screen px-5 sm:px-8 pt-6 pb-16 sm:pt-10"
      style={{ background: "var(--bg)", color: "var(--fg)", fontFamily: "var(--f-ui)" }}
    >
      <div className="mx-auto w-full" style={{ maxWidth: 580 }}>
        <Link href="/" className="inline-flex items-center min-h-[44px]" style={{ color: "var(--fg)" }}>
          <Logo variant="mark" height={30} title="e2a home" />
        </Link>
        <div className="mt-10 sm:mt-16">{children}</div>
      </div>
    </main>
  );
}

export function AccountHeading({
  children,
  headingRef,
}: {
  children: ReactNode;
  headingRef?: React.Ref<HTMLHeadingElement>;
}) {
  return (
    <h1
      ref={headingRef}
      tabIndex={headingRef ? -1 : undefined}
      style={{
        fontFamily: "var(--f-ui)",
        fontWeight: 700,
        fontSize: "clamp(24px, 4.5vw, 30px)",
        letterSpacing: "-0.015em",
        lineHeight: 1.15,
        color: "var(--fg)",
        margin: 0,
      }}
    >
      {children}
    </h1>
  );
}

export function AccountText({ children }: { children: ReactNode }) {
  return (
    <p className="mt-3 text-[15px] leading-[1.6]" style={{ color: "var(--fg-muted)", maxWidth: "62ch" }}>
      {children}
    </p>
  );
}

// Link-styled primary action ("Back to e2a", "Open the dashboard"). Plain
// anchors, not next/link, where the destination must re-read the session.
export const primaryActionStyle: React.CSSProperties = {
  background: "var(--accent-fill)",
  color: "var(--accent-fg)",
  borderRadius: "var(--r-md)",
};

export const secondaryActionStyle: React.CSSProperties = {
  background: "var(--bg-panel)",
  color: "var(--fg)",
  border: "1px solid var(--border)",
  borderRadius: "var(--r-md)",
};

export const dangerActionStyle: React.CSSProperties = {
  background: "var(--danger)",
  color: "#fff",
  borderRadius: "var(--r-md)",
};

export const actionClassName =
  "inline-flex items-center justify-center min-h-[44px] px-5 text-[14px] font-medium transition disabled:opacity-50 disabled:cursor-not-allowed";
