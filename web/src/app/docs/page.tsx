import Link from "next/link";
import { JsonLd } from "../components/JsonLd";
import { breadcrumbs, faqPage, type FaqEntry } from "../../lib/jsonld";

// Deliberately a server component with no "use client". This route used to be
// a JavaScript redirect to /scalar.html, which meant the one thing it shipped
// to a crawler was the string "Loading API docs..." — a sitemap entry at
// priority 0.8 that could answer nothing. The full reference still lives at
// /api-docs (and /scalar.html); this page is the index that links there and
// answers the implementation questions people ask on the way in.

// Implementation-shaped questions, grounded in setup.md / auth.md / sdk.md.
// They deliberately do not repeat /mcp's questions, which cover connecting an
// MCP client specifically; these cover choosing between the client surfaces
// and the mechanics you hit once you have chosen.
//
// Each answer must survive being quoted with none of this page attached, so it
// names e2a instead of saying "we" or "it", and claims only shipped behavior.
const FAQ: FaqEntry[] = [
  {
    question: "How do I connect an AI agent to e2a — over MCP, REST, or an SDK?",
    answer:
      "e2a offers all three over one API, so the choice is about how your agent runs rather than what it can do. Point any Streamable HTTP MCP client at https://api.e2a.dev/mcp and the agent gets the inbox as tools with no API key to paste — the fastest path for coding agents and agent frameworks. If you are writing the integration yourself, install @e2a/sdk for TypeScript or e2a for Python to get typed clients with one-call webhook verification and a WebSocket listen() stream; if your language has no SDK, call the REST API at https://api.e2a.dev/v1 directly against the published OpenAPI 3.1 contract.",
  },
  {
    question: "How does authentication work for the e2a API?",
    answer:
      "Every authenticated e2a request carries an Authorization: Bearer credential, and e2a accepts two kinds, told apart by prefix. Interactive MCP clients use OAuth 2.1 access tokens (ate2a_…), obtained by self-registering through RFC 7591 Dynamic Client Registration with PKCE, so no human ever pastes a secret. API keys are issued from the e2a dashboard or CLI in two scopes — an account key (e2a_acct_…) manages agents, domains, and keys, while an agent key (e2a_agt_…) is pinned to a single inbox — and are what you use for servers, scripts, and CI where no browser is available.",
  },
  {
    question: "Should my agent receive email over a webhook or a WebSocket?",
    answer:
      "Use a webhook when the agent runs as a service that can host a public HTTPS endpoint: e2a POSTs a signed metadata trigger, and the SDK's constructEvent or construct_event helper verifies the per-webhook HMAC before you hydrate the full message. Use the WebSocket stream — client.listen(agentEmail) in either SDK, or e2a listen in the CLI — when the agent runs on a laptop or behind a firewall and cannot host a URL at all. Both carry the same email.received event, and REST polling and the MCP tools read the same mailbox, so different deployments of the same agent can use different channels.",
  },
  {
    question: "How does e2a thread an email conversation?",
    answer:
      "Reply with e2a's reply operation and the original message_id — client.messages.reply(...) in the SDKs, reply_to_message over MCP — and e2a sets the standards-compliant reply headers that keep the exchange in one thread in Gmail and Outlook. conversation_id is a separate, caller-owned correlation field: bind it to your agent framework's session ID so the agent has memory across replies, but on its own it does not set those headers and it is never an authorization decision. Starting a fresh send instead of a reply opens a new thread in the recipient's mail client.",
  },
  {
    question: "How do I keep a retried API call from sending the same email twice?",
    answer:
      "e2a's send, reply, and forward operations accept an Idempotency-Key header — use one UUIDv4 per logical message. Reuse the same key on transport retries such as timeouts and dropped connections and e2a replays the original response instead of sending again; reusing a key with a different body returns 422, and a genuinely new message needs a fresh key. Separately, a send that returns 202 with status pending_review has already been accepted and is being held for approval, so report the status and stop rather than retrying, which would queue a duplicate.",
  },
  {
    question: "Is the e2a /v1 API stable enough to build on?",
    answer:
      "e2a's core /v1 REST API and its TypeScript and Python SDKs are generally available: v1.5.0 is the compatibility baseline, there are no breaking changes within /v1, and every later release is audited against that tag. A small, explicitly enumerated set of newer resources is still beta and may change — those are marked x-stability-level: beta in the OpenAPI document and (beta) in the docs, so GA and beta can be told apart mechanically rather than by reading prose. The contract at https://e2a.dev/v1/openapi.yaml is generated from the live handlers and drift-checked in CI, so it cannot fall out of step with the running service.",
  },
];

// The documents an agent (or a person) should read next. Machine-readable
// first: an LLM landing here should find llms.txt without scrolling.
const REFERENCES: { label: string; href: string; desc: string }[] = [
  {
    label: "API reference",
    href: "/api-docs",
    desc: "Every /v1 endpoint, request and response body, enum, and error code, rendered from the live OpenAPI contract.",
  },
  {
    label: "openapi.yaml",
    href: "https://e2a.dev/v1/openapi.yaml",
    desc: "The OpenAPI 3.1 document itself — generated from the handlers and authoritative for exact signatures.",
  },
  {
    label: "setup.md",
    href: "https://e2a.dev/setup.md",
    desc: "Connect a client, pick or create an inbox, and confirm it is ready to send and receive.",
  },
  {
    label: "auth.md",
    href: "https://e2a.dev/auth.md",
    desc: "The credential model in full: OAuth 2.1 with Dynamic Client Registration, API key scopes, errors, and revocation.",
  },
  {
    label: "sdk.md",
    href: "https://e2a.dev/sdk.md",
    desc: "TypeScript and Python quick starts — send, receive over webhook or WebSocket, reply in-thread — plus raw REST.",
  },
  {
    label: "templates.md",
    href: "https://e2a.dev/templates.md",
    desc: "Email templates (beta): reusable server-rendered bodies with {{variable}} interpolation and a starter catalog.",
  },
  {
    label: "llms.txt",
    href: "https://e2a.dev/llms.txt",
    desc: "Machine-readable index of everything above. llms-full.txt inlines the whole corpus in one fetch.",
  },
];

export default function DocsPage() {
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
          faqPage(FAQ),
          breadcrumbs([
            { name: "e2a", path: "/" },
            { name: "Docs", path: "/docs" },
          ]),
        ]}
      />

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
            <Link href="/mcp" className="text-[13px]" style={{ color: "var(--fg-muted)" }}>
              MCP
            </Link>
            <Link href="/blog" className="text-[13px]" style={{ color: "var(--fg-muted)" }}>
              Blog
            </Link>
            <Link
              href="/api-docs"
              className="inline-flex items-center gap-1.5 px-3.5 py-1.5 text-[13px] font-medium"
              style={{
                background: "var(--fg)",
                color: "var(--bg)",
                borderRadius: "var(--r-md)",
              }}
            >
              API reference <span className="font-mono">→</span>
            </Link>
          </div>
        </div>
      </nav>

      <main className="max-w-[720px] mx-auto px-6 md:px-8 py-14 md:py-[56px] pb-20">
        <p
          className="font-mono text-[11px] mb-3"
          style={{ color: "var(--fg-subtle)", letterSpacing: "0.08em" }}
        >
          DOCS
        </p>
        <h1
          className="text-[34px] md:text-[40px] font-semibold leading-[1.12] mb-5"
          style={{ letterSpacing: "-0.025em" }}
        >
          e2a developer docs
        </h1>

        {/* The lede is the extractable answer: what the docs cover and where
            the exhaustive reference lives. */}
        <p className="text-[17px] leading-[1.6]" style={{ color: "var(--fg-muted)" }}>
          e2a is an authenticated email gateway for AI agents: it gives an agent
          its own verified email address, evaluates SPF, DKIM, and DMARC on every
          inbound message, and delivers that mail over MCP, a signed webhook, a
          WebSocket stream, or REST. These pages cover how to connect, how to
          authenticate, and how mail is threaded; the exhaustive endpoint
          reference is the{" "}
          <Link href="/api-docs" style={{ color: "var(--fg)", textDecoration: "underline" }}>
            API reference
          </Link>
          .
        </p>

        <Section id="reference" title="Where the reference lives">
          <p className="text-[15px] leading-[1.65]" style={{ color: "var(--fg-muted)" }}>
            Each document below is a plain, fetchable file — the Markdown ones
            are written to be read by an agent as easily as by a person.
          </p>
          <ul className="mt-4 space-y-3.5">
            {REFERENCES.map((r) => (
              <li key={r.href} className="text-[14.5px] leading-[1.6]">
                <A href={r.href}>{r.label}</A>{" "}
                <span style={{ color: "var(--fg-muted)" }}>— {r.desc}</span>
              </li>
            ))}
          </ul>
        </Section>

        <Section id="faq" title="Frequently asked questions">
          <div className="mt-1">
            {FAQ.map((f) => (
              <div
                key={f.question}
                className="py-4"
                style={{ borderTop: "1px solid var(--border)" }}
              >
                <h3 className="text-[15px] font-semibold mb-1.5">{f.question}</h3>
                <p
                  className="text-[14.5px] leading-[1.6]"
                  style={{ color: "var(--fg-muted)" }}
                >
                  {f.answer}
                </p>
              </div>
            ))}
          </div>
        </Section>

        <div
          className="mt-12 p-6"
          style={{
            border: "1px solid var(--border)",
            borderRadius: "var(--r-lg)",
            background: "var(--bg-elev)",
          }}
        >
          <h2 className="text-[17px] font-semibold mb-1.5">Give your agent an inbox</h2>
          <p className="text-[14.5px] mb-4" style={{ color: "var(--fg-muted)" }}>
            Free tier, no card. Create an inbox on the shared domain and send
            your first message without any DNS setup.
          </p>
          <Link
            href="/get-started"
            className="inline-flex items-center gap-1.5 px-4 py-2 text-[14px] font-medium"
            style={{
              background: "var(--fg)",
              color: "var(--bg)",
              borderRadius: "var(--r-md)",
            }}
          >
            Start building <span className="font-mono">→</span>
          </Link>
        </div>
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
        <span className="font-mono text-[12px]" style={{ color: "var(--fg-subtle)" }}>
          Apache 2.0
        </span>
      </footer>
    </div>
  );
}

function Section({
  id,
  title,
  children,
}: {
  id: string;
  title: string;
  children: React.ReactNode;
}) {
  return (
    <section id={id} className="mt-12 scroll-mt-20">
      <h2
        className="text-[22px] font-semibold mb-3.5 leading-[1.25]"
        style={{ letterSpacing: "-0.015em" }}
      >
        {title}
      </h2>
      {children}
    </section>
  );
}

function A({ href, children }: { href: string; children: React.ReactNode }) {
  const external = href.startsWith("http");
  return (
    <a
      href={href}
      style={{ color: "var(--fg)", textDecoration: "underline" }}
      {...(external ? { target: "_blank", rel: "noopener noreferrer" } : {})}
    >
      {children}
    </a>
  );
}
