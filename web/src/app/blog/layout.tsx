import type { Metadata } from "next";
import Link from "next/link";
import { SITE_URL } from "../../lib/site";
import { LegalFooterLinks } from "../components/LegalFooterLinks";

const TITLE = "Blog — e2a, email for AI agents";
const DESC =
  "Notes on building authenticated email for AI agents — tutorials, protocol deep-dives, and product updates from the e2a team.";

export const metadata: Metadata = {
  title: { absolute: TITLE },
  description: DESC,
  alternates: { canonical: "/blog" },
  openGraph: {
    title: TITLE,
    description: DESC,
    url: `${SITE_URL}/blog`,
    type: "website",
  },
  twitter: {
    card: "summary_large_image",
    title: TITLE,
    description: DESC,
  },
};

export default function BlogLayout({ children }: { children: React.ReactNode }) {
  return (
    <div
      className="min-h-screen"
      style={{
        background: "var(--bg)",
        color: "var(--fg)",
        fontFamily: "var(--f-ui)",
      }}
    >
      <nav
        className="sticky top-0 z-50"
        style={{
          background: "var(--bg)",
          borderBottom: "1px solid var(--border)",
        }}
      >
        <div className="max-w-[760px] mx-auto flex items-center justify-between px-6 md:px-8 py-3.5">
          <Link
            href="/"
            className="font-mono font-bold text-[15px]"
            style={{ color: "var(--fg)", letterSpacing: "-0.02em" }}
          >
            e2a
          </Link>
          <div className="flex items-center gap-4">
            <Link
              href="/blog"
              className="text-[13px]"
              style={{ color: "var(--fg-muted)" }}
            >
              Blog
            </Link>
            <Link
              href="/api-docs"
              className="text-[13px]"
              style={{ color: "var(--fg-muted)" }}
            >
              Docs
            </Link>
            <Link
              href="/get-started"
              className="inline-flex items-center gap-1.5 px-3.5 py-1.5 text-[13px] font-medium"
              style={{
                background: "var(--fg)",
                color: "var(--bg)",
                borderRadius: "var(--r-md)",
              }}
            >
              Start building <span className="font-mono">→</span>
            </Link>
          </div>
        </div>
      </nav>
      <main className="max-w-[720px] mx-auto px-6 md:px-8 py-14 md:py-[56px] pb-20">
        {children}
      </main>
      <footer
        className="max-w-[720px] mx-auto flex justify-between px-6 md:px-8 py-5"
        style={{ borderTop: "1px solid var(--border)" }}
      >
        <span
          className="font-mono font-bold text-[13px]"
          style={{ color: "var(--fg)", letterSpacing: "-0.02em" }}
        >
          e2a
        </span>
        <LegalFooterLinks
          className="flex gap-3 text-[12px]"
          linkStyle={{ color: "var(--fg-muted)" }}
        />
        <span
          className="font-mono text-[12px]"
          style={{ color: "var(--fg-subtle)" }}
        >
          Apache 2.0
        </span>
      </footer>
    </div>
  );
}
