"use client";

import Link from "next/link";
import { useState } from "react";
import { useAuth } from "./components/AuthProvider";
import { Eyebrow } from "@e2a/ui";
import { JsonLd } from "./components/JsonLd";
import { TokenCanopyBadge } from "./components/loft/TokenCanopyBadge";
import { faqPage, type FaqEntry } from "../lib/jsonld";
import { PRICING_PATH, SIGN_IN_URL } from "../lib/site";

// Agent onboarding surfaces: install the plugin in a coding agent, or point
// any MCP runtime at the hosted server. Application integrations use the SDK
// examples below instead of choosing one of these tabs.
type Tab = "claude" | "codex" | "cursor" | "mcp";

// The two SDKs shown side by side in the application API section.
type SdkTab = "python" | "ts";

// Nav is three grouped menus rather than a flat row: at seven top-level
// items plus four social icons it wrapped onto two lines at common widths.
// Start stays a direct link so visitors can choose the application or agent
// path without first understanding e2a's resource model.
// `newTab` is for INTERNAL routes we still want opened in a new tab, so a
// visitor reading the landing page keeps their place instead of navigating
// away. External links always open in one.
type NavItem = {
  label: string;
  href: string;
  external?: boolean;
  newTab?: boolean;
};

const PRODUCT_LINKS: NavItem[] = [
  { label: "Transactional email", href: "/transactional-email-api" },
  { label: "Agent inboxes", href: "/email-api-for-ai-agents" },
  { label: "Human-in-the-loop", href: "#hitl" },
  { label: "Use cases", href: "/use-cases" },
  { label: "FAQ", href: "#faq" },
];

const RESOURCE_LINKS: NavItem[] = [
  { label: "API Reference", href: "/api-docs", newTab: true },
  { label: "Plugin", href: "https://github.com/tokencanopy/e2a/tree/main/plugins/e2a", external: true },
  { label: "Python SDK", href: "https://pypi.org/project/e2a/", external: true },
  { label: "TypeScript SDK", href: "https://www.npmjs.com/package/@e2a/sdk", external: true },
  { label: "CLI", href: "https://www.npmjs.com/package/@e2a/cli", external: true },
  { label: "Blog", href: "/blog", newTab: true },
];

const USE_CASES: { eyebrow: string; title: string; desc: string; href?: string }[] = [
  { eyebrow: "Support", title: "Support and intake", desc: "Triage inbound requests, answer common questions, and hand off to humans without changing how customers reach you.", href: "/use-cases/support-agent" },
  { eyebrow: "Admin", title: "Scheduling and admin", desc: "Coordinate meetings, send reminders, and follow up where most people already live — their inbox.", href: "/use-cases/scheduling-agent" },
  { eyebrow: "Reception", title: "AI receptionist", desc: "Answer common inquiries, route messages, and forward conversations to the right person.", href: "/use-cases/ai-receptionist" },
  { eyebrow: "Commerce", title: "E-commerce agents", desc: "Answer order questions, handle returns, and coordinate with vendors through persistent email threads.", href: "/use-cases/ecommerce-agent" },
  { eyebrow: "Sales", title: "Sales and follow-through", desc: "Qualify leads, reply to outreach, and keep conversations moving from an agent-owned address.", href: "/use-cases/sales-agent" },
  { eyebrow: "Recruiting", title: "Recruiting agents", desc: "Coordinate candidates, schedule interviews, and keep hiring conversations organized.", href: "/use-cases/recruiting-agent" },
  { eyebrow: "Auth", title: "OTP and verification", desc: "Receive verification codes, confirmation emails, and magic links — then act on them automatically." },
  { eyebrow: "Voice", title: "Voice agents", desc: "After a call ends, your voice agent sends a follow-up, receives a reply, and keeps the thread going." },
  { eyebrow: "Procurement", title: "Procurement", desc: "Coordinate with vendors, chase POs, and manage supplier threads with partners who still run on email." },
];

function UseCaseCardContent({
  useCase,
}: {
  useCase: (typeof USE_CASES)[number];
}) {
  return (
    <>
      <div
        className="font-mono text-[11px] font-semibold uppercase mb-2.5"
        style={{
          color: "var(--accent-strong)",
          letterSpacing: "0.08em",
        }}
      >
        {useCase.eyebrow}
      </div>
      <div
        className="text-[15px] font-semibold mb-1.5"
        style={{ color: "var(--fg)" }}
      >
        {useCase.title}
        {useCase.href ? <span className="font-mono ml-1.5" aria-hidden="true">→</span> : null}
      </div>
      <div
        className="text-[13px] leading-[1.6]"
        style={{ color: "var(--fg-muted)" }}
      >
        {useCase.desc}
      </div>
    </>
  );
}

// Token Canopy's channels, not e2a's — the accounts are the umbrella brand's,
// so they sit beside the "by Token Canopy" credit rather than in with the
// product's own docs/package links. Paths are the official brand marks.
const SOCIAL_LINKS: { label: string; href: string; aria: string; path: string }[] = [
  {
    label: "GitHub",
    href: "https://github.com/tokencanopy/e2a",
    aria: "View source on GitHub",
    path: "M12 .5C5.65.5.5 5.65.5 12c0 5.08 3.29 9.39 7.86 10.91.58.1.79-.25.79-.56 0-.27-.01-1.16-.02-2.1-3.2.7-3.87-1.36-3.87-1.36-.52-1.32-1.27-1.67-1.27-1.67-1.04-.71.08-.7.08-.7 1.15.08 1.76 1.18 1.76 1.18 1.02 1.76 2.69 1.25 3.34.96.1-.74.4-1.25.72-1.54-2.55-.29-5.24-1.28-5.24-5.69 0-1.26.45-2.29 1.18-3.1-.12-.29-.51-1.46.11-3.04 0 0 .96-.31 3.15 1.18a10.95 10.95 0 0 1 5.74 0c2.19-1.49 3.15-1.18 3.15-1.18.62 1.58.23 2.75.11 3.04.74.81 1.18 1.84 1.18 3.1 0 4.42-2.69 5.4-5.25 5.68.41.36.78 1.06.78 2.13 0 1.54-.01 2.78-.01 3.16 0 .31.21.67.8.56C20.21 21.39 23.5 17.08 23.5 12 23.5 5.65 18.35.5 12 .5z",
  },
  {
    label: "X",
    href: "https://x.com/TokenCanopy",
    aria: "Token Canopy on X",
    path: "M18.244 2.25h3.308l-7.227 8.26 8.502 11.24H16.17l-5.214-6.817L4.99 21.75H1.68l7.73-8.835L1.254 2.25H8.08l4.713 6.231zm-1.161 17.52h1.833L7.084 4.126H5.117z",
  },
  {
    label: "LinkedIn",
    href: "https://www.linkedin.com/company/tokencanopy/",
    aria: "Token Canopy on LinkedIn",
    path: "M20.447 20.452h-3.554v-5.569c0-1.328-.027-3.037-1.852-3.037-1.853 0-2.136 1.445-2.136 2.939v5.667H9.351V9h3.414v1.561h.046c.477-.9 1.637-1.85 3.37-1.85 3.601 0 4.267 2.37 4.267 5.455v6.286zM5.337 7.433a2.062 2.062 0 01-2.063-2.065 2.064 2.064 0 112.063 2.065zm1.782 13.019H3.555V9h3.564v11.452zM22.225 0H1.771C.792 0 0 .774 0 1.729v20.542C0 23.227.792 24 1.771 24h20.451C23.2 24 24 23.227 24 22.271V1.729C24 .774 23.2 0 22.222 0h.003z",
  },
  {
    label: "Discord",
    href: "https://discord.gg/EQTK2REXPb",
    aria: "Token Canopy on Discord",
    path: "M20.317 4.37a19.79 19.79 0 00-4.885-1.515.074.074 0 00-.079.037c-.21.375-.444.864-.608 1.25a18.27 18.27 0 00-5.487 0 12.64 12.64 0 00-.617-1.25.077.077 0 00-.079-.037A19.736 19.736 0 003.677 4.37a.07.07 0 00-.032.027C.533 9.046-.32 13.58.099 18.057a.082.082 0 00.031.057 19.9 19.9 0 005.993 3.03.078.078 0 00.084-.028 14.09 14.09 0 001.226-1.994.076.076 0 00-.041-.106 13.107 13.107 0 01-1.872-.892.077.077 0 01-.008-.128 10.2 10.2 0 00.372-.292.074.074 0 01.077-.01c3.928 1.793 8.18 1.793 12.062 0a.074.074 0 01.078.01c.12.098.246.198.373.292a.077.077 0 01-.006.127 12.299 12.299 0 01-1.873.892.077.077 0 00-.041.107c.36.698.772 1.362 1.225 1.993a.076.076 0 00.084.028 19.839 19.839 0 006.002-3.03.077.077 0 00.032-.054c.5-5.177-.838-9.674-3.549-13.66a.061.061 0 00-.031-.03zM8.02 15.33c-1.183 0-2.157-1.085-2.157-2.419 0-1.333.956-2.419 2.157-2.419 1.21 0 2.176 1.096 2.157 2.42 0 1.333-.956 2.418-2.157 2.418zm7.975 0c-1.183 0-2.157-1.085-2.157-2.419 0-1.333.955-2.419 2.157-2.419 1.21 0 2.176 1.096 2.157 2.42 0 1.333-.946 2.418-2.157 2.418z",
  },
];

const FOOTER_LINKS: { label: string; href: string; external?: boolean }[] = [
  { label: "GitHub", href: "https://github.com/tokencanopy/e2a", external: true },
  { label: "Transactional Email", href: "/transactional-email-api" },
  { label: "API Docs", href: "/api-docs" },
  { label: "Blog", href: "/blog" },
  { label: "Python SDK", href: "https://pypi.org/project/e2a/", external: true },
  { label: "TypeScript SDK", href: "https://www.npmjs.com/package/@e2a/sdk", external: true },
  { label: "CLI", href: "https://www.npmjs.com/package/@e2a/cli", external: true },
  { label: "Plugin", href: "https://github.com/tokencanopy/e2a/tree/main/plugins/e2a", external: true },
  { label: "Feedback", href: "/feedback" },
];

// The landing page's answer surface. Every entry is rendered twice: as visible
// prose below, and as FAQPage structured data — Google only honors FAQ markup
// whose answers are on the page, and an answer engine that quotes one of these
// will quote whichever copy it reached first, so they must be the same strings.
//
// Each answer is written to survive being lifted on its own, with none of the
// surrounding page attached: it names e2a rather than saying "we" or "it", and
// it states only what ships today. The questions are the ones people actually
// type; deliberately distinct from /mcp's, which answer "how do I connect".
const FAQ: FaqEntry[] = [
  {
    question: "Can e2a be used as a general transactional email API?",
    answer:
      "Yes. Any application can use e2a's HTTP API and TypeScript or Python SDKs to send transactional email. Agent inboxes, inbound email, threading, and human approval are additional capabilities—not requirements.",
  },
  {
    question: "Can an AI agent have its own email address?",
    answer:
      "Yes. e2a gives an AI agent its own dedicated, two-way email address on the shared agents.e2a.dev domain or on a domain you verify yourself. The inbox belongs to the agent rather than to a human whose mailbox the agent reads, so recipients can tell agent mail apart from yours and revoking the agent never touches anyone's personal mail. The agent reaches that inbox over MCP tools, a signed webhook, a WebSocket stream, REST polling, or the TypeScript and Python SDKs.",
  },
  {
    question: "How do you verify an email actually came from an AI agent?",
    answer:
      "e2a evaluates SPF, every DKIM signature, and DMARC on every inbound message, then hands the agent a normalized verdict — including whether DMARC passed with alignment — as structured evidence instead of raw headers the agent has to parse and trust. That proves the message really came from the domain it claims, so mail from an agent on a verified e2a domain is distinguishable from a spoof of it. e2a treats an aligned DMARC pass as authorization to use the From domain, not as proof of a particular person or mailbox.",
  },
  {
    question:
      "What is the difference between e2a and a transactional email API like Resend?",
    answer:
      "A transactional email API sends mail on behalf of an application; e2a gives an AI agent a two-way mailbox of its own. e2a accepts inbound mail over SMTP, verifies SPF/DKIM/DMARC, and delivers it to the agent over webhook, WebSocket, REST, or MCP, then sends the agent's replies back out in-thread through its HTTP API — with an optional approval hold and optional prompt-injection screening on the way through. The two are not mutually exclusive: e2a can use an upstream provider such as Amazon SES or Resend as the relay that carries its outbound mail to human recipients.",
  },
  {
    question: "Is there an open-source email API for AI agents?",
    answer:
      "Yes. e2a is Apache-2.0, and the whole stack is public at github.com/tokencanopy/e2a: the Go server and SMTP relay, the TypeScript and Python SDKs, the CLI, the MCP server, and the dashboard. You can self-host it against your own Postgres and outbound SMTP relay. The hosted service at e2a.dev runs the same open-source server image and manages the operating infrastructure for you.",
  },
  {
    question: "How do you stop an AI agent from sending an email without approval?",
    answer:
      "e2a has an opt-in human-in-the-loop approval gate you turn on per agent. With it on, an outbound send is accepted but not dispatched — the API returns 202 with status pending_review — and the message leaves only once a person approves it from the dashboard, the MCP tools, the API, or a magic link mailed to the reviewer's own inbox. A recipient allowlist can narrow the gate so only unfamiliar recipients are held, and a hold that expires without a decision can be configured to reject rather than send.",
  },
  {
    question: "How do you protect an agent inbox from prompt injection over email?",
    answer:
      "e2a can screen inbound mail before the agent ever sees it. Its opt-in content screening flags prompt-injection payloads — hidden HTML, Unicode-tag smuggling, encoded instructions — with dependency-free heuristics, and, with the optional LLM detector enabled, phishing as well; each message is then routed to allow, review, or block. Messages routed to review land in the same queue as held outbound mail, so a person decides before the agent acts on them. e2a also attaches SPF, DKIM, and DMARC results to every inbound message, so an agent can weigh how far to trust a sender instead of trusting the From header.",
  },
  {
    question: "How does an AI agent receive email if it runs locally, with no public URL?",
    answer:
      "e2a delivers inbound mail over a WebSocket stream and plain REST polling as well as webhooks, so an agent running on a laptop, in a notebook, or behind a firewall never needs a public HTTPS endpoint. The e2a CLI's listen command bridges that stream to a local HTTP handler, and the hosted MCP server exposes the same inbox as tools for agents that run no server at all. When you do host a webhook, e2a signs every delivery with a per-webhook HMAC secret that the SDKs verify in one call.",
  },
];

export default function Home() {
  const { user, loading: checkingAuth } = useAuth();
  const [activeTab, setActiveTab] = useState<Tab>("claude");
  const [sdkTab, setSdkTab] = useState<SdkTab>("python");

  return (
    <div
      className="min-h-screen flex flex-col overflow-x-hidden w-full"
      style={{
        background: "var(--bg)",
        color: "var(--fg)",
        fontFamily: "var(--f-ui)",
      }}
    >
      {/* Organization / WebSite / SoftwareApplication come from the root
          layout; the FAQ is page-specific, so it is emitted here rather than
          site-wide. Static export prerenders this client component, so the
          markup lands in the HTML crawlers get without running our JS. */}
      <JsonLd data={faqPage(FAQ)} />

      {/* Nav */}
      <nav
        className="sticky top-0 z-50 backdrop-blur-md backdrop-saturate-150"
        style={{
          background: "color-mix(in srgb, var(--bg) 85%, transparent)",
          borderBottom: "1px solid var(--border)",
        }}
      >
        <div className="max-w-[1080px] mx-auto flex items-center justify-between gap-4 px-8 py-3.5">
          <div className="flex items-center gap-2.5">
            <Link
              href="/"
              className="font-mono font-bold text-[17px]"
              style={{ color: "var(--fg)", letterSpacing: "-0.02em" }}
            >
              e2a
            </Link>
            {/* Umbrella brand, in the nav's brand zone beside the wordmark —
                the same product-then-parent lockup the footer and the app
                rail use. Hidden on phones, where the nav has no room. */}
            <TokenCanopyBadge className="hidden md:inline-flex" />
          </div>

          <div className="flex items-center gap-1 text-[13px]">
            <div className="hidden md:flex items-center gap-1">
              <a
                href="#start"
                className="px-3 py-1.5 rounded-md transition hover:bg-[var(--bg-elev)]"
                style={{ color: "var(--fg-muted)" }}
              >
                Start
              </a>
              <NavMenu label="Product" items={PRODUCT_LINKS} />
              {/* Hosted-only: /pricing is served by the deployment's proxy from
                  outside this app (see PRICING_PATH in lib/site). Unset means
                  no pricing page exists — render nothing rather than a 404. */}
              {PRICING_PATH && (
                <a
                  href={PRICING_PATH}
                  className="px-3 py-1.5 rounded-md transition hover:bg-[var(--bg-elev)]"
                  style={{ color: "var(--fg-muted)" }}
                >
                  Pricing
                </a>
              )}
              <NavMenu label="Resources" items={RESOURCE_LINKS} />
              {SOCIAL_LINKS.map((s) => (
                <a
                  key={s.label}
                  href={s.href}
                  target="_blank"
                  rel="noopener noreferrer"
                  aria-label={s.aria}
                  className="inline-flex items-center px-2 py-1.5 rounded-md transition hover:bg-[var(--bg-elev)]"
                  style={{ color: "var(--fg-muted)" }}
                >
                  <svg width="15" height="15" viewBox="0 0 24 24" fill="currentColor" aria-hidden>
                    <path d={s.path} />
                  </svg>
                </a>
              ))}
              <span
                className="mx-1.5"
                style={{ width: 1, height: 18, background: "var(--border)" }}
              />
              {checkingAuth ? (
                <span className="px-3 text-[12px]" style={{ color: "var(--fg-subtle)" }}>
                  ...
                </span>
              ) : user ? null : (
                /* Signed out, "Sign in" is the secondary path for people who
                   already have an account; the primary CTA beside it starts a
                   new one. Signed IN there is no such pair — both would just
                   be doors into the app — so the primary becomes the only
                   one and points at the dashboard. */
                <a
                  href={SIGN_IN_URL}
                  className="px-3 py-1.5 rounded-md transition hover:bg-[var(--bg-elev)]"
                  style={{ color: "var(--fg-muted)" }}
                >
                  Sign in
                </a>
              )}
            </div>
            <Link
              href={user ? "/inboxes" : "/get-started"}
              className="ml-1 inline-flex items-center gap-1.5 px-3.5 py-1.5 font-medium transition"
              style={{
                background: "var(--fg)",
                color: "var(--bg)",
                borderRadius: "var(--r-md)",
              }}
            >
              {user ? "Go to Dashboard" : "Start building"}
              <span className="font-mono">→</span>
            </Link>
          </div>
        </div>
      </nav>

      {/* Hero */}
      <section className="relative overflow-hidden">
        <div
          aria-hidden
          className="absolute pointer-events-none"
          style={{
            top: -160,
            right: -180,
            width: 560,
            height: 560,
            background:
              "radial-gradient(circle at center, var(--accent-soft) 0%, rgba(0,0,0,0) 60%)",
          }}
        />
        <div
          aria-hidden
          className="absolute pointer-events-none"
          style={{
            bottom: -200,
            left: -200,
            width: 520,
            height: 520,
            background:
              "radial-gradient(circle at center, rgba(111,221,229,.18) 0%, rgba(0,0,0,0) 60%)",
          }}
        />
        <div className="relative max-w-[1080px] mx-auto px-6 md:px-8 pt-16 md:pt-[88px] pb-12 md:pb-16 text-center">
          <p
            className="inline-flex items-center gap-2 mb-6 font-mono text-[11px] font-semibold uppercase"
            style={{ color: "var(--success)", letterSpacing: "0.08em" }}
          >
            <span className="relative inline-flex w-2 h-2">
              <span
                className="absolute inline-flex w-full h-full rounded-full opacity-40 animate-ping"
                style={{ background: "var(--success)" }}
              />
              <span
                className="relative inline-flex w-2 h-2 rounded-full"
                style={{ background: "var(--success)" }}
              />
            </span>
            Open source · Apache 2.0 · free to start
          </p>
          <h1
            className="mx-auto mb-6 leading-[1.05]"
            style={{
              fontFamily: "var(--f-editorial)",
              fontWeight: 400,
              fontSize: "clamp(40px, 6vw, 72px)",
              letterSpacing: "-0.012em",
              maxWidth: 880,
              color: "var(--fg)",
            }}
          >
            The open-source{" "}
            <em
              style={{
                fontStyle: "italic",
                color: "var(--accent-strong)",
              }}
            >
              email API
            </em>
            {" "}for applications and AI agents.
          </h1>
          <p
            className="mx-auto mb-3 leading-[1.55]"
            style={{
              fontSize: 17,
              color: "var(--fg-muted)",
              maxWidth: 660,
            }}
          >
            Send transactional email from any product, give agents real two-way
            inboxes, and keep people in control. Use e2a as a hosted service or
            run the Apache-2.0 stack yourself.
          </p>
          <p
            className="mx-auto mb-7 text-[13px] leading-[1.55]"
            style={{ color: "var(--fg-muted)", maxWidth: 660 }}
          >
            For developers, agent-native teams, and businesses building email
            into their products and workflows.
          </p>
          <div className="flex flex-wrap items-center justify-center gap-2.5">
            <Link
              href="/transactional-email-api"
              className="inline-flex items-center gap-2 px-4 py-2.5 text-[14px] font-medium transition"
              style={{
                background: "var(--accent-fill)",
                color: "var(--accent-fg)",
                borderRadius: "var(--r-md)",
              }}
            >
              Send from your app
              <span className="font-mono" aria-hidden="true">→</span>
            </Link>
            <Link
              href="/get-started"
              className="inline-flex items-center gap-2 px-4 py-2.5 text-[14px] font-medium transition"
              style={{
                background: "var(--bg-panel)",
                color: "var(--fg)",
                border: "1px solid var(--border)",
                borderRadius: "var(--r-md)",
              }}
            >
              Give an agent an inbox
            </Link>
          </div>

          <div
            className="mt-11 flex flex-wrap justify-center gap-x-6 gap-y-2 font-mono text-[12px]"
            style={{ color: "var(--fg-subtle)", letterSpacing: "0.02em" }}
          >
            <span>
              <span style={{ color: "var(--fg-muted)" }}>$</span>&nbsp;claude plugin install e2a@e2a
            </span>
            <span style={{ color: "var(--border-strong)" }}>·</span>
            <span>OAuth, no API key</span>
            <span style={{ color: "var(--border-strong)" }}>·</span>
            <span>Apache 2.0</span>
          </div>

          <div className="mt-7 flex justify-center">
            <a
              href="https://www.producthunt.com/products/e2a-open-source-email-api-for-agents?embed=true&utm_source=badge-featured&utm_medium=badge&utm_campaign=badge-e2a-open-source-email-api-for-agents"
              target="_blank"
              rel="noopener noreferrer"
            >
              {/* eslint-disable-next-line @next/next/no-img-element */}
              <img
                src="https://api.producthunt.com/widgets/embed-image/v1/featured.svg?post_id=1145559&theme=light&t=1778615217650"
                alt="e2a, the open-source email API for AI agents | Product Hunt"
                width={250}
                height={54}
                style={{ display: "block" }}
              />
            </a>
          </div>

          <div className="mt-9 mx-auto max-w-[760px]" aria-label="Popular AI agent use cases">
            <div
              className="font-mono text-[10px] mb-3"
              style={{ color: "var(--fg-subtle)", letterSpacing: "0.1em" }}
            >
              BUILD AN AGENT FOR
            </div>
            <div className="flex flex-wrap justify-center gap-2">
              {[
                ["Support", "/use-cases/support-agent"],
                ["Reception", "/use-cases/ai-receptionist"],
                ["Scheduling", "/use-cases/scheduling-agent"],
                ["E-commerce", "/use-cases/ecommerce-agent"],
              ].map(([label, href]) => (
                <Link
                  key={href}
                  href={href}
                  className="inline-flex items-center gap-1.5 px-3 py-1.5 text-[12px] transition hover:bg-[var(--bg-panel)]"
                  style={{
                    color: "var(--fg-muted)",
                    border: "1px solid var(--border)",
                    borderRadius: "999px",
                  }}
                >
                  {label} <span className="font-mono" aria-hidden="true">→</span>
                </Link>
              ))}
              <Link
                href="/use-cases"
                className="inline-flex items-center px-3 py-1.5 text-[12px] font-medium"
                style={{ color: "var(--accent-strong)" }}
              >
                See all use cases →
              </Link>
            </div>
          </div>
        </div>
      </section>

      {/* Divider */}
      <div style={{ height: 1, background: "var(--border)" }} />

      {/* Start paths */}
      <section id="start" className="px-6 md:px-8 py-12 md:py-16 scroll-mt-20">
        <div className="max-w-[1080px] mx-auto">
          <div className="text-center mb-8">
            <Eyebrow>01 · Choose a path</Eyebrow>
            <h2
              className="mt-3 mb-2"
              style={{
                fontFamily: "var(--f-editorial)",
                fontWeight: 400,
                fontSize: "clamp(28px, 4vw, 38px)",
                letterSpacing: "-0.01em",
                color: "var(--fg)",
              }}
            >
              Start with the email path you need.
            </h2>
            <p className="mx-auto max-w-[580px] text-[14px]" style={{ color: "var(--fg-muted)" }}>
              Use the same open-source API for familiar application email,
              agent-owned conversations, or approval-sensitive workflows.
            </p>
          </div>
          <div className="grid grid-cols-1 md:grid-cols-3 gap-3">
            {[
              {
                title: "Send from your application",
                desc: "Deliver receipts, verification messages, alerts, and product notifications over HTTP or an SDK.",
                href: "/transactional-email-api",
              },
              {
                title: "Give an agent an inbox",
                desc: "Add a real two-way address through a plugin, MCP, webhooks, WebSocket, REST, or an SDK.",
                href: "#quickstart",
              },
              {
                title: "Keep people in control",
                desc: "Hold sensitive outbound messages for human approval before they leave the system.",
                href: "#hitl",
              },
            ].map((path) => (
              <Link
                key={path.title}
                href={path.href}
                className="group p-6 transition hover:bg-[var(--bg-elev)]"
                style={{
                  background: "var(--bg-panel)",
                  border: "1px solid var(--border)",
                  borderRadius: "var(--r-lg)",
                }}
              >
                <h3 className="text-[16px] font-semibold mb-2" style={{ color: "var(--fg)" }}>
                  {path.title} <span className="font-mono group-hover:translate-x-0.5 inline-block transition-transform">→</span>
                </h3>
                <p className="text-[13px] leading-[1.6]" style={{ color: "var(--fg-muted)" }}>
                  {path.desc}
                </p>
              </Link>
            ))}
          </div>
        </div>
      </section>

      {/* Agent quick start */}
      <section
        id="quickstart"
        className="px-6 md:px-8 py-12 md:py-16"
        style={{
          background: "var(--bg-elev)",
          borderTop: "1px solid var(--border)",
          borderBottom: "1px solid var(--border)",
        }}
      >
        <div className="max-w-[1080px] mx-auto">
          <div className="text-center mb-7">
            <Eyebrow>02 · Agent quick start</Eyebrow>
            <h2
              className="mt-3 mb-2"
              style={{
                fontFamily: "var(--f-editorial)",
                fontWeight: 400,
                fontSize: "clamp(28px, 4vw, 38px)",
                letterSpacing: "-0.01em",
                color: "var(--fg)",
              }}
            >
              Install the plugin. Your agent has an inbox.
            </h2>
            <p
              className="mx-auto max-w-[520px] text-[14px]"
              style={{ color: "var(--fg-muted)" }}
            >
              The plugin registers the hosted MCP server and an operate-well
              skill, so your agent can send, receive, reply in-thread, and hold
              mail for review out of the box. First tool use runs an OAuth flow
              in your browser — no API key to paste.
            </p>
          </div>

          <div className="flex justify-center mb-4">
            <div
              className="inline-flex gap-0.5 p-1"
              style={{
                background: "var(--bg-panel)",
                border: "1px solid var(--border)",
                borderRadius: "var(--r-md)",
              }}
            >
              {(["claude", "codex", "cursor", "mcp"] as Tab[]).map((tab) => (
                <button
                  key={tab}
                  type="button"
                  onClick={() => setActiveTab(tab)}
                  className="px-3.5 py-1.5 text-[12px] font-medium transition"
                  style={{
                    background: activeTab === tab ? "var(--fg)" : "transparent",
                    color:
                      activeTab === tab ? "var(--bg)" : "var(--fg-muted)",
                    borderRadius: "var(--r-sm)",
                  }}
                >
                  {tab === "claude"
                    ? "Claude Code"
                    : tab === "codex"
                      ? "Codex"
                      : tab === "cursor"
                        ? "Cursor"
                        : "Any MCP client"}
                </button>
              ))}
            </div>
          </div>

          <CodeBlock>
            {activeTab === "claude" && (
              <>
                <Line c="comment"># add the marketplace, then install the plugin</Line>
                <Line>
                  <Tok c="accent">claude</Tok> plugin marketplace add tokencanopy/e2a
                </Line>
                <Line>
                  <Tok c="accent">claude</Tok> plugin install e2a@e2a
                </Line>
                <Line>&nbsp;</Line>
                <Line c="comment"># then just ask, in plain language:</Line>
                <Line c="comment">#   &quot;Create an email agent and listen for new mail.&quot;</Line>
                <Line c="comment">#   &quot;Reply to Dana&apos;s thread and hold it for my approval.&quot;</Line>
              </>
            )}
            {activeTab === "codex" && (
              <>
                <Line c="comment"># add the marketplace</Line>
                <Line>
                  <Tok c="accent">codex</Tok> plugin marketplace add tokencanopy/e2a
                </Line>
                <Line>&nbsp;</Line>
                <Line c="comment"># then launch codex, run /plugins, and install e2a</Line>
                <Line>
                  <Tok c="accent">codex</Tok>
                </Line>
                <Line>
                  <Tok c="flag">/plugins</Tok>
                </Line>
              </>
            )}
            {activeTab === "cursor" && (
              <>
                <Line c="comment"># .cursor/mcp.json — or ~/.cursor/mcp.json for every project</Line>
                <Line>{`{`}</Line>
                <Line>
                  &nbsp;&nbsp;<Tok c="string">&quot;mcpServers&quot;</Tok>: {`{`}
                </Line>
                <Line>
                  &nbsp;&nbsp;&nbsp;&nbsp;<Tok c="string">&quot;e2a&quot;</Tok>: {`{`}
                </Line>
                <Line>
                  &nbsp;&nbsp;&nbsp;&nbsp;&nbsp;&nbsp;<Tok c="string">&quot;url&quot;</Tok>:{" "}
                  <Tok c="accent">&quot;https://api.e2a.dev/mcp&quot;</Tok>
                </Line>
                <Line>&nbsp;&nbsp;&nbsp;&nbsp;{`}`}</Line>
                <Line>&nbsp;&nbsp;{`}`}</Line>
                <Line>{`}`}</Line>
                <Line>&nbsp;</Line>
                <Line c="comment"># Cursor opens your browser to authorize on first use —</Line>
                <Line c="comment"># no API key to paste</Line>
              </>
            )}
            {activeTab === "mcp" && (
              <>
                <Line c="comment"># Zed, Goose, Windsurf, Claude Desktop, raw mcp.json —</Line>
                <Line c="comment"># point straight at the hosted server</Line>
                <Line>
                  <Tok c="accent">https://api.e2a.dev/mcp</Tok>
                </Line>
                <Line>&nbsp;</Line>
                <Line c="comment"># your agent gets the inbox toolset:</Line>
                <Line c="comment">#   list_messages · send_message · reply_to_message · …</Line>
                <Line>&nbsp;</Line>
                <Line c="comment"># hosts with OAuth connectors: add it and authorize in the</Line>
                <Line c="comment"># browser — no key pasted</Line>
              </>
            )}
          </CodeBlock>

        </div>
      </section>

      {/* Application API — the conventional integration path. Agent inboxes
          remain available, but application-triggered sending does not require
          an agent framework. */}
      <section id="build" className="px-6 md:px-8 py-14 md:py-[72px]">
        <div className="max-w-[1080px] mx-auto grid grid-cols-1 md:grid-cols-2 gap-10 md:gap-14 items-center">
          <div>
            <Eyebrow>03 · Application API</Eyebrow>
            <h2
              className="mt-3.5 mb-4 leading-[1.1]"
              style={{
                fontFamily: "var(--f-editorial)",
                fontWeight: 400,
                fontSize: "clamp(30px, 4.5vw, 44px)",
                letterSpacing: "-0.01em",
                color: "var(--fg)",
              }}
            >
              Send from any{" "}
              <em style={{ color: "var(--accent-strong)" }}>application.</em>
            </h2>
            <p
              className="mb-3.5 leading-[1.6]"
              style={{ fontSize: 15, color: "var(--fg-muted)" }}
            >
              Use e2a for receipts, verification messages, product
              notifications, and alerts from the software you already run. No
              agent framework is required.
            </p>
            <p
              className="mb-5 leading-[1.65]"
              style={{ fontSize: 13, color: "var(--fg-muted)" }}
            >
              Call the HTTP API directly or use the TypeScript and Python SDKs.
              If the workflow later needs replies, e2a can turn the same sender
              into a two-way inbox with threading, signed webhooks, WebSocket
              delivery, and optional human approval.
            </p>
            <div className="flex flex-wrap items-center gap-2.5">
              <Link
                href="/transactional-email-api"
                className="inline-flex items-center gap-2 px-4 py-2.5 text-[14px] font-medium"
                style={{
                  background: "var(--accent-fill)",
                  color: "var(--accent-fg)",
                  borderRadius: "var(--r-md)",
                }}
              >
                Transactional email guide
                <span className="font-mono">→</span>
              </Link>
              <Link
                href="/api-docs"
                className="inline-flex items-center px-4 py-2.5 text-[14px] font-medium"
                style={{
                  background: "var(--bg-panel)",
                  color: "var(--fg)",
                  border: "1px solid var(--border-strong)",
                  borderRadius: "var(--r-md)",
                }}
              >
                API reference
              </Link>
            </div>
          </div>

          <div>
            {/* Both SDKs, same ordinary application-triggered send. */}
            <div className="flex mb-3">
              <div
                className="inline-flex gap-0.5 p-1"
                style={{
                  background: "var(--bg-panel)",
                  border: "1px solid var(--border)",
                  borderRadius: "var(--r-md)",
                }}
              >
                {(["python", "ts"] as SdkTab[]).map((tab) => (
                  <button
                    key={tab}
                    type="button"
                    onClick={() => setSdkTab(tab)}
                    className="px-3.5 py-1.5 text-[12px] font-medium transition"
                    style={{
                      background: sdkTab === tab ? "var(--fg)" : "transparent",
                      color: sdkTab === tab ? "var(--bg)" : "var(--fg-muted)",
                      borderRadius: "var(--r-sm)",
                    }}
                  >
                    {tab === "python" ? "Python" : "TypeScript"}
                  </button>
                ))}
              </div>
            </div>

            <CodeBlock>
            {sdkTab === "python" && (
              <>
            <Line c="comment"># pip install e2a</Line>
            <Line>&nbsp;</Line>
            <Line>
              <Tok c="keyword">from</Tok> e2a.v1 <Tok c="keyword">import</Tok> AsyncE2AClient
            </Line>
            <Line>&nbsp;</Line>
            <Line>
              <Tok c="keyword">async with</Tok> <Tok c="fn">AsyncE2AClient</Tok>(api_key=<Tok c="string">&quot;e2a_…&quot;</Tok>) <Tok c="keyword">as</Tok> client:
            </Line>
            <Line>
              &nbsp;&nbsp;<Tok c="keyword">await</Tok> client.messages.<Tok c="fn">send</Tok>(<Tok c="string">&quot;receipts@example.com&quot;</Tok>, {`{`}
            </Line>
            <Line>
              &nbsp;&nbsp;&nbsp;&nbsp;<Tok c="string">&quot;to&quot;</Tok>: [{`{`}<Tok c="string">&quot;email&quot;</Tok>: <Tok c="string">&quot;customer@example.net&quot;</Tok>{`}`}],
            </Line>
            <Line>
              &nbsp;&nbsp;&nbsp;&nbsp;<Tok c="string">&quot;subject&quot;</Tok>: <Tok c="string">&quot;Your receipt&quot;</Tok>,
            </Line>
            <Line>
              &nbsp;&nbsp;&nbsp;&nbsp;<Tok c="string">&quot;text&quot;</Tok>: <Tok c="string">&quot;Thanks for your order.&quot;</Tok>,
            </Line>
            <Line>
              &nbsp;&nbsp;{`}`})
            </Line>
              </>
            )}
            {sdkTab === "ts" && (
              <>
            <Line c="comment">{"// npm i @e2a/sdk"}</Line>
            <Line>&nbsp;</Line>
            <Line>
              <Tok c="keyword">import</Tok> {`{`} E2AClient {`}`} <Tok c="keyword">from</Tok> <Tok c="string">&quot;@e2a/sdk&quot;</Tok>;
            </Line>
            <Line>&nbsp;</Line>
            <Line>
              <Tok c="keyword">const</Tok> client = <Tok c="keyword">new</Tok> <Tok c="fn">E2AClient</Tok>({`{`} apiKey: <Tok c="string">&quot;e2a_…&quot;</Tok> {`}`});
            </Line>
            <Line>&nbsp;</Line>
            <Line>
              <Tok c="keyword">await</Tok> client.messages.<Tok c="fn">send</Tok>(<Tok c="string">&quot;receipts@example.com&quot;</Tok>, {`{`}
            </Line>
            <Line>
              &nbsp;&nbsp;to: [{`{`} email: <Tok c="string">&quot;customer@example.net&quot;</Tok> {`}`}],
            </Line>
            <Line>
              &nbsp;&nbsp;subject: <Tok c="string">&quot;Your receipt&quot;</Tok>,
            </Line>
            <Line>
              &nbsp;&nbsp;text: <Tok c="string">&quot;Thanks for your order.&quot;</Tok>,
            </Line>
            <Line>{`}`});</Line>
              </>
            )}
            </CodeBlock>
          </div>
        </div>
      </section>

      {/* Human-in-the-loop */}
      <section
        id="hitl"
        className="px-6 md:px-8 py-14 md:py-[72px]"
        style={{
          background: "var(--bg-elev)",
          borderTop: "1px solid var(--border)",
          borderBottom: "1px solid var(--border)",
        }}
      >
        <div className="max-w-[1080px] mx-auto grid grid-cols-1 md:grid-cols-2 gap-10 md:gap-14 items-center">
          <div>
            <Eyebrow>03 · Human-in-the-loop</Eyebrow>
            <h2
              className="mt-3.5 mb-4 leading-[1.1]"
              style={{
                fontFamily: "var(--f-editorial)",
                fontWeight: 400,
                fontSize: "clamp(30px, 4.5vw, 44px)",
                letterSpacing: "-0.01em",
                color: "var(--fg)",
              }}
            >
              Approve{" "}
              <em style={{ color: "var(--accent-strong)" }}>before</em> your agent hits send.
            </h2>
            <p
              className="mb-3.5 leading-[1.6]"
              style={{ fontSize: 15, color: "var(--fg-muted)" }}
            >
              Flip one switch and outbound messages pause for your review instead of going straight out. You get a notification — click to see recipients, subject, and body on a secure confirmation page. Approve, edit, or reject.
            </p>
            <p
              className="mb-5 leading-[1.65]"
              style={{ fontSize: 13, color: "var(--fg-muted)" }}
            >
              Per-agent, opt-in, off by default. Configurable TTL with auto-approve or auto-reject on expiry. Reviewable from the dashboard, SDK, or one-click magic links in your inbox.
            </p>
            <div className="flex flex-wrap items-center gap-2.5">
              <Link
                href="/blog/human-in-the-loop-for-agent-email"
                className="inline-flex items-center gap-2 px-4 py-2.5 text-[14px] font-medium"
                style={{
                  background: "var(--accent-fill)",
                  color: "var(--accent-fg)",
                  borderRadius: "var(--r-md)",
                }}
              >
                Read the announcement
                <span className="font-mono">→</span>
              </Link>
              <Link
                href="/inboxes"
                className="inline-flex items-center px-4 py-2.5 text-[14px] font-medium"
                style={{
                  background: "var(--bg-panel)",
                  color: "var(--fg)",
                  border: "1px solid var(--border)",
                  borderRadius: "var(--r-md)",
                }}
              >
                Enable on an agent
              </Link>
            </div>
          </div>

          <CodeBlock title="hitl.ts · sdk">
            <Line c="comment">{"// Turn on HITL in the dashboard, or hold outbound for"}</Line>
            <Line c="comment">{"// review via the agent's protection config."}</Line>
            <Line>&nbsp;</Line>
            <Line c="comment">{"// review held messages with an account-scoped key"}</Line>
            <Line>
              <Tok c="keyword">const</Tok> held = <Tok c="keyword">await</Tok> client.reviews.<Tok c="fn">list</Tok>().<Tok c="fn">toArray</Tok>({`{`} limit: <Tok c="accent">50</Tok> {`}`});
            </Line>
            <Line c="dim">&nbsp;&nbsp;msg_abc123  customer@acme.io   <Tok c="warn">in 47m</Tok></Line>
            <Line c="dim">&nbsp;&nbsp;msg_def456  legal@stripe.com   <Tok c="warn">in 2h 12m</Tok></Line>
            <Line>&nbsp;</Line>
            <Line c="comment">{"// approve (sends it) or reject with a reason"}</Line>
            <Line>
              <Tok c="keyword">await</Tok> client.reviews.<Tok c="fn">approve</Tok>(<Tok c="string">&quot;msg_abc123&quot;</Tok>);
            </Line>
            <Line c="success">&nbsp;&nbsp;→ approved · delivering now</Line>
          </CodeBlock>
        </div>
      </section>

      {/* Use cases */}
      <section
        id="use-cases"
        className="px-6 md:px-8 py-12 md:py-16"
      >
        <div className="max-w-[1080px] mx-auto">
          <div className="text-center mb-10">
            <Eyebrow>04 · Use cases</Eyebrow>
            <h2
              className="mt-3 mb-2"
              style={{
                fontFamily: "var(--f-editorial)",
                fontWeight: 400,
                fontSize: "clamp(28px, 4vw, 38px)",
                letterSpacing: "-0.01em",
                color: "var(--fg)",
              }}
            >
              What you can build.
            </h2>
            <p
              className="mx-auto max-w-[460px] text-[14px]"
              style={{ color: "var(--fg-muted)" }}
            >
              If it can receive email and take action, e2a can power it.
            </p>
          </div>

          <div
            className="grid grid-cols-1 sm:grid-cols-2 md:grid-cols-3 overflow-hidden"
            style={{
              background: "var(--bg-panel)",
              border: "1px solid var(--border)",
              borderRadius: "var(--r-lg)",
            }}
          >
            {USE_CASES.map((u, i) => {
              const col3 = i % 3;
              const lastRow = i >= USE_CASES.length - 3;
              return u.href ? (
                  <Link
                    href={u.href}
                    key={u.title}
                    className="block p-6 md:p-7 transition hover:bg-[var(--bg-elev)]"
                    style={{
                      borderRight:
                        col3 < 2 ? "1px solid var(--border)" : "none",
                      borderBottom: lastRow
                        ? "none"
                        : "1px solid var(--border)",
                    }}
                  >
                    <UseCaseCardContent useCase={u} />
                  </Link>
                ) : (
                  <div
                  key={u.title}
                  className="p-6 md:p-7"
                  style={{
                    borderRight:
                      col3 < 2 ? "1px solid var(--border)" : "none",
                    borderBottom: lastRow
                      ? "none"
                      : "1px solid var(--border)",
                  }}
                >
                    <UseCaseCardContent useCase={u} />
                  </div>
                );
            })}
          </div>
        </div>
      </section>

      {/* FAQ — continues the alternation: 04 Use cases is light, so this one
          takes the elevated background. Question-shaped H2/H3 with the answer
          immediately below is the shape both featured snippets and LLM
          extraction key off, and it mirrors the same content emitted as
          FAQPage structured data at the top of this page. */}
      <section
        id="faq"
        className="px-6 md:px-8 py-12 md:py-16 scroll-mt-20"
        style={{
          background: "var(--bg-elev)",
          borderTop: "1px solid var(--border)",
        }}
      >
        <div className="max-w-[760px] mx-auto">
          <div className="mb-8">
            <Eyebrow>05 · FAQ</Eyebrow>
            <h2
              className="mt-3"
              style={{
                fontFamily: "var(--f-editorial)",
                fontWeight: 400,
                fontSize: "clamp(28px, 4vw, 38px)",
                letterSpacing: "-0.01em",
                color: "var(--fg)",
              }}
            >
              Questions people ask.
            </h2>
          </div>
          <div>
            {FAQ.map((f) => (
              <div
                key={f.question}
                className="py-5"
                style={{ borderTop: "1px solid var(--border)" }}
              >
                <h3
                  className="text-[15px] font-semibold mb-2"
                  style={{ color: "var(--fg)" }}
                >
                  {f.question}
                </h3>
                <p
                  className="text-[14px] leading-[1.65]"
                  style={{ color: "var(--fg-muted)" }}
                >
                  {f.answer}
                </p>
              </div>
            ))}
          </div>
        </div>
      </section>

      {/* CTA — continues the alternation: 05 FAQ is elevated, so the closing
          section drops back to the page background. */}
      <section
        className="px-6 md:px-8 py-14 md:py-[72px] text-center"
        style={{
          background: "var(--bg)",
          borderTop: "1px solid var(--border)",
        }}
      >
        <div className="max-w-[720px] mx-auto">
          <h2
            className="mb-3.5 leading-[1.05]"
            style={{
              fontFamily: "var(--f-editorial)",
              fontWeight: 400,
              fontSize: "clamp(32px, 4.5vw, 46px)",
              letterSpacing: "-0.012em",
              color: "var(--fg)",
            }}
          >
            Your agent&apos;s inbox is{" "}
            <em style={{ color: "var(--accent-strong)" }}>one sign-in</em> away.
          </h2>
          <p
            className="mb-7 leading-[1.55]"
            style={{ fontSize: 15, color: "var(--fg-muted)" }}
          >
            Free to start. No credit card. Up and running in under two minutes.
          </p>
          <div className="flex flex-wrap items-center justify-center gap-2.5">
            <Link
              href="/get-started"
              className="inline-flex items-center gap-2 px-4 py-2.5 text-[14px] font-medium"
              style={{
                background: "var(--accent-fill)",
                color: "var(--accent-fg)",
                borderRadius: "var(--r-md)",
              }}
            >
              Get started free
              <span className="font-mono">→</span>
            </Link>
            <Link
              href="/api-docs"
              className="inline-flex items-center px-4 py-2.5 text-[14px] font-medium"
              style={{
                background: "var(--bg-panel)",
                color: "var(--fg)",
                border: "1px solid var(--border)",
                borderRadius: "var(--r-md)",
              }}
            >
              Read the docs
            </Link>
          </div>
          <p
            className="mt-4 text-[12px]"
            style={{ color: "var(--fg-subtle)" }}
          >
            Have feedback?{" "}
            <Link
              href="/feedback"
              style={{ color: "var(--accent-strong)" }}
            >
              We&apos;d love to hear from you.
            </Link>
          </p>
        </div>
      </section>

      {/* Footer */}
      <footer
        className="px-6 md:px-8 py-6"
        style={{ borderTop: "1px solid var(--border)" }}
      >
        <div className="max-w-[1080px] mx-auto flex flex-col md:flex-row md:items-center md:justify-between gap-4">
          {/* Product and who makes it. The tagline that used to sit between
              them is gone — the hero above already says what e2a is, so in
              the footer it was just noise between the two names. */}
          <div className="flex items-center gap-3 flex-wrap">
            <span
              className="font-mono font-bold text-[15px]"
              style={{ color: "var(--fg)", letterSpacing: "-0.02em" }}
            >
              e2a
            </span>
            <TokenCanopyBadge />
          </div>
          <div className="flex flex-wrap gap-x-4 gap-y-1.5 text-[12px]">
            {FOOTER_LINKS.map((l) =>
              l.external ? (
                <a
                  key={l.label}
                  href={l.href}
                  target="_blank"
                  rel="noopener noreferrer"
                  style={{ color: "var(--fg-muted)" }}
                >
                  {l.label}
                </a>
              ) : (
                <Link
                  key={l.label}
                  href={l.href}
                  style={{ color: "var(--fg-muted)" }}
                >
                  {l.label}
                </Link>
              ),
            )}
          </div>
          <span
            className="font-mono text-[12px]"
            style={{ color: "var(--fg-subtle)" }}
          >
            Apache 2.0
          </span>
        </div>
      </footer>
    </div>
  );
}

// ─────────────────────────────────────────────────────────────────────────
// Local helpers — small enough to live next to the page rather than become
// reusable primitives. The shared InkConsole primitive takes a `lines`
// array; here we want JSX children so syntax highlighting reads inline.
// ─────────────────────────────────────────────────────────────────────────

// A nav dropdown. Each menu owns its open state so adding one doesn't mean
// threading another boolean through the page component.
//
// Opens on hover, click, AND focus — hover alone left everything inside
// unreachable by keyboard, and unreliable on touch devices that get the
// desktop layout. Focus/blur are on the wrapper (React bubbles them), so
// tabbing into the button opens the menu and tabbing past the last link
// closes it; Escape dismisses without a pointer.
function NavMenu({ label, items }: { label: string; items: NavItem[] }) {
  const [open, setOpen] = useState(false);
  return (
    <div
      className="relative"
      onMouseEnter={() => setOpen(true)}
      onMouseLeave={() => setOpen(false)}
      onFocus={() => setOpen(true)}
      onBlur={(e) => {
        // Only close when focus actually leaves the menu, not when it moves
        // between the button and its own links.
        if (!e.currentTarget.contains(e.relatedTarget as Node | null)) {
          setOpen(false);
        }
      }}
      onKeyDown={(e) => {
        if (e.key === "Escape") setOpen(false);
      }}
    >
      <button
        type="button"
        aria-expanded={open}
        aria-haspopup="true"
        onClick={() => setOpen((o) => !o)}
        className="px-3 py-1.5 rounded-md transition hover:bg-[var(--bg-elev)]"
        style={{ color: "var(--fg-muted)" }}
      >
        {label} <span className="text-[10px]">▾</span>
      </button>
      {open && (
        <div
          className="absolute top-full left-0 min-w-[190px] py-1.5 z-50"
          style={{
            background: "var(--bg-panel)",
            border: "1px solid var(--border)",
            borderRadius: "var(--r-md)",
            boxShadow: "var(--sh-2)",
          }}
        >
          {items.map((d) =>
            d.external ? (
              <a
                key={d.label}
                href={d.href}
                target="_blank"
                rel="noopener noreferrer"
                className="block px-4 py-1.5 text-[13px] transition hover:bg-[var(--bg-elev)]"
                style={{ color: "var(--fg-muted)" }}
              >
                {d.label}
              </a>
            ) : d.href.startsWith("#") ? (
              <a
                key={d.label}
                href={d.href}
                className="block px-4 py-1.5 text-[13px] transition hover:bg-[var(--bg-elev)]"
                style={{ color: "var(--fg-muted)" }}
              >
                {d.label}
              </a>
            ) : (
              <Link
                key={d.label}
                href={d.href}
                target={d.newTab ? "_blank" : undefined}
                rel={d.newTab ? "noopener noreferrer" : undefined}
                className="block px-4 py-1.5 text-[13px] transition hover:bg-[var(--bg-elev)]"
                style={{ color: "var(--fg-muted)" }}
              >
                {d.label}
              </Link>
            ),
          )}
        </div>
      )}
    </div>
  );
}

function CodeBlock({
  children,
  title,
}: {
  children: React.ReactNode;
  title?: string;
}) {
  return (
    <div
      className="overflow-hidden font-mono"
      style={{
        background: "var(--ink)",
        border: "1px solid var(--ink-border)",
        borderRadius: "var(--r-lg)",
        boxShadow:
          "0 1px 0 rgba(255,255,255,.03) inset, 0 6px 24px rgba(20,15,8,.08)",
      }}
    >
      <div
        className="flex items-center gap-2.5 px-4 py-2.5"
        style={{
          borderBottom: "1px solid var(--ink-border)",
          background: "var(--ink-elev)",
        }}
      >
        <span className="w-2.5 h-2.5 rounded-full" style={{ background: "#3a342e" }} />
        <span className="w-2.5 h-2.5 rounded-full" style={{ background: "#3a342e" }} />
        <span className="w-2.5 h-2.5 rounded-full" style={{ background: "#3a342e" }} />
        <span
          className="ml-2 text-[11px]"
          style={{ color: "var(--ink-fg-muted)", letterSpacing: "0.04em" }}
        >
          {title ?? "~/my-agent"}
        </span>
      </div>
      <div
        className="px-5 py-4 text-[12.5px] leading-[1.75] overflow-x-auto"
        style={{ color: "var(--ink-fg)" }}
      >
        {children}
      </div>
    </div>
  );
}

type LineKind = "plain" | "comment" | "dim" | "success";

function Line({
  children,
  c = "plain",
}: {
  children: React.ReactNode;
  c?: LineKind;
}) {
  const color =
    c === "comment"
      ? "var(--ink-fg-muted)"
      : c === "dim"
        ? "var(--ink-fg-muted)"
        : c === "success"
          ? "var(--machine)"
          : "var(--ink-fg)";
  return <div style={{ color }}>{children}</div>;
}

function Tok({
  children,
  c,
}: {
  children: React.ReactNode;
  c: "prompt" | "string" | "keyword" | "fn" | "flag" | "accent" | "warn";
}) {
  const color =
    c === "prompt"
      ? "var(--machine)"
      : c === "string"
        ? "var(--spectral)"
        : c === "keyword"
          ? "#C8A8FF"
          : c === "fn"
            ? "var(--spectral)"
            : c === "flag"
              ? "var(--machine)"
              : c === "accent"
                ? "var(--accent)"
                : "var(--warn)";
  const extra =
    c === "prompt" ? { userSelect: "none" as const, marginRight: 8 } : {};
  return <span style={{ color, ...extra }}>{children}</span>;
}
