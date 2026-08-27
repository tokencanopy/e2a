import type { Metadata } from "next";
import { SITE_URL } from "../../../lib/site";

const TITLE = "Python SDK docs — send email from your AI agent";
const DESC =
  "Python SDK guide for e2a. Install the e2a package, send transactional email, receive mail with structured domain evidence, use WebSocket delivery, and thread conversations across agents and humans.";

export const metadata: Metadata = {
  title: { absolute: TITLE },
  description: DESC,
  alternates: { canonical: "/docs/python" },
  openGraph: {
    title: TITLE,
    description: DESC,
    url: `${SITE_URL}/docs/python`,
    type: "article",
  },
  twitter: {
    card: "summary_large_image",
    title: TITLE,
    description: DESC,
  },
};

export default function DocsPythonLayout({ children }: { children: React.ReactNode }) {
  return <>{children}</>;
}
