// Site-level config that the marketing/inboxes surface needs at build time.
// All values resolve from public env vars so a fork or self-host can override
// without touching source. Defaults are localhost-friendly so `next dev` and
// the static build both work out of the box without any env setup.

export const SITE_URL =
  process.env.NEXT_PUBLIC_SITE_URL?.replace(/\/$/, "") || "http://localhost:3000";

export const SITE_NAME = process.env.NEXT_PUBLIC_SITE_NAME || "e2a";

// Shared agent domain for slug-based registration (e.g. "agents.example.com").
// Empty when the deployment doesn't offer a shared domain — in that mode the
// landing page copy reads as "your custom domain" rather than naming the
// shared host.
//
// Use AGENTS_DOMAIN for *logic* (equality checks, filtering) — empty means
// "no shared domain configured" and that distinction matters. Use
// AGENTS_DOMAIN_DISPLAY for any *human-visible* template that includes the
// domain after an `@` — it falls back to "your-domain.com" so a forgotten
// build arg doesn't ship something like `slug@` to real users.
export const AGENTS_DOMAIN = process.env.NEXT_PUBLIC_AGENTS_DOMAIN || "";
// "agents.example.com" rather than "your-domain.com" so the placeholder
// still hints at the shared-subdomain pattern (the agents.* prefix is
// part of the product's mental model — your domain doesn't host its own
// MX, ours does on a subdomain you don't have to own).
export const AGENTS_DOMAIN_DISPLAY = AGENTS_DOMAIN || "agents.example.com";

// Address shown in the in-app feedback form. Empty hides the link.
export const FEEDBACK_EMAIL = process.env.NEXT_PUBLIC_FEEDBACK_EMAIL || "";

// Google Search Console verification token — only emitted into <head> when
// configured, so forks don't accidentally inherit the upstream property.
export const GOOGLE_SITE_VERIFICATION =
  process.env.NEXT_PUBLIC_GOOGLE_SITE_VERIFICATION || "";

// Umami web analytics. The e2a.dev website record id, created in the Umami
// dashboard. Empty in the OSS build and for self-hosters (no tracking); the
// hosted deployment bakes in NEXT_PUBLIC_UMAMI_WEBSITE_ID at image build
// time. The tracker is gated to public marketing routes only — the
// authenticated dashboard is not tracked.
export const UMAMI_WEBSITE_ID = process.env.NEXT_PUBLIC_UMAMI_WEBSITE_ID || "";

// Umami collector origin (e.g. "https://umami.tokencanopy.com") — where
// beacons actually get POSTed. Paired with UMAMI_WEBSITE_ID and deliberately
// given NO default: a self-hoster running their own Umami instance who sets
// only NEXT_PUBLIC_UMAMI_WEBSITE_ID (the variable .env.example presents as
// "the" way to enable analytics) must not have their pageviews silently
// beaconed to the upstream operator's collector. UmamiTracker requires BOTH
// vars before it loads at all. The hosted deployment bakes this in at image
// build time alongside the website id.
export const UMAMI_COLLECTOR_ORIGIN = process.env.NEXT_PUBLIC_UMAMI_COLLECTOR_ORIGIN || "";

// Site-relative path of the pricing page, when the deployment has one.
//
// Pricing is NOT part of this app. The hosted deployment serves it from the
// private ops repo via a Caddy route, because tier numbers and dollar amounts
// are hosted-service concerns the OSS repo doesn't own. Staging has no such
// route and neither does a self-host. So the sitemap must only advertise
// /pricing where it actually resolves — an unset value means "no pricing page",
// and listing a 404 in a sitemap is a crawl-quality problem, not a no-op.
export const PRICING_PATH = process.env.NEXT_PUBLIC_PRICING_PATH || "";

// Sign-in entry point for the dashboard's "Sign in" links. Defaults to the
// legacy Google OAuth door, which every self-host deployment has. The hosted
// deployment bakes in NEXT_PUBLIC_E2A_SIGN_IN_URL=/api/auth/oidc/login at
// image build time to make the TokenCanopy OIDC door (multi-provider chooser)
// the default. The OIDC route only exists when the server runs with
// E2A_OIDC_ENABLED=true, so operators must leave this unset unless they also
// enable and configure the server's generic OIDC provider.
export const LEGACY_SIGN_IN_URL = "/api/auth/login";
export const SIGN_IN_URL =
  process.env.NEXT_PUBLIC_E2A_SIGN_IN_URL || LEGACY_SIGN_IN_URL;

// The route alone cannot identify an OIDC provider: self-hosters use the same
// endpoint for Entra, Okta, and other providers. Only the upstream hosted site
// identities brand that route as TokenCanopy; every other OIDC deployment
// remains provider-neutral.
const TOKEN_CANOPY_HOSTED_SITE_URLS = new Set([
  "https://e2a.dev",
  "https://staging.e2a.dev",
]);
export const SIGN_IN_LABEL =
  SIGN_IN_URL === LEGACY_SIGN_IN_URL
    ? "Sign in with Google"
    : SIGN_IN_URL === "/api/auth/oidc/login" &&
        TOKEN_CANOPY_HOSTED_SITE_URLS.has(SITE_URL)
      ? "Sign in with TokenCanopy"
      : "Sign in";

// Adds the post-login destination to the requested sign-in door. The target
// may be a same-origin path or an absolute operator-supplied URL;
// a placeholder origin lets URL handle either shape without leaking into the
// returned same-origin path.
export function signInURLWithReturnTo(
  returnTo: string,
  signInURL = SIGN_IN_URL,
): string {
  const placeholderOrigin = "https://e2a-sign-in.invalid";
  const url = new URL(signInURL, placeholderOrigin);
  url.searchParams.set("return_to", returnTo);

  if (url.origin === placeholderOrigin) {
    return `${url.pathname}${url.search}${url.hash}`;
  }
  return url.toString();
}
