import Link from "next/link";
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
  if (href.startsWith("/")) {
    return (
      <Link href={href} style={style}>
        {children}
      </Link>
    );
  }
  return (
    <a href={href} target="_blank" rel="noopener noreferrer" style={style}>
      {children}
    </a>
  );
}
