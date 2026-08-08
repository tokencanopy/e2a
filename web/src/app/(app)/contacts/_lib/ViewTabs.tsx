"use client";

// Minimal view-switcher for the two account-wide address views (Contacts ·
// Suppressions). Deliberately two hardcoded links in AgentHeader's underline
// style — promote to a shared component only if a third view appears.

import Link from "next/link";

const VIEWS = [
  { key: "contacts", label: "Contacts", href: "/contacts" },
  { key: "suppressions", label: "Suppressions", href: "/contacts/suppressions" },
] as const;

export type ContactsView = (typeof VIEWS)[number]["key"];

export function ViewTabs({ active }: { active: ContactsView }) {
  return (
    <div className="mb-6 flex items-center gap-1" style={{ borderBottom: "1px solid var(--border)" }}>
      {VIEWS.map((view) => {
        const isActive = view.key === active;
        return (
          <Link
            key={view.key}
            href={view.href}
            aria-current={isActive ? "page" : undefined}
            style={{
              padding: "8px 14px 10px",
              fontSize: 13,
              fontWeight: isActive ? 600 : 400,
              color: isActive ? "var(--fg)" : "var(--fg-muted)",
              borderBottom: isActive ? "2px solid var(--accent)" : "2px solid transparent",
              marginBottom: -1,
              textDecoration: "none",
            }}
          >
            {view.label}
          </Link>
        );
      })}
    </div>
  );
}
