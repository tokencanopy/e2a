import type { Metadata } from "next";
import Link from "next/link";
import { SITE_URL } from "../../../lib/site";
import { breadcrumbs, faqPage, type FaqEntry } from "../../../lib/jsonld";
import { JsonLd } from "../../components/JsonLd";

const TITLE = "e2a vs AgentMail vs Transactional Email APIs";
const DESC =
  "Compare e2a, AgentMail, Resend, Postmark, Mailgun, SendGrid, and Amazon SES for AI agent email infrastructure, inboxes, authentication, and gateway policy.";

export const metadata: Metadata = {
  title: { absolute: TITLE },
  description: DESC,
  keywords: [
    "AgentMail alternative",
    "e2a vs AgentMail",
    "email API for AI agents comparison",
    "transactional email API comparison",
    "email inbox API for AI agents",
    "email infrastructure for AI agents",
  ],
  alternates: { canonical: "/compare/e2a-vs-agentmail" },
  openGraph: {
    title: TITLE,
    description: DESC,
    url: `${SITE_URL}/compare/e2a-vs-agentmail`,
    type: "website",
    images: [{ url: "/og-image.png", width: 1200, height: 630 }],
  },
  twitter: { card: "summary_large_image", title: TITLE, description: DESC },
};

const FAQ: FaqEntry[] = [
  {
    question: "What is the difference between e2a and AgentMail?",
    answer:
      "Both provide email infrastructure for AI agents. AgentMail emphasizes programmable inboxes and mailbox features. e2a emphasizes an open-source, authenticated gateway with explicit inbound and outbound trust policies, review actions, content screening, and configurable hold behavior.",
  },
  {
    question: "Is e2a a transactional email API?",
    answer:
      "e2a can send transactional mail, but its primary model is a two-way agent inbox. Transactional APIs such as SES, Resend, Postmark, Mailgun, and SendGrid are generally chosen when an application needs to send product-triggered notifications and delivery events.",
  },
  {
    question: "Can e2a work with a transactional email provider?",
    answer:
      "Yes. e2a can use an upstream SMTP provider for outbound delivery while providing the agent identity, inbound receiver, authentication evidence, policy engine, and review workflow around it.",
  },
  {
    question: "Which email API is best for an AI agent?",
    answer:
      "Choose based on the job. Use a transactional API for one-way product notifications, an agent inbox platform for managed two-way mailbox workflows, or e2a when gateway-level inbound and outbound policy control, authenticated identity, open source, or self-hosting matter.",
  },
];

const TRANSACTIONAL_PRODUCTS = [
  ["Amazon SES", "Infrastructure-oriented delivery", "Low-level outbound email delivery for teams comfortable operating the surrounding application layer.", "https://aws.amazon.com/ses/"],
  ["Resend", "Developer-first sending", "A modern API-oriented choice for product and transactional email.", "https://resend.com/"],
  ["Postmark", "Transactional delivery", "Focused on product-triggered email and delivery visibility.", "https://postmarkapp.com/"],
  ["Mailgun", "Sending and routing", "Flexible email sending, parsing, and routing tools for developers.", "https://www.mailgun.com/"],
  ["Twilio SendGrid", "High-volume platform", "Broad transactional and marketing email tooling at larger sending volumes.", "https://sendgrid.com/"],
] as const;

const COMPARISON_ROWS = [
  ["Outbound email", "Core", "Core", "Core"],
  ["Inbound email", "Core", "Core", "Varies by provider"],
  ["Persistent agent inboxes", "Core", "Core", "Usually application-built"],
  ["Threads and replies", "Core", "Core", "Usually application-built"],
  ["Webhooks / WebSockets / MCP", "Yes", "Yes", "Varies"],
  ["Inbound sender authentication evidence", "Documented", "Core e2a primitive", "Varies"],
  ["Inbound allowlist or domain policy", "Documented", "Yes", "Usually application-built"],
  ["Outbound recipient policy", "Documented", "Yes", "Usually application-built"],
  ["Flag / review / block actions", "Drafts, labels, and allowlists", "Gateway policy", "Usually application-built"],
  ["Review expiry behavior", "Application-managed", "Configurable hold TTL and expiry action", "Application-managed"],
  ["Open source / self-hosting", "Check current offering", "Apache-2.0 / self-hostable", "Varies"],
] as const;

export default function E2aVsAgentMailPage() {
  return (
    <div className="min-h-screen" style={{ background: "var(--bg)", color: "var(--fg)", fontFamily: "var(--f-ui)" }}>
      <JsonLd
        data={[
          faqPage(FAQ),
          breadcrumbs([
            { name: "e2a", path: "/" },
            { name: "Email API for AI agents", path: "/email-api-for-ai-agents" },
            { name: "e2a vs AgentMail", path: "/compare/e2a-vs-agentmail" },
          ]),
        ]}
      />

      <nav className="sticky top-0 z-50" style={{ background: "color-mix(in srgb, var(--bg) 88%, transparent)", borderBottom: "1px solid var(--border)", backdropFilter: "blur(12px)" }}>
        <div className="max-w-[960px] mx-auto flex items-center justify-between px-6 md:px-8 py-3.5">
          <Link href="/" className="font-mono font-bold text-[17px]">e2a</Link>
          <div className="flex items-center gap-4 text-[13px]">
            <Link href="/email-api-for-ai-agents" style={{ color: "var(--fg-muted)" }}>How it works</Link>
            <Link href="/get-started" className="px-3.5 py-1.5 font-medium" style={{ background: "var(--fg)", color: "var(--bg)", borderRadius: "var(--r-md)" }}>Start building <span className="font-mono">→</span></Link>
          </div>
        </div>
      </nav>

      <main className="max-w-[960px] mx-auto px-6 md:px-8 py-14 md:py-20 pb-24">
        <header className="max-w-[780px] mb-14 md:mb-20">
          <p className="font-mono text-[11px] mb-4" style={{ color: "var(--accent-strong)", letterSpacing: "0.08em" }}>EMAIL INFRASTRUCTURE COMPARISON</p>
          <h1 className="text-[38px] md:text-[58px] font-semibold leading-[1.04] mb-6" style={{ letterSpacing: "-0.045em" }}>e2a vs AgentMail vs transactional email APIs.</h1>
          <p className="text-[18px] md:text-[21px] leading-[1.55]" style={{ color: "var(--fg-muted)" }}>
            These products solve related but different problems. Transactional APIs
            send application mail. Agent inbox platforms give software a mailbox.
            e2a adds authenticated inbound and outbound gateway policy around an
            agent’s identity.
          </p>
          <p className="mt-5 text-[12px] font-mono" style={{ color: "var(--fg-subtle, var(--fg-muted))" }}>Last verified against public product documentation: August 9, 2026.</p>
        </header>

        <section aria-labelledby="categories-heading">
          <div className="grid md:grid-cols-3 gap-3">
            {[
              ["01", "Transactional APIs", "Send receipts, alerts, password resets, and other product-triggered messages.", "Resend, Postmark, Mailgun, SendGrid, Amazon SES"],
              ["02", "Agent inbox platforms", "Give an agent a persistent address for receiving, replying, and managing conversations.", "AgentMail, e2a"],
              ["03", "Gateway infrastructure", "Control identity, trust, screening, review, and delivery behavior at the mail boundary.", "e2a"],
            ].map(([number, title, text, examples]) => (
              <div key={number} className="p-6" style={{ background: "var(--bg-panel)", border: "1px solid var(--border)", borderRadius: "var(--r-lg)" }}>
                <div className="font-mono text-[11px] mb-5" style={{ color: "var(--accent-strong)" }}>{number}</div>
                <h2 id={number === "01" ? "categories-heading" : undefined} className="text-[18px] font-semibold mb-2" style={{ letterSpacing: "-0.02em" }}>{title}</h2>
                <p className="text-[14px] leading-[1.6] mb-4" style={{ color: "var(--fg-muted)" }}>{text}</p>
                <p className="font-mono text-[11px] leading-[1.6]" style={{ color: "var(--fg-subtle, var(--fg-muted))" }}>{examples}</p>
              </div>
            ))}
          </div>
        </section>

        <section className="mt-16 md:mt-24" aria-labelledby="matrix-heading">
          <p className="font-mono text-[11px] mb-3" style={{ color: "var(--accent-strong)", letterSpacing: "0.08em" }}>THE SHORT VERSION</p>
          <h2 id="matrix-heading" className="text-[30px] md:text-[38px] font-semibold mb-4" style={{ letterSpacing: "-0.035em" }}>Choose the layer your agent actually needs.</h2>
          <p className="max-w-[720px] text-[15px] leading-[1.65] mb-7" style={{ color: "var(--fg-muted)" }}>
            “Varies” and “usually application-built” are intentional: providers
            change quickly, and a missing feature in public documentation is not
            proof that a provider cannot support it.
          </p>
          <div className="overflow-x-auto" style={{ border: "1px solid var(--border)", borderRadius: "var(--r-lg)" }}>
            <table className="w-full min-w-[720px] text-left text-[13px]">
              <thead style={{ background: "var(--bg-panel)" }}>
                <tr>
                  <th className="p-4 font-semibold">Capability</th>
                  <th className="p-4 font-semibold">AgentMail</th>
                  <th className="p-4 font-semibold">e2a</th>
                  <th className="p-4 font-semibold">Transactional APIs</th>
                </tr>
              </thead>
              <tbody>
                {COMPARISON_ROWS.map(([capability, agentMail, e2a, transactional], index) => (
                  <tr key={capability} style={{ borderTop: "1px solid var(--border)", background: index % 2 ? "var(--bg-panel)" : "transparent" }}>
                    <th scope="row" className="p-4 font-medium">{capability}</th>
                    <td className="p-4" style={{ color: "var(--fg-muted)" }}>{agentMail}</td>
                    <td className="p-4" style={{ color: "var(--accent-strong)" }}>{e2a}</td>
                    <td className="p-4" style={{ color: "var(--fg-muted)" }}>{transactional}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <p className="mt-4 text-[12px] leading-[1.6]" style={{ color: "var(--fg-subtle, var(--fg-muted))" }}>
            AgentMail references: <a href="https://docs.agentmail.to/knowledge-base/what-is-agentmail" className="underline">capabilities</a> and <a href="https://docs.agentmail.to/knowledge-base/human-in-the-loop" className="underline">human-in-the-loop workflows</a>. Re-check provider features and pricing before making a purchasing decision.
          </p>
        </section>

        <section className="mt-16 md:mt-24" aria-labelledby="policy-heading">
          <div className="p-7 md:p-9" style={{ background: "var(--bg-elev)", border: "1px solid var(--border)", borderRadius: "var(--r-lg)" }}>
            <p className="font-mono text-[11px] mb-3" style={{ color: "var(--accent-strong)", letterSpacing: "0.08em" }}>WHERE e2a IS DIFFERENT</p>
            <h2 id="policy-heading" className="text-[28px] md:text-[36px] font-semibold mb-4" style={{ letterSpacing: "-0.03em" }}>Policy lives at the gateway.</h2>
            <p className="text-[15px] leading-[1.7] max-w-[760px]" style={{ color: "var(--fg-muted)" }}>
              e2a configures inbound and outbound trust independently. A policy
              can allow open traffic, match an address or domain allowlist, or
              restrict outbound mail to the agent’s own domain. A non-match can
              be flagged, held for review, or blocked. Content screening has its
              own inbound and outbound sensitivity, and held messages have a TTL,
              expiry action, and notification policy.
            </p>
            <Link href="/email-api-for-ai-agents" className="inline-flex mt-6 text-[13px]" style={{ color: "var(--accent-strong)" }}>Read how e2a works →</Link>
          </div>
        </section>

        <section className="mt-16 md:mt-24" aria-labelledby="providers-heading">
          <h2 id="providers-heading" className="text-[28px] md:text-[36px] font-semibold mb-6" style={{ letterSpacing: "-0.03em" }}>Transactional email services</h2>
          <div className="grid sm:grid-cols-2 gap-3">
            {TRANSACTIONAL_PRODUCTS.map(([name, label, text, href]) => (
              <a key={name} href={href} target="_blank" rel="noreferrer" className="p-5" style={{ background: "var(--bg-panel)", border: "1px solid var(--border)", borderRadius: "var(--r-lg)" }}>
                <div className="flex items-center justify-between gap-3 mb-2">
                  <h3 className="text-[16px] font-semibold">{name}</h3>
                  <span className="font-mono text-[11px]" style={{ color: "var(--accent-strong)" }}>{label}</span>
                </div>
                <p className="text-[14px] leading-[1.6]" style={{ color: "var(--fg-muted)" }}>{text}</p>
              </a>
            ))}
          </div>
        </section>

        <section className="mt-16 md:mt-24" aria-labelledby="faq-heading">
          <h2 id="faq-heading" className="text-[28px] md:text-[36px] font-semibold mb-6" style={{ letterSpacing: "-0.03em" }}>Email infrastructure comparison: FAQ</h2>
          <div className="divide-y" style={{ borderTop: "1px solid var(--border)", borderColor: "var(--border)" }}>
            {FAQ.map((entry) => (
              <details key={entry.question} className="py-5 group">
                <summary className="cursor-pointer list-none flex items-center justify-between gap-4 text-[16px] font-medium">{entry.question}<span className="font-mono text-[18px] transition-transform group-open:rotate-45" style={{ color: "var(--accent-strong)" }}>+</span></summary>
                <p className="mt-3 max-w-[760px] text-[14px] leading-[1.7]" style={{ color: "var(--fg-muted)" }}>{entry.answer}</p>
              </details>
            ))}
          </div>
        </section>

        <section className="mt-16 md:mt-24 text-center p-8 md:p-12" style={{ background: "var(--bg-elev)", border: "1px solid var(--border)", borderRadius: "var(--r-lg)" }}>
          <h2 className="text-[28px] font-semibold mb-3" style={{ letterSpacing: "-0.025em" }}>Build the agent, not the mail gateway.</h2>
          <p className="text-[15px] mb-6" style={{ color: "var(--fg-muted)" }}>Give your agent an authenticated inbox and keep policy at the boundary.</p>
          <Link href="/get-started" className="inline-flex items-center gap-2 px-4 py-2.5 text-[14px] font-medium" style={{ background: "var(--fg)", color: "var(--bg)", borderRadius: "var(--r-md)" }}>Start building <span className="font-mono">→</span></Link>
        </section>
      </main>
    </div>
  );
}
