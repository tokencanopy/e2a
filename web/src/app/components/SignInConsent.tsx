import { PRIVACY_URL, TERMS_URL } from "../../lib/site";

// Rendered directly beneath the primary "Sign in" call to action — the
// landing page nav and the dashboard's signed-out screen both use this so
// the wording and gating logic live in exactly one place.
//
// Both legal pages are hosted-deployment-only (see PRIVACY_URL / TERMS_URL
// in lib/site): a self-host or staging build that hasn't configured both
// renders nothing here at all, rather than a half sentence or a link into a
// 404. It is deliberately all-or-nothing — one URL without the other would
// either link "Terms" nowhere or assert a policy that doesn't exist.
export function SignInConsent({
  className,
  style,
}: {
  className?: string;
  style?: React.CSSProperties;
}) {
  if (!PRIVACY_URL || !TERMS_URL) return null;
  return (
    <p data-testid="sign-in-consent" className={className} style={style}>
      By signing in you agree to the{" "}
      <LegalAnchor href={TERMS_URL}>Terms</LegalAnchor> and{" "}
      <LegalAnchor href={PRIVACY_URL}>Privacy policy</LegalAnchor>.
    </p>
  );
}

function LegalAnchor({
  href,
  children,
}: {
  href: string;
  children: React.ReactNode;
}) {
  const style: React.CSSProperties = { textDecoration: "underline" };
  // Legal URLs are operator-supplied and may resolve outside this Next app
  // (the hosted deployment serves /privacy and /terms as static Caddy
  // routes, not Next pages) — always a plain <a>, never next/link's <Link>,
  // which would prefetch the route's RSC payload and 404 against a path
  // Next doesn't own. See lib/site.ts's legalFooterLinks for the full story.
  if (href.startsWith("/")) {
    return (
      <a href={href} style={style}>
        {children}
      </a>
    );
  }
  return (
    <a href={href} target="_blank" rel="noopener noreferrer" style={style}>
      {children}
    </a>
  );
}
