import Link from "next/link";
import { legalFooterLinks } from "../../lib/site";

// Privacy/Terms links for a public-page footer that doesn't already carry
// its own link array (blog, docs, mcp — the landing page instead spreads
// legalFooterLinks() straight into its own FOOTER_LINKS). Renders nothing
// when neither PRIVACY_URL nor TERMS_URL is configured, so a self-host or
// staging footer looks exactly as it did before this existed.
export function LegalFooterLinks({
  className,
  linkStyle,
}: {
  className?: string;
  linkStyle?: React.CSSProperties;
}) {
  const links = legalFooterLinks();
  if (links.length === 0) return null;
  return (
    <div className={className}>
      {links.map((l) =>
        l.external ? (
          <a
            key={l.label}
            href={l.href}
            target="_blank"
            rel="noopener noreferrer"
            style={linkStyle}
          >
            {l.label}
          </a>
        ) : (
          <Link key={l.label} href={l.href} style={linkStyle}>
            {l.label}
          </Link>
        ),
      )}
    </div>
  );
}
