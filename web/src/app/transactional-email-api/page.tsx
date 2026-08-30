import type { Metadata } from "next";
import Link from "next/link";
import { JsonLd } from "../components/JsonLd";
import { PRICING_PATH, SITE_URL } from "../../lib/site";
import {
  breadcrumbs,
  faqPage,
  softwareApplication,
  type FaqEntry,
} from "../../lib/jsonld";

const TITLE = "Transactional Email API for Applications | e2a";
const DESC =
  "Use e2a's open-source transactional email API to send receipts, verification messages, product notifications, and alerts from any application. No AI agent or agent framework is required.";

export const metadata: Metadata = {
  title: { absolute: TITLE },
  description: DESC,
  keywords: [
    "transactional email API",
    "open source email API",
    "email API for developers",
    "send email with TypeScript",
    "send email with Python",
    "self-hosted email API",
  ],
  alternates: { canonical: "/transactional-email-api" },
  openGraph: {
    title: TITLE,
    description: DESC,
    url: `${SITE_URL}/transactional-email-api`,
    type: "website",
    images: [{ url: "/og-image.png", width: 1200, height: 630 }],
  },
  twitter: { card: "summary_large_image", title: TITLE, description: DESC },
};

const FAQ: FaqEntry[] = [
  {
    question: "Can e2a be used as a general transactional email API?",
    answer:
      "Yes. Any application can use e2a's HTTP API and TypeScript or Python SDKs to send transactional email. Agent inboxes, inbound email, threading, and human approval are additional capabilities—not requirements.",
  },
  {
    question: "Do I need an AI agent or agent framework to use e2a?",
    answer:
      "No. A conventional backend, scheduled job, serverless function, command-line script, or internal service can call e2a directly. An AI runtime is optional and is only needed when your product actually contains an agent.",
  },
  {
    question: "Can I self-host the transactional email API?",
    answer:
      "Yes. e2a is Apache-2.0 open source, including the Go server, SMTP relay, TypeScript and Python SDKs, CLI, MCP server, and dashboard. You can run it with your own Postgres database and outbound SMTP relay, or use the hosted service.",
  },
];

const CAPABILITIES = [
  ["Application email", "Send receipts, verification messages, alerts, and product notifications from your backend."],
  ["Typed integrations", "Use the TypeScript or Python SDK, or call the documented HTTP API from any language."],
  ["Two-way when needed", "Add inbound email, persistent threads, signed webhooks, and WebSocket delivery without changing providers."],
  ["Human approval", "Optionally hold sensitive outbound messages for a person to approve, edit, or reject."],
] as const;

export default function TransactionalEmailApiPage() {
  return <TransactionalEmailApiPageContent pricingPath={PRICING_PATH} />;
}

export function TransactionalEmailApiPageContent({ pricingPath }: { pricingPath: string }) {
  return (
    <div
      className="min-h-screen"
      style={{ background: "var(--bg)", color: "var(--fg)", fontFamily: "var(--f-ui)" }}
    >
      <JsonLd
        data={[
          softwareApplication(DESC),
          faqPage(FAQ),
          breadcrumbs([
            { name: "e2a", path: "/" },
            { name: "Transactional email API", path: "/transactional-email-api" },
          ]),
        ]}
      />

      <nav
        className="sticky top-0 z-50"
        style={{
          background: "color-mix(in srgb, var(--bg) 88%, transparent)",
          borderBottom: "1px solid var(--border)",
        }}
      >
        <div className="max-w-[960px] mx-auto flex items-center justify-between px-6 md:px-8 py-3.5">
          <Link href="/" className="font-mono font-bold text-[15px]" style={{ color: "var(--fg)" }}>
            e2a
          </Link>
          <div className="flex items-center gap-4 text-[13px]">
            <Link href="/email-api-for-ai-agents" className="hidden sm:inline" style={{ color: "var(--fg-muted)" }}>
              Agent inboxes
            </Link>
            <Link href="/api-docs" className="hidden sm:inline" style={{ color: "var(--fg-muted)" }}>
              API docs
            </Link>
            <Link
              href="/get-started?step=address"
              className="px-3.5 py-1.5 font-medium"
              style={{ background: "var(--fg)", color: "var(--bg)", borderRadius: "var(--r-md)" }}
            >
              Create sender
            </Link>
          </div>
        </div>
      </nav>

      <main className="max-w-[960px] mx-auto px-6 md:px-8 py-16 md:py-24 pb-24">
        <section className="max-w-[820px]">
          <p className="font-mono text-[11px] mb-4" style={{ color: "var(--accent-strong)", letterSpacing: "0.08em" }}>
            TRANSACTIONAL EMAIL API
          </p>
          <h1
            className="text-[42px] md:text-[62px] leading-[1.04] mb-6"
            style={{ fontFamily: "var(--f-editorial)", fontWeight: 400, letterSpacing: "-0.025em" }}
          >
            The open-source transactional email API for applications.
          </h1>
          <p className="text-[18px] leading-[1.65] max-w-[760px] mb-4" style={{ color: "var(--fg-muted)" }}>
            Send receipts, verification messages, product notifications, and alerts from the software you already run. No AI agent or agent framework is required.
          </p>
          <p className="text-[14px] leading-[1.65] max-w-[720px] mb-8" style={{ color: "var(--fg-muted)" }}>
            Start with outbound email over HTTP, TypeScript, or Python. Add replies, threading, approval, or an agent-owned inbox only when your product needs them.
          </p>
          <div className="flex flex-wrap gap-2.5">
            <Link
              href="/get-started?step=address"
              className="inline-flex items-center gap-2 px-4 py-2.5 text-[14px] font-medium"
              style={{ background: "var(--accent-fill)", color: "var(--accent-fg)", borderRadius: "var(--r-md)" }}
            >
              Create a sender <span className="font-mono">→</span>
            </Link>
            <Link
              href="/api-docs"
              className="inline-flex items-center px-4 py-2.5 text-[14px] font-medium"
              style={{ background: "var(--bg-panel)", color: "var(--fg)", border: "1px solid var(--border)", borderRadius: "var(--r-md)" }}
            >
              Read the API docs
            </Link>
            {pricingPath && (
              <Link
                href={pricingPath}
                className="inline-flex items-center px-4 py-2.5 text-[14px] font-medium"
                style={{ color: "var(--fg-muted)" }}
              >
                View pricing
              </Link>
            )}
            <a
              href="https://github.com/tokencanopy/e2a"
              className="inline-flex items-center px-4 py-2.5 text-[14px] font-medium"
              style={{ color: "var(--fg-muted)" }}
            >
              View source
            </a>
          </div>
        </section>

        <section className="mt-16 md:mt-24" aria-labelledby="capabilities-heading">
          <h2 id="capabilities-heading" className="text-[30px] md:text-[38px] font-semibold mb-7" style={{ letterSpacing: "-0.035em" }}>
            Start simple. Keep the richer email workflow available.
          </h2>
          <div className="grid sm:grid-cols-2 gap-3">
            {CAPABILITIES.map(([title, description]) => (
              <div key={title} className="p-6" style={{ background: "var(--bg-panel)", border: "1px solid var(--border)", borderRadius: "var(--r-lg)" }}>
                <h3 className="text-[17px] font-semibold mb-2">{title}</h3>
                <p className="text-[14px] leading-[1.65]" style={{ color: "var(--fg-muted)" }}>{description}</p>
              </div>
            ))}
          </div>
        </section>

        <section className="mt-16 md:mt-24 grid md:grid-cols-[1fr_1fr] gap-8 md:gap-12 items-start" aria-labelledby="resource-heading">
          <div>
            <p className="font-mono text-[11px] mb-3" style={{ color: "var(--accent-strong)", letterSpacing: "0.08em" }}>
              ONE RESOURCE MODEL
            </p>
            <h2 id="resource-heading" className="text-[28px] md:text-[36px] font-semibold mb-4" style={{ letterSpacing: "-0.03em" }}>
              A sender now. An inbox when you need one.
            </h2>
            <p className="text-[15px] leading-[1.7]" style={{ color: "var(--fg-muted)" }}>
              In the e2a API, each sending identity is represented by an agent resource. For a conventional application, treat it as the sender and optional inbox; the name describes the resource, not a requirement to run AI. That same identity can later receive replies or participate in an automated workflow without a second email system.
            </p>
          </div>
          <pre
            className="overflow-x-auto p-5 text-[12px] leading-[1.7]"
            style={{ background: "var(--code-bg)", color: "var(--code-fg)", border: "1px solid var(--border)", borderRadius: "var(--r-lg)" }}
          >
            <code>{`await client.messages.send("receipts@example.com", {
  to: [{ email: "customer@example.net" }],
  subject: "Your receipt",
  text: "Thanks for your order.",
});`}</code>
          </pre>
        </section>

        <section className="mt-16 md:mt-24" aria-labelledby="faq-heading">
          <h2 id="faq-heading" className="text-[28px] md:text-[34px] font-semibold mb-6" style={{ letterSpacing: "-0.03em" }}>
            Transactional email API: FAQ
          </h2>
          <div style={{ borderTop: "1px solid var(--border)" }}>
            {FAQ.map((entry) => (
              <div key={entry.question} className="py-5" style={{ borderBottom: "1px solid var(--border)" }}>
                <h3 className="text-[16px] font-semibold mb-2">{entry.question}</h3>
                <p className="text-[14px] leading-[1.7]" style={{ color: "var(--fg-muted)" }}>{entry.answer}</p>
              </div>
            ))}
          </div>
        </section>

        <section className="mt-16 md:mt-24 p-8 md:p-12 text-center" style={{ background: "var(--bg-elev)", border: "1px solid var(--border)", borderRadius: "var(--r-lg)" }}>
          <h2 className="text-[28px] font-semibold mb-3">Send your first application email.</h2>
          <p className="text-[15px] mb-6" style={{ color: "var(--fg-muted)" }}>
            Start free with the hosted service, or run the Apache-2.0 stack yourself.
          </p>
          <Link href="/get-started?step=address" className="inline-flex items-center gap-2 px-4 py-2.5 text-[14px] font-medium" style={{ background: "var(--fg)", color: "var(--bg)", borderRadius: "var(--r-md)" }}>
            Create a sender <span className="font-mono">→</span>
          </Link>
        </section>
      </main>
    </div>
  );
}
