import type { Metadata } from "next";
import { SITE_URL } from "../../../lib/site";

const TITLE = "Python SDK docs — send transactional email with e2a";
const DESC =
  "Python SDK guide for e2a. Send transactional email from any application without an agent framework, or receive mail with structured domain evidence, WebSocket delivery, and threaded conversations.";

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
