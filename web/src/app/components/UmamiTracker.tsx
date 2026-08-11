"use client";

import { usePathname } from "next/navigation";
import { UMAMI_WEBSITE_ID } from "../../lib/site";

/**
 * Loads the Umami tracker on public marketing routes only.
 *
 * e2a.dev serves the public marketing site and the authenticated dashboard
 * from one Next.js app; the dashboard is a route group ((app)) with
 * unprefixed URLs, so there is no structural way to exclude it in the
 * layout tree. These are the dashboard's top-level path segments, whose
 * URLs carry inbox/agent/resource ids that must not land in analytics
 * (the public-sites-only decision in
 * tokencanopy/docs/superpowers/specs/2026-08-10-umami-web-analytics-design.md).
 * Keep this list in sync with the (app) route group.
 */
const DASHBOARD_SEGMENTS = new Set([
  "api-keys",
  "billing",
  "contacts",
  "dashboard",
  "domains",
  "feedback",
  "get-started",
  "inboxes",
  "metrics",
  "reviews",
  "settings",
  "templates",
  "trash",
  "webhook-secrets",
  "webhooks",
]);

export function UmamiTracker() {
  const pathname = usePathname();

  if (!UMAMI_WEBSITE_ID) return null;

  const segment = pathname.split("/")[1] ?? "";
  if (DASHBOARD_SEGMENTS.has(segment)) return null;

  return (
    <script
      async
      defer
      src="https://umami.tokencanopy.com/script.js"
      data-website-id={UMAMI_WEBSITE_ID}
    />
  );
}
