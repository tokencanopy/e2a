import { legalFooterLinks } from "../../lib/site";

// Privacy/Terms links for a public-page footer that doesn't already carry
// its own link array (blog, docs, mcp — the landing page instead spreads
// legalFooterLinks() straight into its own FOOTER_LINKS). Renders nothing
// when neither PRIVACY_URL nor TERMS_URL is configured, so a self-host or
// staging footer looks exactly as it did before this existed.
//
// Every entry from legalFooterLinks() is `plain` — always a bare <a>, never
// next/link's <Link> — because these are operator-supplied URLs that may
// resolve outside this Next app (see the comment on legalFooterLinks in
// lib/site.ts). Only `external` varies the target/rel.
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
      {links.map((l) => (
        <a
          key={l.label}
          href={l.href}
          target={l.external ? "_blank" : undefined}
          rel={l.external ? "noopener noreferrer" : undefined}
          style={linkStyle}
        >
          {l.label}
        </a>
      ))}
    </div>
  );
}
