import type { Metadata } from "next";
import Link from "next/link";
import { SITE_URL } from "../../lib/site";
import { JsonLd } from "../components/JsonLd";
import { breadcrumbs, faqPage, howTo, type FaqEntry } from "../../lib/jsonld";

// Deliberately a server component with no "use client". Everything on this
// page must exist in the static HTML: answer engines and most crawlers never
// run our JavaScript, and this route's whole job is to be quotable.

const TITLE = "Email over MCP — give your AI agent a real inbox";
const DESC =
  "Connect Claude Code, Codex, Cursor, or any MCP client to a real, authenticated email address in one command. OAuth 2.1, no API key, SPF/DKIM-verified inbound.";

const MCP_URL = "https://api.e2a.dev/mcp";

export const metadata: Metadata = {
  title: { absolute: `${TITLE} — e2a` },
  description: DESC,
  alternates: { canonical: "/mcp" },
  keywords: [
    "email mcp server",
    "mcp server for email",
    "claude code email mcp",
    "codex email mcp",
    "cursor email mcp",
    "give ai agent email address mcp",
    "agent inbox mcp",
  ],
  openGraph: {
    title: TITLE,
    description: DESC,
    url: `${SITE_URL}/mcp`,
    type: "website",
    images: [{ url: "/og-image.png", width: 1200, height: 630 }],
  },
  twitter: { card: "summary_large_image", title: TITLE, description: DESC },
};

// Each answer is written to survive being quoted on its own, with no
// surrounding page. That is the unit an answer engine actually lifts.
const FAQ: FaqEntry[] = [
  {
    question: "How do I give my AI agent an email address?",
    answer:
      "Add the e2a MCP server to your agent's client and authorize it in the browser. In Claude Code that is a single command: `claude mcp add --transport http --scope user e2a https://api.e2a.dev/mcp`, then `/mcp` to authorize. The agent gets its own address on agents.e2a.dev — or on a domain you own — and can send, receive, reply, and forward mail from that moment on.",
  },
  {
    question: "Do I need an API key to use the e2a MCP server?",
    answer:
      "No. The hosted MCP server at https://api.e2a.dev/mcp uses OAuth 2.1 with Dynamic Client Registration (RFC 7591) and PKCE, so an interactive client registers itself and completes authorization in the browser with no secret to paste. API keys exist for headless and CI use, where a browser flow is not available.",
  },
  {
    question: "Which MCP clients does e2a work with?",
    answer:
      "Any client that speaks Streamable HTTP MCP. e2a is tested with Claude Code, OpenAI Codex, Cursor, Windsurf, Claude Desktop, and VS Code with GitHub Copilot. All of them point at the same endpoint, https://api.e2a.dev/mcp; only the configuration syntax differs.",
  },
  {
    question: "How is this different from connecting an agent to Gmail?",
    answer:
      "A Gmail integration lets an agent read a human's mailbox. With e2a the inbox belongs to the agent itself: it has its own verified sending identity, so recipients can tell agent mail apart from yours, and revoking it does not touch your personal mail. e2a also verifies SPF, DKIM, and DMARC on every inbound message and hands the agent that authentication evidence, which a mailbox integration does not.",
  },
  {
    question: "Can I review what the agent sends before it goes out?",
    answer:
      "Yes. Human-in-the-loop approval can be enabled per agent. When it is on, an outbound send is held and you get an approve or reject prompt — in the dashboard, the CLI, or straight from your own inbox — and the message only leaves once you approve it.",
  },
  {
    question: "Is e2a open source?",
    answer:
      "Yes. The core is Apache-2.0 at github.com/tokencanopy/e2a — Go backend, SMTP relay, TypeScript and Python SDKs, CLI, MCP server, and dashboard — and can be self-hosted against your own Postgres and outbound relay. e2a.dev is the hosted version of that same codebase.",
  },
];

const CLIENTS: {
  id: string;
  name: string;
  intro: string;
  lang: string;
  code: string;
  after?: string;
}[] = [
  {
    id: "claude-code",
    name: "Claude Code",
    intro: "One command, then authorize in the browser.",
    lang: "sh",
    code: `claude mcp add --transport http --scope user e2a ${MCP_URL}`,
    after:
      "Run /mcp in Claude Code and authorize e2a. --scope user makes it available in every project; omit it to configure only the current one.",
  },
  {
    id: "codex",
    name: "OpenAI Codex",
    intro: "Add the server, then log in.",
    lang: "sh",
    code: `codex mcp add e2a --url ${MCP_URL}\ncodex mcp login e2a`,
  },
  {
    id: "cursor",
    name: "Cursor · Windsurf · Claude Desktop",
    intro: "Add the endpoint to the client's MCP configuration.",
    lang: "json",
    code: `{
  "mcpServers": {
    "e2a": { "url": "${MCP_URL}" }
  }
}`,
    after: "Complete the OAuth prompt when the client opens it.",
  },
  {
    id: "vscode",
    name: "VS Code with GitHub Copilot",
    intro: "Add .vscode/mcp.json with the servers key.",
    lang: "json",
    code: `{
  "servers": {
    "e2a": {
      "type": "http",
      "url": "${MCP_URL}"
    }
  }
}`,
  },
];

const HOW_TO = howTo({
  name: "Give an AI agent an email address over MCP",
  description:
    "Connect an MCP client to the hosted e2a server so the agent has its own authenticated inbox.",
  steps: [
    {
      name: "Add the MCP server",
      text: `Point your MCP client at ${MCP_URL} over Streamable HTTP. In Claude Code: claude mcp add --transport http --scope user e2a ${MCP_URL}`,
    },
    {
      name: "Authorize in the browser",
      text: "Trigger the client's authorization flow (/mcp in Claude Code, codex mcp login e2a in Codex). e2a uses OAuth 2.1 with Dynamic Client Registration, so there is no API key to paste.",
    },
    {
      name: "Claim an inbox",
      text: "Pick a slug on the shared agents.e2a.dev domain, or verify a domain you own with a DNS TXT record. The agent now has an address it can send from and receive to.",
    },
    {
      name: "Send and receive",
      text: "Ask the agent to send a message and reply to it from your own mail client. Inbound arrives with SPF, DKIM, and DMARC results attached and a stable conversation_id that threads the exchange.",
    },
  ],
});

export default function McpPage() {
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
          HOW_TO,
          breadcrumbs([
            { name: "e2a", path: "/" },
            { name: "MCP", path: "/mcp" },
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
            <Link href="/blog" className="text-[13px]" style={{ color: "var(--fg-muted)" }}>
              Blog
            </Link>
            <Link href="/api-docs" className="text-[13px]" style={{ color: "var(--fg-muted)" }}>
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
        <p
          className="font-mono text-[11px] mb-3"
          style={{ color: "var(--fg-subtle)", letterSpacing: "0.08em" }}
        >
          MCP SERVER
        </p>
        <h1
          className="text-[34px] md:text-[40px] font-semibold leading-[1.12] mb-5"
          style={{ letterSpacing: "-0.025em" }}
        >
          Email over MCP — give your AI agent a real inbox
        </h1>

        {/* The lede is the extractable answer. Keep it self-contained: state
            what the product does, for whom, and what it costs to start. */}
        <p className="text-[17px] leading-[1.6]" style={{ color: "var(--fg-muted)" }}>
          e2a is an MCP server that gives an AI agent its own authenticated email
          address. Connect Claude Code, Codex, Cursor, or any Streamable HTTP MCP
          client in one command and the agent can send, receive, reply, and
          forward real mail — with SPF/DKIM-verified inbound and an optional
          approval hold before anything leaves. OAuth 2.1, no API key, free tier.
        </p>

        <Endpoint />

        <Section id="connect" title="How do I connect my MCP client?">
          <P>
            Point the client at <Code>{MCP_URL}</Code> over Streamable HTTP and
            authorize in the browser. e2a uses OAuth 2.1 with Dynamic Client
            Registration, so the client registers itself — there is no key to
            paste and no secret to store.
          </P>
          {CLIENTS.map((c) => (
            <div key={c.id} className="mt-7">
              <h3 className="text-[15px] font-semibold mb-1.5">{c.name}</h3>
              <p className="text-[14px] mb-2.5" style={{ color: "var(--fg-muted)" }}>
                {c.intro}
              </p>
              <Pre lang={c.lang}>{c.code}</Pre>
              {c.after ? (
                <p className="text-[13.5px] mt-2.5" style={{ color: "var(--fg-muted)" }}>
                  {c.after}
                </p>
              ) : null}
            </div>
          ))}
          <P className="mt-7">
            For Goose, Zed, and other MCP clients, use the same endpoint.
            Ready-to-paste configurations live in the{" "}
            <A href="https://github.com/tokencanopy/e2a/tree/main/plugins/e2a/clients">
              client examples
            </A>
            . For headless or CI runs where no browser is available, pass an
            account API key instead — see{" "}
            <A href="https://e2a.dev/setup.md">setup.md</A>.
          </P>
        </Section>

        <Section id="capabilities" title="What can the agent do once it is connected?">
          <P>
            The MCP server exposes the full mail surface as tools, so the agent
            works in its inbox the way a person works in theirs:
          </P>
          <ul className="mt-3 space-y-2 text-[15px]" style={{ color: "var(--fg-muted)" }}>
            {[
              ["Send and reply", "Compose new mail, reply in-thread, forward, and attach files."],
              ["Read inbound", "List conversations and messages, and pull attachments."],
              ["Manage identity", "Create agents, claim a slug, or register and verify a custom domain."],
              ["Route and screen", "Suppressions, contacts, templates, and webhooks for downstream systems."],
            ].map(([label, body]) => (
              <li key={label} className="flex gap-2.5">
                <span style={{ color: "var(--fg-subtle)" }}>—</span>
                <span>
                  <strong style={{ color: "var(--fg)" }}>{label}.</strong> {body}
                </span>
              </li>
            ))}
          </ul>
        </Section>

        <Section id="different" title="How is this different from a Gmail MCP server?">
          <P>
            A Gmail integration lets an agent read a human&rsquo;s mailbox. With
            e2a <em>the inbox is the agent</em> — it has its own verified sending
            identity, so recipients can tell agent mail from yours, and revoking
            it never touches your personal mail.
          </P>
          <P className="mt-3.5">
            Every inbound message is checked for SPF, DKIM, and DMARC before the
            agent sees it, and the result is handed over as structured evidence
            rather than a header the agent has to parse and trust. Optional
            prompt-injection and phishing screening runs on the same path, and
            human-in-the-loop approval can hold any outbound send until you
            approve it from the dashboard, the CLI, or your own inbox.
          </P>
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
                <p className="text-[14.5px] leading-[1.6]" style={{ color: "var(--fg-muted)" }}>
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
            Free tier, no card. Add the server, authorize, and send your first
            message in about a minute.
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

function Endpoint() {
  return (
    <div
      className="mt-7 flex flex-wrap items-baseline gap-x-3 gap-y-1 px-4 py-3"
      style={{
        border: "1px solid var(--border)",
        borderRadius: "var(--r-md)",
      }}
    >
      <span
        className="font-mono text-[11px]"
        style={{ color: "var(--fg-subtle)", letterSpacing: "0.06em" }}
      >
        ENDPOINT
      </span>
      <code className="font-mono text-[13.5px]" style={{ color: "var(--fg)" }}>
        {MCP_URL}
      </code>
      <span className="font-mono text-[11.5px]" style={{ color: "var(--fg-subtle)" }}>
        Streamable HTTP · OAuth 2.1
      </span>
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
      {/* Question-shaped H2 with the answer immediately below it — the shape
          both featured snippets and LLM extraction key off. */}
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

function P({
  children,
  className = "",
}: {
  children: React.ReactNode;
  className?: string;
}) {
  return (
    <p
      className={`text-[15px] leading-[1.65] ${className}`}
      style={{ color: "var(--fg-muted)" }}
    >
      {children}
    </p>
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

function Code({ children }: { children: React.ReactNode }) {
  return (
    <code
      className="font-mono text-[13.5px] px-1.5 py-0.5"
      style={{
        background: "var(--bg-sunken)",
        borderRadius: "var(--r-sm)",
        color: "var(--fg)",
      }}
    >
      {children}
    </code>
  );
}

function Pre({ lang, children }: { lang: string; children: string }) {
  return (
    <pre
      className="overflow-x-auto px-4 py-3.5 text-[12.5px] leading-[1.65] font-mono"
      style={{
        background: "var(--ink)",
        border: "1px solid var(--ink-border)",
        borderRadius: "var(--r-lg)",
        color: "var(--ink-fg)",
      }}
    >
      <code data-lang={lang}>{children}</code>
    </pre>
  );
}
