"use client";

import Script from "next/script";
import { useEffect, useRef, useState } from "react";
import { usePathname } from "next/navigation";
import { UMAMI_WEBSITE_ID } from "../../lib/site";

export const UMAMI_TRACKER_PATH =
  "/vendor/umami/umami-v3.2.0.1ad1145d.js";
const UMAMI_COLLECTOR_ORIGIN = "https://umami.tokencanopy.com";

type UmamiPayload = Record<string, unknown> & {
  referrer?: string;
  title?: string;
  url?: string;
};

type UmamiBeforeSend = (
  type: string,
  payload: UmamiPayload,
) => UmamiPayload | undefined;

declare global {
  interface Window {
    tcUmamiBeforeSend?: UmamiBeforeSend;
    umami?: {
      track: (transform: (payload: UmamiPayload) => UmamiPayload) => void;
    };
  }
}

/**
 * Public pages are an allowlist, not the inverse of the authenticated route
 * group. Unknown routes therefore stay untracked if a new product surface is
 * added without updating this component.
 */
const PUBLIC_MARKETING_PATHS = new Set([
  "/",
  "/api-docs",
  "/blog",
  "/blog/build-ecommerce-agent-with-email",
  "/blog/build-procurement-agent-with-email",
  "/blog/build-recruiting-agent-with-email",
  "/blog/build-sales-agent-with-email",
  "/blog/build-voice-follow-up-agent-with-email",
  "/blog/email-agent-with-google-adk",
  "/blog/email-for-openclaw-agents",
  "/blog/human-in-the-loop-for-agent-email",
  "/blog/send-email-from-python-agent",
  "/compare/e2a-vs-agentmail",
  "/docs",
  "/docs/python",
  "/email-api-for-ai-agents",
  "/mcp",
  "/python-sdk",
  "/use-cases",
  "/use-cases/ai-receptionist",
  "/use-cases/ecommerce-agent",
  "/use-cases/procurement-agent",
  "/use-cases/recruiting-agent",
  "/use-cases/sales-agent",
  "/use-cases/scheduling-agent",
  "/use-cases/support-agent",
  "/use-cases/voice-agent",
]);

function normalizePathname(pathname: string): string {
  if (pathname === "/") return pathname;
  return pathname.replace(/\/+$/, "");
}

export function isPublicMarketingPath(pathname: string | null): boolean {
  if (!pathname?.startsWith("/")) return false;
  return PUBLIC_MARKETING_PATHS.has(normalizePathname(pathname));
}

function sanitizePublicURL(raw: string, origin: string): string | undefined {
  try {
    const url = new URL(raw, origin);
    if (url.origin !== origin || !isPublicMarketingPath(url.pathname)) {
      return undefined;
    }
    // Rebuild rather than mutating the parsed URL so userinfo, search data,
    // and fragments can never survive into the analytics payload.
    return new URL(normalizePathname(url.pathname), origin).toString();
  } catch {
    return undefined;
  }
}

function sanitizeReferrer(raw: string, origin: string): string {
  if (!raw) return "";

  try {
    const url = new URL(raw, origin);
    if (url.origin === origin) {
      return sanitizePublicURL(url.toString(), origin) ?? "";
    }

    // Cross-origin attribution needs only the referring origin. Never carry a
    // third party's query string or fragment into our analytics store.
    return `${url.origin}/`;
  } catch {
    return "";
  }
}

/**
 * Loads Umami only for known public routes and keeps its automatic runtime
 * disabled. Umami normally installs persistent History API hooks for SPA
 * pageviews; removing its script tag on a dashboard navigation does not undo
 * those hooks. Manual pageviews let this root-layout component fail closed.
 */
export function UmamiTracker() {
  const pathname = usePathname();
  const isPublicPath = isPublicMarketingPath(pathname);
  const [trackerReady, setTrackerReady] = useState(false);
  // undefined = first page in this document; "" = a private route was visited.
  const lastTrackedURL = useRef<string | undefined>(undefined);

  useEffect(() => {
    const beforeSend: UmamiBeforeSend = (_type, payload) => {
      if (!isPublicMarketingPath(window.location.pathname)) return undefined;

      const url = sanitizePublicURL(
        typeof payload.url === "string" ? payload.url : window.location.href,
        window.location.origin,
      );
      if (!url) return undefined;

      return {
        ...payload,
        url,
        referrer: sanitizeReferrer(
          typeof payload.referrer === "string" ? payload.referrer : "",
          window.location.origin,
        ),
      };
    };

    window.tcUmamiBeforeSend = beforeSend;
    return () => {
      if (window.tcUmamiBeforeSend === beforeSend) {
        delete window.tcUmamiBeforeSend;
      }
    };
  }, []);

  useEffect(() => {
    if (!isPublicPath) {
      // Do not attribute a later public pageview through an authenticated page.
      lastTrackedURL.current = "";
      return;
    }
    if (!trackerReady || !window.umami || !pathname) return;

    const url = sanitizePublicURL(pathname, window.location.origin);
    if (!url || lastTrackedURL.current === url) return;

    const referrer =
      lastTrackedURL.current === undefined
        ? sanitizeReferrer(document.referrer, window.location.origin)
        : lastTrackedURL.current;

    window.umami.track((payload) => ({
      ...payload,
      referrer,
      title: document.title,
      url,
    }));
    lastTrackedURL.current = url;
  }, [isPublicPath, pathname, trackerReady]);

  if (!UMAMI_WEBSITE_ID || !isPublicPath) return null;

  return (
    <Script
      id="tc-umami-tracker"
      src={UMAMI_TRACKER_PATH}
      strategy="afterInteractive"
      data-host-url={UMAMI_COLLECTOR_ORIGIN}
      data-website-id={UMAMI_WEBSITE_ID}
      data-auto-track="false"
      data-before-send="tcUmamiBeforeSend"
      data-exclude-search="true"
      data-exclude-hash="true"
      onReady={() => setTrackerReady(true)}
    />
  );
}
