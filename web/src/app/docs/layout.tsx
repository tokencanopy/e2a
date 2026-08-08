import type { Metadata } from "next";
import { SITE_URL } from "../../lib/site";

const TITLE = "Docs — e2a email API for AI agents";
// Describes what this page actually answers — connecting over MCP/REST/SDK,
// the credential model, webhook vs WebSocket delivery, threading, idempotency
// — rather than the whole product. A description that overshoots its page is
// the one search engines rewrite.
const DESC =
  "Developer docs for e2a, the authenticated email API for AI agents: connect over MCP, REST, or the TypeScript and Python SDKs; API keys and OAuth 2.1; webhook vs WebSocket delivery; conversation threading and idempotent sends.";

export const metadata: Metadata = {
  title: { absolute: TITLE },
  description: DESC,
  alternates: { canonical: "/docs" },
  openGraph: {
    title: TITLE,
    description: DESC,
    url: `${SITE_URL}/docs`,
    type: "article",
  },
  twitter: {
    card: "summary_large_image",
    title: TITLE,
    description: DESC,
  },
};

export default function DocsLayout({ children }: { children: React.ReactNode }) {
  return <>{children}</>;
}
