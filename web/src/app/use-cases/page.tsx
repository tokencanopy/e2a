import type { Metadata } from "next";
import Link from "next/link";
import { SITE_URL } from "../../lib/site";
import { breadcrumbs, type JsonLdNode } from "../../lib/jsonld";
import { JsonLd } from "../components/JsonLd";

const TITLE = "What can you build with e2a? — AI agent email use cases";
const DESC =
  "Explore what developers can build with e2a: support agents, AI receptionists, scheduling agents, e-commerce agents, and more with real authenticated email inboxes.";

export const metadata: Metadata = {
  title: { absolute: TITLE },
  description: DESC,
  keywords: [
    "AI agent email use cases",
    "what can AI agents do with email",
    "build an email agent",
    "AI agent inbox use cases",
    "email automation for AI agents",
  ],
  alternates: { canonical: "/use-cases" },
  openGraph: {
    title: TITLE,
    description: DESC,
    url: `${SITE_URL}/use-cases`,
    type: "website",
    images: [{ url: "/og-image.png", width: 1200, height: 630 }],
  },
  twitter: { card: "summary_large_image", title: TITLE, description: DESC },
};

const USE_CASES: Array<{
  slug: string;
  eyebrow: string;
  title: string;
  description: string;
  comingSoon?: boolean;
}> = [
  {
    slug: "support-agent",
    eyebrow: "SUPPORT",
    title: "AI support agent",
    description:
      "Triage customer requests, retrieve context, reply in-thread, and hold sensitive responses for approval.",
  },
  {
    slug: "ai-receptionist",
    eyebrow: "RECEPTION",
    title: "AI receptionist",
    description:
      "Answer common inquiries, route messages, and forward conversations to the right person or team.",
  },
  {
    slug: "scheduling-agent",
    eyebrow: "SCHEDULING",
    title: "AI scheduling agent",
    description:
      "Coordinate participants, propose times, handle replies, and preserve state across a conversation.",
  },
  {
    slug: "ecommerce-agent",
    eyebrow: "E-COMMERCE",
    title: "E-commerce agent",
    description:
      "Answer order questions, handle returns, and coordinate with vendors through persistent email threads.",
  },
  {
    slug: "sales-agent",
    eyebrow: "SALES",
    title: "Sales and follow-up agent",
    description:
      "Qualify inbound interest, follow up with leads, and keep outreach moving with a verified identity.",
  },
  {
    slug: "recruiting-agent",
    eyebrow: "RECRUITING",
    title: "Recruiting agent",
    description:
      "Coordinate candidate conversations, schedule interviews, and keep hiring workflows in one thread.",
  },
  {
    slug: "voice-agent",
    eyebrow: "VOICE",
    title: "Voice follow-up agent",
    description:
      "Turn a completed voice call into an email follow-up, receive replies, and keep the conversation going.",
  },
  {
    slug: "procurement-agent",
    eyebrow: "PROCUREMENT",
    title: "Procurement agent",
    description:
      "Coordinate with vendors, chase purchase orders, and manage supplier threads that still run through email.",
  },
];

const collectionJsonLd: JsonLdNode = {
  "@context": "https://schema.org",
  "@type": "CollectionPage",
  "@id": `${SITE_URL}/use-cases#page`,
  name: TITLE,
  description: DESC,
  url: `${SITE_URL}/use-cases`,
  isPartOf: { "@id": `${SITE_URL}/#website` },
  mainEntity: {
    "@type": "ItemList",
    itemListElement: USE_CASES.filter((useCase) => !useCase.comingSoon).map((useCase, index) => ({
      "@type": "ListItem",
      position: index + 1,
      name: useCase.title,
      description: useCase.description,
      url: `${SITE_URL}/use-cases/${useCase.slug}`,
    })),
  },
};

export default function UseCasesPage() {
  return (
    <div
      className="min-h-screen"
      style={{
        background: "var(--bg)",
        color: "var(--fg)",
        fontFamily: "var(--f-ui)",
      }}
    >
      <JsonLd
        data={[
          collectionJsonLd,
          breadcrumbs([
            { name: "e2a", path: "/" },
            { name: "Use cases", path: "/use-cases" },
          ]),
        ]}
      />

      <nav
        className="sticky top-0 z-50"
        style={{
          background: "color-mix(in srgb, var(--bg) 88%, transparent)",
          borderBottom: "1px solid var(--border)",
          backdropFilter: "blur(12px)",
        }}
      >
        <div className="max-w-[920px] mx-auto flex items-center justify-between px-6 md:px-8 py-3.5">
          <Link href="/" className="font-mono font-bold text-[17px]">e2a</Link>
          <div className="flex items-center gap-4 text-[13px]">
            <Link href="/docs" style={{ color: "var(--fg-muted)" }}>Docs</Link>
            <Link
              href="/get-started"
              className="px-3.5 py-1.5 font-medium"
              style={{ background: "var(--fg)", color: "var(--bg)", borderRadius: "var(--r-md)" }}
            >
              Start building <span className="font-mono">→</span>
            </Link>
          </div>
        </div>
      </nav>

      <main className="max-w-[920px] mx-auto px-6 md:px-8 py-14 md:py-20 pb-24">
        <header className="max-w-[740px] mb-14 md:mb-20">
          <p className="font-mono text-[11px] mb-4" style={{ color: "var(--accent-strong)", letterSpacing: "0.08em" }}>
            USE CASES
          </p>
          <h1 className="text-[40px] md:text-[58px] font-semibold leading-[1.06] mb-6" style={{ letterSpacing: "-0.04em" }}>
            What can you build with e2a?
          </h1>
          <p className="text-[18px] md:text-[20px] leading-[1.55]" style={{ color: "var(--fg-muted)" }}>
            e2a gives every agent a real, authenticated email inbox. Use it to
            build agents that receive requests, take action, and keep people in
            the loop through the communication channel they already use.
          </p>
        </header>

        <section aria-labelledby="use-case-list">
          <h2 id="use-case-list" className="sr-only">AI agent email use cases</h2>
          <div className="grid sm:grid-cols-2 gap-3">
            {USE_CASES.map((useCase) => (
              <Link
                key={useCase.slug}
                href={useCase.comingSoon ? "/use-cases" : `/use-cases/${useCase.slug}`}
                className="group p-6 md:p-7"
                style={{
                  background: "var(--bg-panel)",
                  border: "1px solid var(--border)",
                  borderRadius: "var(--r-lg)",
                  opacity: useCase.comingSoon ? 0.7 : 1,
                  cursor: useCase.comingSoon ? "default" : "pointer",
                }}
              >
                <div className="font-mono text-[11px] font-semibold mb-3" style={{ color: "var(--accent-strong)", letterSpacing: "0.08em" }}>
                  {useCase.eyebrow}
                </div>
                <h3 className="text-[18px] font-semibold mb-2" style={{ letterSpacing: "-0.02em" }}>
                  {useCase.title} {!useCase.comingSoon && <span className="font-mono text-[15px] group-hover:translate-x-0.5 inline-block transition-transform">→</span>}
                </h3>
                <p className="text-[14px] leading-[1.6]" style={{ color: "var(--fg-muted)" }}>{useCase.description}</p>
                {useCase.comingSoon && <p className="font-mono text-[10px] mt-4" style={{ color: "var(--fg-subtle)", letterSpacing: "0.06em" }}>PAGE COMING NEXT</p>}
              </Link>
            ))}
          </div>
        </section>

        <section className="mt-16 md:mt-24 grid md:grid-cols-2 gap-8 md:gap-12">
          <div>
            <p className="font-mono text-[11px] mb-3" style={{ color: "var(--fg-subtle)", letterSpacing: "0.08em" }}>THE COMMON THREAD</p>
            <h2 className="text-[27px] font-semibold mb-4" style={{ letterSpacing: "-0.025em" }}>Email is the interface.</h2>
            <p className="text-[15px] leading-[1.65]" style={{ color: "var(--fg-muted)" }}>
              These agents all need the same underlying primitives: a dedicated
              identity, inbound authentication evidence, durable conversation
              context, and a safe way to send replies.
            </p>
          </div>
          <div className="p-6 md:p-7" style={{ background: "var(--bg-elev)", border: "1px solid var(--border)", borderRadius: "var(--r-lg)" }}>
            <ul className="space-y-3 text-[14px]" style={{ color: "var(--fg-muted)" }}>
              <li><span style={{ color: "var(--accent-strong)" }}>01</span> Give the agent its own inbox.</li>
              <li><span style={{ color: "var(--accent-strong)" }}>02</span> Deliver mail over MCP, SDK, webhook, WebSocket, or REST.</li>
              <li><span style={{ color: "var(--accent-strong)" }}>03</span> Let the agent act, reply, and preserve the thread.</li>
              <li><span style={{ color: "var(--accent-strong)" }}>04</span> Hold sensitive outbound actions for human approval.</li>
            </ul>
          </div>
        </section>

        <section className="mt-16 md:mt-24 text-center p-8 md:p-12" style={{ background: "var(--bg-elev)", border: "1px solid var(--border)", borderRadius: "var(--r-lg)" }}>
          <h2 className="text-[28px] font-semibold mb-3" style={{ letterSpacing: "-0.025em" }}>Have a workflow in mind?</h2>
          <p className="text-[15px] mb-6" style={{ color: "var(--fg-muted)" }}>Start with a free inbox and connect the agent runtime you already use.</p>
          <Link href="/get-started" className="inline-flex items-center gap-2 px-4 py-2.5 text-[14px] font-medium" style={{ background: "var(--fg)", color: "var(--bg)", borderRadius: "var(--r-md)" }}>
            Get started <span className="font-mono">→</span>
          </Link>
        </section>
      </main>
    </div>
  );
}
