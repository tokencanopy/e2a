// Central post registry. Every post is listed here and referenced by slug.
// Keeping metadata in one place lets the index page and sitemap share a
// single source of truth without parsing MDX frontmatter at build time.

export type Post = {
  slug: string;
  title: string;
  description: string;
  /** ISO date string (YYYY-MM-DD). Drives ordering + sitemap lastModified. */
  date: string;
  author: string;
  /** Rough reading time in minutes — shown on the index card. */
  readingMinutes: number;
};

export const posts: Post[] = [
  {
    slug: "send-email-from-python-agent",
    title: "Send real email from a Python AI agent in 20 lines",
    description:
      "A minimal walkthrough: give your Python agent an email address, send verified mail, receive replies via webhook or WebSocket, and thread the whole conversation.",
    date: "2026-04-13",
    author: "e2a",
    readingMinutes: 5,
  },
  {
    slug: "email-for-openclaw-agents",
    title: "Give your OpenClaw agent an email address (with real-time WebSocket delivery)",
    description:
      "OpenClaw runs locally, so by default it has no public inbox. This tutorial wires it to a real email address via e2a — WebSocket delivery, no public URL, under 5 minutes.",
    date: "2026-04-19",
    author: "e2a",
    readingMinutes: 5,
  },
  {
    slug: "human-in-the-loop-for-agent-email",
    title: "Human-in-the-loop: approve before your agent hits send",
    description:
      "Per-agent approval for outbound AI email. Flip one switch; the next time your agent tries to send, you get a notification with approve / reject buttons. Review in the dashboard, CLI, or straight from your inbox.",
    date: "2026-04-24",
    author: "e2a",
    readingMinutes: 6,
  },
  {
    slug: "email-agent-with-google-adk",
    title: "Give your Google ADK agent an email inbox (with conversation memory)",
    description:
      "Wire a Google Agent Development Kit agent to a real email address. HMAC-verified webhooks, multi-turn memory across email replies via the conversation_id ↔ ADK session_id binding, ~30 lines of business logic. Full runnable example included.",
    date: "2026-04-27",
    author: "e2a",
    readingMinutes: 7,
  },
  {
    slug: "build-ecommerce-agent-with-email",
    title: "Build an e-commerce agent that handles order email",
    description:
      "Connect an AI agent to customer order email: look up order context, answer delivery questions, keep replies threaded, and hold refunds for human approval.",
    date: "2026-08-08",
    author: "e2a",
    readingMinutes: 6,
  },
  {
    slug: "build-sales-agent-with-email",
    title: "Build a sales follow-up agent with authenticated email",
    description:
      "Give a sales agent its own inbox, connect replies to CRM context, send personalized follow-ups, and add a human approval step for sensitive outreach.",
    date: "2026-08-08",
    author: "e2a",
    readingMinutes: 6,
  },
  {
    slug: "build-recruiting-agent-with-email",
    title: "Build a recruiting agent that coordinates candidates by email",
    description:
      "Connect a recruiting agent to candidate email, ATS and calendar context, interview coordination, and human approval for high-impact messages.",
    date: "2026-08-08",
    author: "e2a",
    readingMinutes: 6,
  },
  {
    slug: "build-voice-follow-up-agent-with-email",
    title: "Build a voice follow-up agent with email",
    description:
      "Turn a completed voice call into an authenticated email follow-up, receive replies, preserve call context, and route commitments through approval.",
    date: "2026-08-08",
    author: "e2a",
    readingMinutes: 5,
  },
  {
    slug: "build-procurement-agent-with-email",
    title: "Build a procurement agent that coordinates with vendors",
    description:
      "Connect vendor email to purchase-order context, extract updates, follow up on missing information, and hold commitments for human approval.",
    date: "2026-08-08",
    author: "e2a",
    readingMinutes: 5,
  },
  {
    slug: "approval-gate-in-infrastructure",
    title: "Your approval gate shouldn't live in your agent's code",
    description:
      "Most teams put the human-approval checkpoint in their own application code, where it only holds for the code paths that remember to call it. Where the gate should live instead: the email API boundary.",
    date: "2026-09-04",
    author: "e2a",
    readingMinutes: 3,
  },
  {
    slug: "inbox-is-transport",
    title: "Your agent's inbox is storage, not transport",
    description:
      "An agent that polls its inbox every 30 minutes is checking a PO box. Inbound mail should push to the agent wherever it runs - signed webhooks, WebSocket with no public URL, REST polling, and MCP - without a deployment project first.",
    date: "2026-09-05",
    author: "e2a",
    readingMinutes: 3,
  },
  {
    slug: "inbound-prompt-injection",
    title: "Anyone in the world can put text in front of your agent's model for the price of an email",
    description:
      "Inbound email is untrusted input the whole internet can write - and a prime indirect prompt-injection vector for agents that act on what they read. How e2a screens message content (heuristics + optional LLM detector, allow / review / block, fail-safe to review) before your agent ever sees it.",
    date: "2026-09-06",
    author: "e2a",
    readingMinutes: 3,
  },
];

export function getPost(slug: string): Post | undefined {
  return posts.find((p) => p.slug === slug);
}

export function getPostsSortedByDate(): Post[] {
  return [...posts].sort((a, b) => (a.date < b.date ? 1 : -1));
}
