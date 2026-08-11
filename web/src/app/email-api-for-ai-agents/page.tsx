import type { Metadata } from "next";
import Link from "next/link";
import { SITE_URL } from "../../lib/site";
import { breadcrumbs, faqPage, type FaqEntry, softwareApplication } from "../../lib/jsonld";
import { JsonLd } from "../components/JsonLd";

const TITLE = "Email API for AI Agents | e2a";
const DESC =
  "e2a is open-source email infrastructure for AI agents. Give agents real, authenticated inboxes to send, receive, and reply to email through an API, SDK, webhook, WebSocket, or MCP.";

export const metadata: Metadata = {
  title: { absolute: TITLE },
  description: DESC,
  keywords: [
    "email API for AI agents",
    "email infrastructure for AI agents",
    "email service for AI agents",
    "AI agent inbox",
    "build an email agent",
    "MCP email server",
  ],
  alternates: { canonical: "/email-api-for-ai-agents" },
  openGraph: {
    title: TITLE,
    description: DESC,
    url: `${SITE_URL}/email-api-for-ai-agents`,
    type: "website",
    images: [{ url: "/og-image.png", width: 1200, height: 630 }],
  },
  twitter: { card: "summary_large_image", title: TITLE, description: DESC },
};

const FAQ: FaqEntry[] = [
  {
    question: "What is an email API for AI agents?",
    answer:
      "An email API for AI agents gives an agent its own email identity and the programmatic ability to receive, send, and reply to messages. e2a adds authenticated inbound delivery, in-thread replies, multiple integration surfaces, and an optional human approval gate for outbound actions.",
  },
  {
    question: "What is the difference between e2a and a transactional email API?",
    answer:
      "A transactional email API primarily sends application mail. e2a gives an AI agent a two-way inbox of its own: it can receive requests, preserve conversation threads, reply from its identity, and hand sensitive actions to a human for approval.",
  },
  {
    question: "What can I build with an AI agent email API?",
    answer:
      "Developers use e2a to build support agents, AI receptionists, scheduling agents, ecommerce agents, sales and follow-up agents, recruiting agents, voice follow-up agents, and procurement agents that work through persistent email conversations.",
  },
  {
    question: "How does e2a authenticate inbound email?",
    answer:
      "e2a evaluates SPF, DKIM, and DMARC on inbound messages and delivers the authentication result as structured evidence. Webhook deliveries are signed so the receiving service can verify that the event came from e2a before invoking an agent.",
  },
];

export default function EmailApiForAgentsPage() {
  return (
    <div className="min-h-screen" style={{ background: "var(--bg)", color: "var(--fg)", fontFamily: "var(--f-ui)" }}>
      <JsonLd
        data={[
          softwareApplication(DESC),
          faqPage(FAQ),
          breadcrumbs([
            { name: "e2a", path: "/" },
            { name: "Email API for AI agents", path: "/email-api-for-ai-agents" },
          ]),
        ]}
      />

      <nav
        className="sticky top-0 z-50"
        style={{ background: "color-mix(in srgb, var(--bg) 88%, transparent)", borderBottom: "1px solid var(--border)", backdropFilter: "blur(12px)" }}
      >
        <div className="max-w-[920px] mx-auto flex items-center justify-between px-6 md:px-8 py-3.5">
          <Link href="/" className="font-mono font-bold text-[17px]">e2a</Link>
          <div className="flex items-center gap-4 text-[13px]">
            <Link href="/use-cases" style={{ color: "var(--fg-muted)" }}>Use cases</Link>
            <Link href="/compare/e2a-vs-agentmail" className="hidden sm:inline" style={{ color: "var(--fg-muted)" }}>Compare</Link>
            <Link href="/get-started" className="px-3.5 py-1.5 font-medium" style={{ background: "var(--fg)", color: "var(--bg)", borderRadius: "var(--r-md)" }}>
              Start building <span className="font-mono">→</span>
            </Link>
          </div>
        </div>
      </nav>

      <main className="max-w-[920px] mx-auto px-6 md:px-8 py-14 md:py-20 pb-24">
        <header className="max-w-[760px] mb-14 md:mb-20">
          <p className="font-mono text-[11px] mb-4" style={{ color: "var(--accent-strong)", letterSpacing: "0.08em" }}>
            EMAIL INFRASTRUCTURE FOR AI AGENTS
          </p>
          <h1 className="text-[40px] md:text-[60px] font-semibold leading-[1.04] mb-6" style={{ letterSpacing: "-0.045em" }}>
            Build agents that work over email.
          </h1>
          <p className="text-[18px] md:text-[21px] leading-[1.55]" style={{ color: "var(--fg-muted)" }}>
            e2a gives every agent a real, authenticated inbox. Receive requests,
            take action, reply in-thread, and bring in a human when the action
            matters.
          </p>
        </header>

        <section className="grid md:grid-cols-3 gap-3" aria-labelledby="primitives-heading">
          <h2 id="primitives-heading" className="sr-only">What e2a provides</h2>
          {[
            ["01", "An agent identity", "A real address that belongs to the agent, not a human mailbox it happens to read."],
            ["02", "A two-way channel", "Receive authenticated mail and send replies that stay in the same conversation."],
            ["03", "A safer action boundary", "Hold outbound mail for human approval and pass sender evidence into the agent."],
          ].map(([number, title, text]) => (
            <div key={number} className="p-6" style={{ background: "var(--bg-panel)", border: "1px solid var(--border)", borderRadius: "var(--r-lg)" }}>
              <div className="font-mono text-[11px] mb-5" style={{ color: "var(--accent-strong)" }}>{number}</div>
              <h2 className="text-[18px] font-semibold mb-2" style={{ letterSpacing: "-0.02em" }}>{title}</h2>
              <p className="text-[14px] leading-[1.6]" style={{ color: "var(--fg-muted)" }}>{text}</p>
            </div>
          ))}
        </section>

        <section className="mt-16 md:mt-24" aria-labelledby="build-heading">
          <div className="flex items-end justify-between gap-4 mb-6">
            <div>
              <p className="font-mono text-[11px] mb-3" style={{ color: "var(--accent-strong)", letterSpacing: "0.08em" }}>START WITH A WORKFLOW</p>
              <h2 id="build-heading" className="text-[30px] md:text-[38px] font-semibold" style={{ letterSpacing: "-0.035em" }}>Build the agent your work needs.</h2>
            </div>
            <Link href="/use-cases" className="hidden sm:inline text-[13px]" style={{ color: "var(--accent-strong)" }}>See all use cases →</Link>
          </div>
          <div className="grid sm:grid-cols-2 gap-3">
            {[
              ["Support", "Answer customer requests, retrieve context, and escalate sensitive cases.", "/use-cases/support-agent"],
              ["Reception", "Route inquiries and hand conversations to the right person or team.", "/use-cases/ai-receptionist"],
              ["Scheduling", "Coordinate participants and preserve state across every reply.", "/use-cases/scheduling-agent"],
              ["Ecommerce", "Answer order questions and route returns, refunds, and changes to a human.", "/use-cases/ecommerce-agent"],
              ["Sales and GTM", "Start approved conversations, receive replies, qualify interest, and hand off opportunities.", "/use-cases/sales-agent"],
              ["Procurement", "Chase purchase orders and manage supplier conversations over multiple days.", "/use-cases/procurement-agent"],
            ].map(([title, text, href]) => (
              <Link key={href} href={href} className="group p-6" style={{ background: "var(--bg-panel)", border: "1px solid var(--border)", borderRadius: "var(--r-lg)" }}>
                <h3 className="text-[17px] font-semibold mb-2" style={{ letterSpacing: "-0.02em" }}>{title} <span className="font-mono text-[14px] group-hover:translate-x-0.5 inline-block transition-transform">→</span></h3>
                <p className="text-[14px] leading-[1.6]" style={{ color: "var(--fg-muted)" }}>{text}</p>
              </Link>
            ))}
          </div>
        </section>

        <section className="mt-16 md:mt-24" aria-labelledby="integrate-heading">
          <h2 id="integrate-heading" className="text-[28px] md:text-[34px] font-semibold mb-4" style={{ letterSpacing: "-0.03em" }}>Use the runtime you already have.</h2>
          <p className="text-[15px] leading-[1.65] max-w-[680px] mb-7" style={{ color: "var(--fg-muted)" }}>
            Connect an agent over the hosted MCP server, Python or TypeScript
            SDK, signed webhooks, WebSocket, REST, or the CLI. e2a is the email
            layer; your agent framework remains yours.
          </p>
          <div className="flex flex-wrap gap-2">
            {["MCP", "Python SDK", "TypeScript SDK", "Webhooks", "WebSocket", "REST", "CLI"].map((label) => (
              <span key={label} className="px-3 py-1.5 font-mono text-[12px]" style={{ color: "var(--fg-muted)", border: "1px solid var(--border)", borderRadius: "999px" }}>{label}</span>
            ))}
          </div>
        </section>

        <section className="mt-16 md:mt-24" aria-labelledby="faq-heading">
          <h2 id="faq-heading" className="text-[28px] md:text-[34px] font-semibold mb-6" style={{ letterSpacing: "-0.03em" }}>Email API for AI agents: FAQ</h2>
          <div className="divide-y" style={{ borderTop: "1px solid var(--border)", borderColor: "var(--border)" }}>
            {FAQ.map((entry) => (
              <details key={entry.question} className="py-5 group">
                <summary className="cursor-pointer list-none flex items-center justify-between gap-4 text-[16px] font-medium">
                  {entry.question}
                  <span className="font-mono text-[18px] transition-transform group-open:rotate-45" style={{ color: "var(--accent-strong)" }}>+</span>
                </summary>
                <p className="mt-3 max-w-[760px] text-[14px] leading-[1.7]" style={{ color: "var(--fg-muted)" }}>{entry.answer}</p>
              </details>
            ))}
          </div>
        </section>

        <section className="mt-16 md:mt-24 text-center p-8 md:p-12" style={{ background: "var(--bg-elev)", border: "1px solid var(--border)", borderRadius: "var(--r-lg)" }}>
          <h2 className="text-[28px] font-semibold mb-3" style={{ letterSpacing: "-0.025em" }}>Give your agent an inbox.</h2>
          <p className="text-[15px] mb-6" style={{ color: "var(--fg-muted)" }}>Start free, use the framework you already know, and build from a real workflow.</p>
          <Link href="/get-started" className="inline-flex items-center gap-2 px-4 py-2.5 text-[14px] font-medium" style={{ background: "var(--fg)", color: "var(--bg)", borderRadius: "var(--r-md)" }}>
            Start building <span className="font-mono">→</span>
          </Link>
        </section>
      </main>
    </div>
  );
}
