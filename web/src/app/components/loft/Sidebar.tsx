"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import type { ReactNode } from "react";
import { useAuth } from "../AuthProvider";
import { useTheme } from "../ThemeProvider";
import { usePendingCount } from "../hooks/usePendingCount";
import { useUnreadCount } from "../hooks/useUnreadCount";
import { ThemeToggle } from "@e2a/ui";
import { TokenCanopyGlyph } from "./TokenCanopyBadge";

type IconKey = "plus" | "grid" | "user" | "clock" | "globe" | "key" | "settings" | "msg" | "shield" | "card" | "doc" | "trash" | "chart";

const ICONS: Record<IconKey, ReactNode> = {
  plus: (
    <>
      <circle cx="12" cy="12" r="9.5" />
      <path d="M12 8v8M8 12h8" />
    </>
  ),
  grid: (
    <>
      <rect x="3.5" y="3.5" width="7" height="7" rx="1.5" />
      <rect x="13.5" y="3.5" width="7" height="7" rx="1.5" />
      <rect x="3.5" y="13.5" width="7" height="7" rx="1.5" />
      <rect x="13.5" y="13.5" width="7" height="7" rx="1.5" />
    </>
  ),
  user: (
    <>
      <circle cx="12" cy="8" r="4" />
      <path d="M4.5 21a7.5 7.5 0 0115 0" />
    </>
  ),
  clock: (
    <>
      <circle cx="12" cy="12" r="9.5" />
      <polyline points="12 6.5 12 12 16 14" />
    </>
  ),
  chart: (
    <>
      <path d="M4 20V4" />
      <path d="M4 20h16" />
      <path d="M8.5 20v-6" />
      <path d="M13 20V9" />
      <path d="M17.5 20v-9.5" />
    </>
  ),
  globe: (
    <>
      <circle cx="12" cy="12" r="9.5" />
      <path d="M3 12h18" />
      <path d="M12 3a16 16 0 010 18 16 16 0 010-18z" />
    </>
  ),
  key: (
    <>
      <circle cx="8" cy="14" r="4" />
      <path d="M11 11l9-9M15 6l3 3M18 3l3 3" />
    </>
  ),
  settings: (
    <>
      <circle cx="12" cy="12" r="2.5" />
      <path d="M19 12a7 7 0 00-.1-1.3l2-1.6-2-3.5-2.4 1a7 7 0 00-2.2-1.3l-.3-2.6h-4l-.4 2.6a7 7 0 00-2.2 1.3l-2.4-1-2 3.5 2 1.6A7 7 0 005 12c0 .4 0 .9.1 1.3l-2 1.6 2 3.5 2.4-1a7 7 0 002.2 1.3l.4 2.6h4l.3-2.6a7 7 0 002.2-1.3l2.4 1 2-3.5-2-1.6c.1-.4.1-.9.1-1.3z" />
    </>
  ),
  msg: <path d="M21 15a2 2 0 01-2 2H7l-4 4V5a2 2 0 012-2h14a2 2 0 012 2z" />,
  card: (
    <>
      <rect x="3" y="5" width="18" height="14" rx="2" />
      <path d="M3 10h18" />
      <path d="M7 15h3" />
    </>
  ),
  // Document-with-lines — the reusable email templates library.
  doc: (
    <>
      <path d="M14 3H7a2 2 0 00-2 2v14a2 2 0 002 2h10a2 2 0 002-2V8l-5-5z" />
      <path d="M14 3v5h5" />
      <path d="M9 13h6M9 17h4" />
    </>
  ),
  // Shield-with-checkmark — denotes the signing-secret integrity guard.
  // Matches the "API keys" icon's silhouette weight so the credential
  // pair reads as visually related in the sidebar.
  shield: (
    <>
      <path d="M12 3l8 3v6c0 4.5-3.5 8-8 9-4.5-1-8-4.5-8-9V6l8-3z" />
      <path d="M9 12l2 2 4-4" />
    </>
  ),
  // Trash can — the account-wide trash (deleted inboxes, restorable 30d).
  trash: (
    <>
      <path d="M4 7h16" />
      <path d="M9 7V5a1 1 0 011-1h4a1 1 0 011 1v2" />
      <path d="M6 7l1 13a2 2 0 002 2h6a2 2 0 002-2l1-13" />
      <path d="M10 11v6M14 11v6" />
    </>
  ),
};

type NavItem = {
  href: string;
  label: string;
  icon: IconKey;
  /** When true, also count `pathname === href + "/…"` as active. */
  matchPrefix?: boolean;
  /** Additional prefixes that should highlight this item — used when the
   *  feature lives under a different URL root (e.g. Agents is at
   *  `/inboxes` but the per-agent screens are at `/inboxes/*`). */
  matchPrefixes?: string[];
};

const NAV_ITEMS: NavItem[] = [
  { href: "/get-started", label: "Get started", icon: "plus" },
  {
    href: "/inboxes",
    label: "Inboxes",
    icon: "grid",
    // Keep Inboxes lit when the user drills into a specific inbox's
    // screens (messages, settings, etc.). Note: we don't use `matchPrefix:
    // true` here because that would also light up Inboxes on
    // /reviews — Pending is a sibling top-level feature.
    matchPrefixes: ["/inboxes"],
  },
  {
    href: "/reviews",
    label: "Pending",
    icon: "clock",
    matchPrefix: true,
  },
  // Delivery health sits with the other check-in surfaces (Inboxes, Pending)
  // rather than down in the configuration block. Named "Metrics" to match
  // /v1/metrics, `e2a metrics`, and the MCP tools — one word everywhere. Not
  // "Dashboard": /dashboard is a retired redirect to /inboxes.
  { href: "/metrics", label: "Metrics", icon: "chart" },
  {
    href: "/contacts",
    label: "Contacts",
    icon: "user",
    matchPrefix: true,
  },
  {
    // Templates (beta) — reusable email templates + the starter gallery.
    // matchPrefix keeps the entry lit on the /templates/edit detail view.
    href: "/templates",
    label: "Templates",
    icon: "doc",
    matchPrefix: true,
  },
  { href: "/domains", label: "Domains", icon: "globe" },
  { href: "/api-keys", label: "API keys", icon: "key" },
  { href: "/webhooks", label: "Webhooks", icon: "shield" },
  { href: "/trash", label: "Trash", icon: "trash" },
  { href: "/billing", label: "Billing", icon: "card" },
];

function NavIcon({ kind }: { kind: IconKey }) {
  return (
    <svg
      width="17"
      height="17"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.6"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden
    >
      {ICONS[kind]}
    </svg>
  );
}

function isActive(
  pathname: string,
  item: NavItem | { href: string; matchPrefix?: boolean; matchPrefixes?: string[] },
) {
  if (pathname === item.href) return true;
  if (item.matchPrefix && pathname.startsWith(item.href + "/")) return true;
  if (item.matchPrefixes) {
    for (const p of item.matchPrefixes) {
      if (pathname === p || pathname.startsWith(p + "/")) return true;
    }
  }
  return false;
}

// BottomNavLink renders a single bottom-of-sidebar entry (Settings,
// Send feedback) with the same active-state treatment the main
// NAV_ITEMS loop applies — accent inset shadow + elevated background +
// non-muted text + aria-current=page. The two visually-similar links
// previously diverged: Settings had the full active treatment inlined
// (4× redundant `isActive(...)` calls), Feedback had no active
// treatment at all. Factoring them through one component closes that
// asymmetry and keeps the keyboard/screen-reader contract consistent.
function BottomNavLink({
  href,
  icon,
  label,
  pathname,
}: {
  href: string;
  icon: IconKey;
  label: string;
  pathname: string;
}) {
  const active = isActive(pathname, { href });
  return (
    <Link
      href={href}
      aria-current={active ? "page" : undefined}
      className="flex items-center gap-2.5 px-3 py-2 text-[13px] font-sans"
      style={{
        borderRadius: "var(--r-md)",
        fontWeight: active ? 500 : 400,
        color: active ? "var(--fg)" : "var(--fg-muted)",
        background: active ? "var(--bg-elev)" : "transparent",
        boxShadow: active ? "inset 2px 0 0 var(--accent)" : "none",
      }}
    >
      <NavIcon kind={icon} />
      {label}
    </Link>
  );
}

function userInitials(user: { name?: string; email: string }): string {
  if (user.name) {
    const parts = user.name.trim().split(/\s+/);
    return ((parts[0]?.[0] ?? "") + (parts[1]?.[0] ?? ""))
      .toUpperCase()
      .slice(0, 2);
  }
  return user.email.slice(0, 2).toUpperCase();
}

// The default `className` hides the sidebar below `md` because the app
// layout swaps in a mobile slide-in sheet at those sizes. Pass an explicit
// className (e.g. "flex flex-col") to render the sidebar in any container,
// like that sheet.
export function Sidebar({
  className = "hidden md:flex md:flex-col",
}: {
  className?: string;
} = {}) {
  const pathname = usePathname() ?? "";
  const { user, signOut } = useAuth();
  const pendingCount = usePendingCount();
  const unreadCount = useUnreadCount();
  const { theme, setTheme } = useTheme();

  return (
    <aside
      className={`${className} w-[248px] shrink-0 sticky top-0 h-screen`}
      style={{
        background: "var(--bg-panel)",
        borderRight: "1px solid var(--border)",
      }}
    >
      {/*
        Brand block — the app's single identity anchor, and the only place
        either brand is stated: umbrella (Token Canopy) over product (e2a).
        The page Topbar it used to align with is gone; its only content was a
        breadcrumb repeating the nav's active tab and the page's own H1.
        Padding is horizontal-only; min-h + flex centering handle the rest.
      */}
      <Link
        href="/"
        className="flex items-center gap-2.5 px-5"
        style={{
          minHeight: "var(--chrome-h)",
          borderBottom: "1px solid var(--border)",
        }}
      >
        <TokenCanopyGlyph size={30} />
        <div className="min-w-0">
          {/* Umbrella brand, stated first and carrying the only mark. */}
          <div
            className="text-[13.5px] font-semibold leading-none truncate"
            style={{ color: "var(--fg)", letterSpacing: "0.005em" }}
          >
            Token Canopy
          </div>
          {/* Product, named beneath it. Set in mono because that is e2a's
              own voice throughout the app (ids, addresses, the logo tile),
              which keeps it reading as a distinct thing rather than a
              tagline under the brand. */}
          <div
            className="font-mono text-[12px] mt-1 leading-none truncate"
            style={{ color: "var(--fg-muted)", letterSpacing: "-0.01em" }}
          >
            e2a
          </div>
        </div>
      </Link>

      {/*
        Workspace/org switcher intentionally omitted until multi-tenant
        orgs land (tracked in GitHub issue #130). Until then the
        bottom-of-sidebar user card is the canonical identity
        affordance — a second card here would just duplicate it.
      */}

      {/* Nav */}
      <nav className="flex-1 px-3 pt-3 pb-1.5">
        {NAV_ITEMS.map((item) => {
          const active = isActive(pathname, item);
          let badgeLabel: number | string | null = null;
          if (item.href === "/inboxes" && unreadCount && unreadCount.count > 0) {
            badgeLabel = unreadCount.more ? "99+" : unreadCount.count;
          } else if (
            item.href === "/reviews" &&
            pendingCount !== null &&
            pendingCount > 0
          ) {
            badgeLabel = pendingCount;
          }
          return (
            <Link
              key={item.href}
              href={item.href}
              aria-current={active ? "page" : undefined}
              className="flex items-center gap-2.5 px-3 py-2 mb-px text-[13px] font-sans"
              style={{
                borderRadius: "var(--r-md)",
                fontWeight: active ? 500 : 400,
                color: active ? "var(--fg)" : "var(--fg-muted)",
                background: active ? "var(--bg-elev)" : "transparent",
                boxShadow: active ? "inset 2px 0 0 var(--accent)" : "none",
              }}
            >
              <NavIcon kind={item.icon} />
              <span className="flex-1">{item.label}</span>
              {badgeLabel !== null && (
                <span
                  data-nav-badge
                  className="inline-flex items-center justify-center font-mono text-[10px] font-bold text-white"
                  style={{
                    minWidth: 18,
                    height: 18,
                    padding: "0 6px",
                    background: "var(--accent)",
                    borderRadius: 999,
                  }}
                >
                  {badgeLabel}
                </span>
              )}
            </Link>
          );
        })}
      </nav>

      {/* Bottom */}
      <div
        className="px-3 pt-2.5 pb-3.5"
        style={{ borderTop: "1px solid var(--border)" }}
      >
        <BottomNavLink
          href="/settings"
          icon="settings"
          label="Settings"
          pathname={pathname}
        />
        <BottomNavLink
          href="/feedback"
          icon="msg"
          label="Send feedback"
          pathname={pathname}
        />

        {/* Theme toggle — light / dark / system, persisted to localStorage
            by ThemeProvider. */}
        <div className="mt-2.5">
          <ThemeToggle value={theme} onChange={setTheme} className="w-full" />
        </div>

        {/* User card */}
        {user && (
          <div
            className="flex items-center gap-2.5 mt-2.5 px-2.5 py-2"
            style={{
              border: "1px solid var(--border-sub)",
              borderRadius: "var(--r-md)",
            }}
          >
            <div
              className="flex items-center justify-center text-[11px] font-bold text-white shrink-0 rounded-full"
              style={{
                width: 28,
                height: 28,
                background: "var(--av-4)",
              }}
            >
              {userInitials(user)}
            </div>
            <div className="flex-1 min-w-0">
              <div
                className="text-[12px] font-medium truncate"
                style={{ color: "var(--fg)" }}
              >
                {user.name || "User"}
              </div>
              <div
                className="font-mono text-[10px] truncate"
                style={{ color: "var(--fg-subtle)" }}
              >
                {user.email}
              </div>
            </div>
            <button
              type="button"
              onClick={signOut}
              title="Sign out"
              className="shrink-0 transition"
              style={{ color: "var(--fg-muted)" }}
            >
              <svg
                width="16"
                height="16"
                viewBox="0 0 24 24"
                fill="none"
                stroke="currentColor"
                strokeWidth="1.6"
                strokeLinecap="round"
                strokeLinejoin="round"
                aria-hidden
              >
                <path d="M9 21H5a2 2 0 01-2-2V5a2 2 0 012-2h4" />
                <polyline points="16 17 21 12 16 7" />
                <line x1="21" y1="12" x2="9" y2="12" />
              </svg>
            </button>
          </div>
        )}
      </div>
    </aside>
  );
}
