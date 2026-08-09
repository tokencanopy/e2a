import type { Metadata } from "next";
import Link from "next/link";
import { notFound } from "next/navigation";
import { SITE_URL } from "../../../lib/site";
import {
  breadcrumbs,
  faqPage,
  howTo,
  type FaqEntry,
} from "../../../lib/jsonld";
import { JsonLd } from "../../components/JsonLd";

type UseCase = {
  slug: string;
  eyebrow: string;
  title: string;
  description: string;
  summary: string;
  keywords: string[];
  workflow: string[];
  capabilities: string[];
  exampleLabel: string;
  exampleHref: string;
  codeLanguage: string;
  code: string;
  faqs: FaqEntry[];
  cta: string;
};

const USE_CASES: Record<string, UseCase> = {
  "support-agent": {
    slug: "support-agent",
    eyebrow: "USE CASE · SUPPORT",
    title: "Build an AI support agent that works from a real inbox",
    description:
      "Give an AI support agent a real inbox. Receive customer email, verify sender identity, reply in-thread, and hold sensitive outbound messages for human approval with e2a.",
    summary:
      "e2a lets developers build support agents that receive customer email at a real address, inspect SPF/DKIM/DMARC authentication evidence, draft or send a threaded reply, and route sensitive outbound messages through human approval.",
    keywords: [
      "AI support agent email",
      "build an email support agent",
      "autonomous customer support email agent",
      "AI agent that receives and replies to email",
      "human approval for AI support email",
    ],
    workflow: [
      "A customer emails your support address.",
      "e2a receives the message and evaluates sender authentication.",
      "Your agent reads the structured event and retrieves application context.",
      "The agent classifies the request and drafts a response.",
      "Low-risk replies send automatically; sensitive replies wait for approval.",
      "The response stays in the original email conversation.",
    ],
    capabilities: [
      "SPF, DKIM, and DMARC evidence on inbound mail",
      "Signed webhooks, WebSockets, REST polling, or MCP delivery",
      "Conversation correlation and threaded replies",
      "Per-agent human-in-the-loop approval",
      "Optional inbound prompt-injection and phishing screening",
    ],
    exampleLabel: "Support agent runbook",
    exampleHref: "https://github.com/tokencanopy/e2a-runbooks",
    codeLanguage: "python",
    code: `async for event in client.listen("support@example.com"):\n    if event.type != "email.received":\n        continue\n\n    message = event.data\n    reply = await support_agent.run(\n        message["conversation_id"],\n        message["text"],\n    )\n    await client.messages.reply(\n        message["delivered_to"],\n        message["message_id"],\n        {"text": reply},\n    )`,
    faqs: [
      {
        question: "Can an AI support agent receive email without a public URL?",
        answer:
          "Yes. An e2a support agent can receive inbound email over a WebSocket stream, REST polling, or MCP, so a local or private application does not need to expose a public webhook endpoint.",
      },
      {
        question: "How does e2a verify a support request's sender?",
        answer:
          "e2a evaluates SPF, every DKIM signature, and DMARC on inbound mail, then delivers the authentication results as structured evidence for the support agent to use in its decision-making.",
      },
      {
        question: "Can a human approve an AI-generated support reply?",
        answer:
          "Yes. Human-in-the-loop approval can hold outbound messages for review before they are sent, with approval available through the dashboard, API, MCP tools, CLI, or a magic link.",
      },
      {
        question: "Does e2a preserve email threads?",
        answer:
          "Yes. e2a supports reply-in-thread delivery and exposes a stable conversation_id so the application can connect an email conversation to the support agent's own session state.",
      },
    ],
    cta: "Build your support agent",
  },
  "ai-receptionist": {
    slug: "ai-receptionist",
    eyebrow: "USE CASE · RECEPTIONIST",
    title: "Build an AI receptionist with its own inbox",
    description:
      "Build an AI receptionist that answers business inquiries, routes messages, forwards requests to the right person, and keeps a threaded record with e2a's authenticated agent inboxes.",
    summary:
      "e2a gives an AI receptionist a real email address so it can answer common inquiries, route messages to the right person, forward conversations, and keep human approval in the loop before sending sensitive replies.",
    keywords: [
      "AI receptionist email",
      "build an AI receptionist",
      "virtual receptionist email agent",
      "AI agent for business inquiries",
      "email receptionist for small business",
    ],
    workflow: [
      "A visitor sends an inquiry to the organization's receptionist address.",
      "The agent identifies the request type and checks whether it can answer.",
      "It answers routine questions using approved context.",
      "It forwards or escalates specialized requests to a team address.",
      "It records the conversation and follows up in-thread.",
    ],
    capabilities: [
      "A dedicated, real email identity for the agent",
      "Inbound delivery over MCP, WebSocket, REST, or webhook",
      "Reply, forward, and conversation-aware routing",
      "Labels and application-owned routing metadata",
      "Human approval before sensitive outbound mail",
    ],
    exampleLabel: "Receptionist runbook",
    exampleHref: "https://github.com/tokencanopy/e2a-runbooks",
    codeLanguage: "typescript",
    code: `const result = await receptionist.run({\n  conversationId: event.data.conversation_id,\n  request: event.data.text,\n});\n\nif (result.forwardTo) {\n  await client.messages.forward(agentEmail, messageId, {\n    to: [{ email: result.forwardTo }],\n  });\n} else {\n  await client.messages.reply(agentEmail, messageId, {\n    text: result.reply,\n  });\n}`,
    faqs: [
      {
        question: "What can an AI receptionist do with email?",
        answer:
          "An AI receptionist can answer routine inquiries, collect missing details, route messages, forward conversations to a human team, and follow up with the sender in the same email thread.",
      },
      {
        question: "Can an AI receptionist forward a message to a human team?",
        answer:
          "Yes. An agent using e2a can forward an inbound message to a human or team address while retaining the original conversation context in the application.",
      },
      {
        question: "How do I prevent an AI receptionist from sending an unsafe reply?",
        answer:
          "Enable e2a's per-agent human-in-the-loop approval gate so outbound messages are held for review before they leave the system.",
      },
      {
        question: "Can the receptionist work behind a firewall?",
        answer:
          "Yes. WebSocket delivery, REST polling, and MCP do not require a public URL, so a local receptionist can receive mail while remaining behind a firewall.",
      },
    ],
    cta: "Give your receptionist an inbox",
  },
  "scheduling-agent": {
    slug: "scheduling-agent",
    eyebrow: "USE CASE · SCHEDULING",
    title: "Build an AI scheduling agent that coordinates by email",
    description:
      "Build an AI scheduling agent that coordinates participants, proposes times, handles replies and rescheduling, and preserves conversation state across email with e2a.",
    summary:
      "e2a helps developers build scheduling agents that receive meeting requests, propose times, process replies, send confirmations, and preserve the agent's conversation state across multi-turn email threads.",
    keywords: [
      "AI scheduling agent email",
      "build an email scheduling agent",
      "AI assistant that schedules meetings by email",
      "scheduling secretary AI agent",
      "automated meeting coordination email",
    ],
    workflow: [
      "A participant emails the scheduling agent with a meeting request.",
      "The agent extracts attendees, constraints, and scheduling intent.",
      "It checks the application's availability source.",
      "It proposes times and waits for a reply.",
      "It updates state when participants accept, decline, or request changes.",
      "It sends a final confirmation and future reminders when appropriate.",
    ],
    capabilities: [
      "Inbound webhook, WebSocket, REST, or MCP delivery",
      "conversation_id correlation for multi-turn state",
      "Threaded replies and message history",
      "Scheduled sending for supported beta workflows",
      "Optional review before confirmations or reminders are sent",
    ],
    exampleLabel: "Scheduling secretary runbook",
    exampleHref: "https://github.com/tokencanopy/e2a-runbooks",
    codeLanguage: "python",
    code: `event = await client.inbound.next("scheduler@example.com")\nrequest = await scheduler.extract(event.data["text"])\n\nreply = await scheduler.propose_times(request)\nawait client.messages.reply(\n    event.data["delivered_to"],\n    event.data["message_id"],\n    {"text": reply},\n)\n# Bind conversation_id to your scheduler's session state`,
    faqs: [
      {
        question: "Can an AI scheduling agent handle multiple replies in one conversation?",
        answer:
          "Yes. e2a preserves reply threading and exposes conversation_id so the application can rebuild scheduling state across multiple email turns.",
      },
      {
        question: "How does an email scheduling agent remember the meeting context?",
        answer:
          "The application binds e2a's conversation_id to its own session or workflow state, while e2a handles inbound delivery, reply routing, and the email thread.",
      },
      {
        question: "Can a scheduling agent work with local or private applications?",
        answer:
          "Yes. The agent can consume mail over WebSocket, REST polling, or MCP without exposing a public webhook endpoint, which works well for private calendar and scheduling systems.",
      },
      {
        question: "Can scheduled messages be reviewed before they are sent?",
        answer:
          "For workflows using e2a's supported scheduled-sending and review features, outbound messages can be configured to require human approval before dispatch.",
      },
    ],
    cta: "Build your scheduling agent",
  },
  "ecommerce-agent": {
    slug: "ecommerce-agent",
    eyebrow: "USE CASE · E-COMMERCE",
    title: "Build an e-commerce agent that handles customer email",
    description:
      "Build an e-commerce agent that answers order questions, handles returns, coordinates with vendors, and keeps customers updated through authenticated email with e2a.",
    summary:
      "e2a helps developers build e-commerce agents that receive customer and vendor email, look up order context, send threaded updates, and route refunds or sensitive actions through human approval.",
    keywords: [
      "e-commerce AI agent email",
      "AI agent for e-commerce customer support",
      "build an order support agent",
      "AI returns agent email",
      "e-commerce email automation agent",
    ],
    workflow: [
      "A customer emails about an order, delivery, return, or product question.",
      "e2a delivers the message with structured sender-authentication evidence.",
      "The agent looks up the order or customer context in the commerce system.",
      "It answers routine questions and keeps the reply in the same thread.",
      "Refunds, replacements, and unusual requests are held for human approval.",
      "The agent follows up with the customer or coordinates with a vendor.",
    ],
    capabilities: [
      "Dedicated inboxes for customer service or vendor agents",
      "Inbound authentication evidence and signed delivery",
      "Threaded replies for order and return conversations",
      "MCP, SDK, webhook, WebSocket, or REST integration",
      "Human approval for refunds, replacements, and sensitive sends",
    ],
    exampleLabel: "E-commerce support workflow",
    exampleHref: "https://e2a.dev/use-cases/support-agent",
    codeLanguage: "python",
    code: `event = await client.inbound.next("orders@example.com")\norder = await shop.lookup_order(event.data["text"])\n\nreply = await commerce_agent.answer(\n    email=event.data["text"],\n    order=order,\n)\nawait client.messages.reply(\n    event.data["delivered_to"],\n    event.data["message_id"],\n    {"text": reply},\n)`,
    faqs: [
      {
        question: "What can an e-commerce agent do with email?",
        answer:
          "An e-commerce agent can answer order-status questions, explain shipping updates, collect return details, coordinate with vendors, and keep customers informed in persistent email threads.",
      },
      {
        question: "Can an e-commerce agent handle returns or refunds?",
        answer:
          "Yes. The agent can gather the request and call the commerce system, while e2a's human-in-the-loop approval gate can require a person to review a refund, replacement, or other sensitive outbound action before it is sent.",
      },
      {
        question: "Can the agent connect email to order data?",
        answer:
          "Yes. e2a delivers the email event and conversation context; the agent connects that input to the store, order-management, shipping, or CRM APIs that contain the business data.",
      },
      {
        question: "Can an e-commerce agent coordinate with vendors?",
        answer:
          "Yes. A dedicated vendor or procurement inbox can receive supplier messages, route them to the right workflow, and send threaded follow-ups through e2a.",
      },
    ],
    cta: "Build your e-commerce agent",
  },
  "sales-agent": {
    slug: "sales-agent",
    eyebrow: "USE CASE · SALES",
    title: "Build a sales agent that keeps every follow-up moving",
    description:
      "Build a sales agent that qualifies inbound interest, follows up with leads, and keeps conversations moving through a verified email identity with e2a.",
    summary:
      "e2a helps developers build sales agents that receive replies, qualify opportunities, send personalized follow-ups, and route high-impact or unfamiliar outreach through human approval.",
    keywords: [
      "AI sales agent email",
      "build an AI email sales agent",
      "automated lead follow-up agent",
      "AI agent for sales outreach",
      "human approval for AI sales email",
    ],
    workflow: [
      "A lead replies to an email or sends a new inquiry.",
      "e2a delivers the message with sender-authentication evidence.",
      "The agent enriches the lead with CRM or application context.",
      "It qualifies intent and drafts a relevant next step.",
      "Routine follow-ups send automatically; sensitive outreach waits for approval.",
      "The agent preserves the thread and schedules the next action.",
    ],
    capabilities: [
      "Dedicated, verified identity for the sales agent",
      "Inbound delivery over MCP, SDK, webhook, WebSocket, or REST",
      "Thread-aware replies and conversation correlation",
      "Templates and scheduled sending for supported beta workflows",
      "Human approval and recipient safeguards for outbound mail",
    ],
    exampleLabel: "Sales follow-up workflow",
    exampleHref: "https://e2a.dev/use-cases/support-agent",
    codeLanguage: "typescript",
    code: `const lead = await crm.lookupByEmail(event.data.header_from);\nconst nextStep = await salesAgent.run({\n  lead,\n  message: event.data.text,\n  conversationId: event.data.conversation_id,\n});\n\nawait client.messages.reply(agentEmail, messageId, {\n  text: nextStep.reply,\n});`,
    faqs: [
      {
        question: "What can an AI sales agent do with email?",
        answer:
          "An AI sales agent can qualify inbound interest, answer product questions, personalize follow-ups, keep a lead's conversation moving, and hand important decisions to a human.",
      },
      {
        question: "Can a sales agent use CRM data?",
        answer:
          "Yes. e2a provides the email event and conversation context; the agent can connect that input to the CRM, product, or business systems that contain lead data.",
      },
      {
        question: "Can I approve sales emails before they are sent?",
        answer:
          "Yes. e2a's human-in-the-loop approval gate can hold outbound sales messages for review before dispatch, with per-agent policies for which messages require approval.",
      },
      {
        question: "Does e2a support scheduled follow-ups?",
        answer:
          "Scheduled sending is available for supported beta workflows. The application can also implement its own scheduling and use e2a when the next message is ready to send.",
      },
    ],
    cta: "Build your sales agent",
  },
  "recruiting-agent": {
    slug: "recruiting-agent",
    eyebrow: "USE CASE · RECRUITING",
    title: "Build a recruiting agent that coordinates candidates by email",
    description:
      "Build a recruiting agent that answers candidate questions, coordinates interviews, sends updates, and keeps hiring conversations organized with e2a.",
    summary:
      "e2a helps developers build recruiting agents that receive candidate email, connect it to hiring workflow context, coordinate interviews, and hold sensitive or high-impact messages for human approval.",
    keywords: [
      "AI recruiting agent email",
      "build an AI recruiting assistant",
      "AI agent for candidate communication",
      "automated interview scheduling email",
      "recruiting email automation agent",
    ],
    workflow: [
      "A candidate replies to a recruiter or sends a hiring question.",
      "e2a delivers the message with structured authentication evidence.",
      "The agent retrieves the candidate, role, and hiring-stage context.",
      "It answers routine questions or proposes interview times.",
      "Rejections, offers, and sensitive updates wait for human approval.",
      "The agent follows up in the same conversation and updates the hiring system.",
    ],
    capabilities: [
      "Dedicated inboxes for recruiting or coordination agents",
      "Threaded candidate conversations with application-owned state",
      "MCP, SDK, webhook, WebSocket, or REST delivery",
      "Integration with ATS, calendar, and hiring workflow APIs",
      "Human approval for high-impact candidate communication",
    ],
    exampleLabel: "Scheduling agent workflow",
    exampleHref: "https://e2a.dev/use-cases/scheduling-agent",
    codeLanguage: "python",
    code: `event = await client.inbound.next("hiring@example.com")\ncandidate = await ats.lookup(event.data["header_from"])\nreply = await recruiting_agent.run(\n    candidate=candidate,\n    message=event.data["text"],\n    conversation_id=event.data["conversation_id"],\n)\nawait client.messages.reply(\n    event.data["delivered_to"],\n    event.data["message_id"],\n    {"text": reply},\n)`,
    faqs: [
      {
        question: "What can a recruiting agent do with email?",
        answer:
          "A recruiting agent can answer candidate questions, coordinate interviews, send reminders and updates, collect information, and keep hiring conversations connected to the recruiting system.",
      },
      {
        question: "Can a recruiting agent schedule interviews?",
        answer:
          "Yes. A recruiting agent can coordinate availability through email and connect to a calendar or scheduling API, while e2a handles the inbox, delivery, and threaded replies.",
      },
      {
        question: "Can humans approve recruiting messages?",
        answer:
          "Yes. Enable e2a's human-in-the-loop approval gate for high-impact communications such as rejections, offers, or messages to unfamiliar recipients.",
      },
      {
        question: "Can candidate data stay in our own systems?",
        answer:
          "Yes. The agent can retrieve hiring context from your ATS and application systems; e2a provides the email transport and conversation context rather than replacing those systems.",
      },
    ],
    cta: "Build your recruiting agent",
  },
  "voice-agent": {
    slug: "voice-agent",
    eyebrow: "USE CASE · VOICE",
    title: "Build a voice agent that follows up by email",
    description:
      "Build a voice agent that sends a follow-up after a call, receives replies, and keeps the conversation moving with a real authenticated inbox from e2a.",
    summary:
      "e2a gives voice agents a real email identity so they can turn calls into follow-ups, receive replies, preserve conversation context, and route sensitive commitments through human approval.",
    keywords: [
      "voice AI agent email",
      "voice agent email follow-up",
      "build a voice agent that sends email",
      "AI phone agent email integration",
      "voice assistant email inbox",
    ],
    workflow: [
      "A voice agent completes a call and records the outcome.",
      "The application creates a concise follow-up from the call context.",
      "e2a sends the follow-up from the voice agent's verified address.",
      "The recipient replies by email and e2a delivers the message to the agent.",
      "The agent answers, escalates, or schedules the next action.",
      "Sensitive commitments can wait for human approval before sending.",
    ],
    capabilities: [
      "A persistent email identity for the voice agent",
      "Outbound HTTP API and threaded replies",
      "Inbound WebSocket, webhook, REST, or MCP delivery",
      "conversation_id correlation with call or CRM state",
      "Human approval for commitments and sensitive follow-ups",
    ],
    exampleLabel: "Follow-up agent workflow",
    exampleHref: "https://e2a.dev/use-cases/support-agent",
    codeLanguage: "typescript",
    code: `const followUp = await voiceAgent.summarizeCall(call);\nawait client.messages.send(agentEmail, {\n  to: [{ email: call.recipient }],\n  subject: followUp.subject,\n  text: followUp.text,\n  conversation_id: call.id,\n});\n\n// Later: receive and reply to the email conversation`,
    faqs: [
      {
        question: "What can a voice agent do with email?",
        answer:
          "A voice agent can send a call summary, confirm next steps, receive a reply, answer follow-up questions, and continue the interaction over a persistent email conversation.",
      },
      {
        question: "Can the email reply reconnect to the original call?",
        answer:
          "Yes. The application can bind e2a's conversation_id to the call, CRM record, or voice-session identifier so later email turns reconnect to the original workflow.",
      },
      {
        question: "Can a human review a voice agent's follow-up?",
        answer:
          "Yes. e2a's human-in-the-loop approval gate can hold follow-ups for review before they are delivered to the recipient.",
      },
      {
        question: "Does the voice agent need a public webhook?",
        answer:
          "No. The email side of the workflow can use WebSocket delivery, REST polling, or MCP when the voice application runs locally or behind a firewall.",
      },
    ],
    cta: "Build your voice follow-up agent",
  },
  "procurement-agent": {
    slug: "procurement-agent",
    eyebrow: "USE CASE · PROCUREMENT",
    title: "Build a procurement agent that coordinates with vendors",
    description:
      "Build a procurement agent that requests quotes, follows up on purchase orders, tracks vendor replies, and keeps supplier conversations organized with e2a.",
    summary:
      "e2a helps developers build procurement agents that receive vendor email, connect messages to purchase-order context, send follow-ups, and require human approval before sensitive commitments leave the organization.",
    keywords: [
      "AI procurement agent email",
      "vendor management AI agent",
      "purchase order email automation agent",
      "AI agent for supplier communication",
      "procurement email workflow automation",
    ],
    workflow: [
      "A vendor sends a quote, shipping update, invoice, or question.",
      "e2a delivers the message with sender-authentication evidence.",
      "The agent matches it to a purchase order or vendor record.",
      "It extracts the update and requests missing information when needed.",
      "Pricing, purchase, and contractual commitments wait for human approval.",
      "The agent follows up with the vendor and updates the procurement system.",
    ],
    capabilities: [
      "Dedicated inboxes for procurement or vendor workflows",
      "Inbound authentication evidence and signed webhooks",
      "Threaded vendor replies and conversation correlation",
      "MCP, SDK, webhook, WebSocket, or REST integration",
      "Approval controls for commitments and unfamiliar recipients",
    ],
    exampleLabel: "E-commerce and vendor workflow",
    exampleHref: "https://e2a.dev/use-cases/ecommerce-agent",
    codeLanguage: "python",
    code: `event = await client.inbound.next("vendors@example.com")\npo = await procurement.lookup_purchase_order(\n    event.data["text"]\n)\nreply = await procurement_agent.follow_up(\n    message=event.data["text"],\n    purchase_order=po,\n)\nawait client.messages.reply(\n    event.data["delivered_to"],\n    event.data["message_id"],\n    {"text": reply},\n)`,
    faqs: [
      {
        question: "What can a procurement agent do with email?",
        answer:
          "A procurement agent can request quotes, extract vendor updates, chase purchase orders, ask for missing information, and keep supplier conversations connected to the procurement system.",
      },
      {
        question: "Can a procurement agent read purchase-order context?",
        answer:
          "Yes. The agent can connect inbound email to the purchase-order, vendor-management, and inventory systems that contain the organization's business context.",
      },
      {
        question: "Can humans approve vendor commitments?",
        answer:
          "Yes. e2a can hold messages for human review before sending pricing, contractual, purchasing, or other sensitive commitments.",
      },
      {
        question: "Can procurement agents work with vendors that do not use e2a?",
        answer:
          "Yes. e2a sends and receives ordinary email over SMTP, so vendors can use Gmail, Outlook, or any other normal mail client.",
      },
    ],
    cta: "Build your procurement agent",
  },
};

export function generateStaticParams() {
  return Object.keys(USE_CASES).map((slug) => ({ slug }));
}

export async function generateMetadata({
  params,
}: {
  params: Promise<{ slug: string }>;
}): Promise<Metadata> {
  const { slug } = await params;
  const useCase = USE_CASES[slug];
  if (!useCase) return {};
  return {
    title: { absolute: `${useCase.title} | e2a` },
    description: useCase.description,
    keywords: useCase.keywords,
    alternates: { canonical: `/use-cases/${useCase.slug}` },
    openGraph: {
      title: `${useCase.title} | e2a`,
      description: useCase.description,
      url: `${SITE_URL}/use-cases/${useCase.slug}`,
      type: "website",
      images: [{ url: "/og-image.png", width: 1200, height: 630 }],
    },
    twitter: {
      card: "summary_large_image",
      title: `${useCase.title} | e2a`,
      description: useCase.description,
    },
  };
}

export default async function UseCasePage({
  params,
}: {
  params: Promise<{ slug: string }>;
}) {
  const { slug } = await params;
  const useCase = USE_CASES[slug];
  if (!useCase) notFound();

  const howToData = howTo({
    name: useCase.title,
    description: useCase.summary,
    steps: useCase.workflow.map((step, index) => ({
      name: `Step ${index + 1}`,
      text: step,
    })),
  });

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
          faqPage(useCase.faqs),
          howToData,
          breadcrumbs([
            { name: "e2a", path: "/" },
            { name: "Use cases", path: "/#use-cases" },
            { name: useCase.title, path: `/use-cases/${useCase.slug}` },
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
          <Link href="/" className="font-mono font-bold text-[17px]">
            e2a
          </Link>
          <div className="flex items-center gap-4 text-[13px]">
            <Link href="/#use-cases" style={{ color: "var(--fg-muted)" }}>
              Use cases
            </Link>
            <Link href="/docs" style={{ color: "var(--fg-muted)" }}>
              Docs
            </Link>
            <Link
              href="/get-started"
              className="px-3.5 py-1.5 font-medium"
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

      <main className="max-w-[920px] mx-auto px-6 md:px-8 py-14 md:py-20 pb-24">
        <header className="max-w-[760px]">
          <p
            className="font-mono text-[11px] mb-4"
            style={{ color: "var(--accent-strong)", letterSpacing: "0.08em" }}
          >
            {useCase.eyebrow}
          </p>
          <h1
            className="text-[38px] md:text-[56px] font-semibold leading-[1.08] mb-6"
            style={{ letterSpacing: "-0.035em" }}
          >
            {useCase.title}
          </h1>
          <p
            className="text-[18px] md:text-[20px] leading-[1.55] mb-6"
            style={{ color: "var(--fg-muted)" }}
          >
            {useCase.summary}
          </p>
          <div className="flex flex-wrap gap-3">
            <Link
              href="/get-started"
              className="inline-flex items-center gap-2 px-4 py-2.5 text-[14px] font-medium"
              style={{
                background: "var(--fg)",
                color: "var(--bg)",
                borderRadius: "var(--r-md)",
              }}
            >
              {useCase.cta} <span className="font-mono">→</span>
            </Link>
            <Link
              href={useCase.exampleHref}
              target="_blank"
              rel="noopener noreferrer"
              className="inline-flex items-center px-4 py-2.5 text-[14px]"
              style={{
                border: "1px solid var(--border-strong)",
                color: "var(--fg)",
                borderRadius: "var(--r-md)",
              }}
            >
              See the runnable example ↗
            </Link>
          </div>
        </header>

        <section className="grid md:grid-cols-[1fr_0.9fr] gap-8 md:gap-12 mt-16 md:mt-24">
          <div>
            <p
              className="font-mono text-[11px] mb-3"
              style={{ color: "var(--fg-subtle)", letterSpacing: "0.08em" }}
            >
              THE WORKFLOW
            </p>
            <h2 className="text-[26px] font-semibold mb-6" style={{ letterSpacing: "-0.02em" }}>
              What the agent does
            </h2>
            <ol className="space-y-4">
              {useCase.workflow.map((step, index) => (
                <li key={step} className="flex gap-3 text-[15px] leading-[1.55]">
                  <span
                    className="font-mono text-[12px] shrink-0 pt-0.5"
                    style={{ color: "var(--accent-strong)" }}
                  >
                    0{index + 1}
                  </span>
                  <span style={{ color: "var(--fg-muted)" }}>{step}</span>
                </li>
              ))}
            </ol>
          </div>

          <div
            className="p-6 md:p-7 self-start"
            style={{
              background: "var(--bg-panel)",
              border: "1px solid var(--border)",
              borderRadius: "var(--r-lg)",
            }}
          >
            <p
              className="font-mono text-[11px] mb-3"
              style={{ color: "var(--fg-subtle)", letterSpacing: "0.08em" }}
            >
              WHY E2A
            </p>
            <ul className="space-y-3">
              {useCase.capabilities.map((capability) => (
                <li key={capability} className="flex gap-2.5 text-[14px] leading-[1.5]">
                  <span style={{ color: "var(--accent-strong)" }}>✓</span>
                  <span style={{ color: "var(--fg-muted)" }}>{capability}</span>
                </li>
              ))}
            </ul>
          </div>
        </section>

        <section className="mt-16 md:mt-24">
          <div className="flex items-end justify-between gap-4 mb-4">
            <div>
              <p
                className="font-mono text-[11px] mb-3"
                style={{ color: "var(--fg-subtle)", letterSpacing: "0.08em" }}
              >
                START BUILDING
              </p>
              <h2 className="text-[26px] font-semibold" style={{ letterSpacing: "-0.02em" }}>
                The integration stays small
              </h2>
            </div>
            <Link href={useCase.exampleHref} target="_blank" rel="noopener noreferrer" className="text-[13px]" style={{ color: "var(--accent-strong)" }}>
              {useCase.exampleLabel} ↗
            </Link>
          </div>
          <pre
            className="overflow-x-auto p-5 md:p-6 text-[13px] leading-[1.65]"
            style={{
              background: "var(--bg-elev)",
              border: "1px solid var(--border)",
              borderRadius: "var(--r-lg)",
              color: "var(--fg)",
            }}
          >
            <code>{useCase.code}</code>
          </pre>
          <p className="text-[13px] leading-[1.6] mt-3" style={{ color: "var(--fg-subtle)" }}>
            The agent owns the business logic. e2a handles the email identity,
            delivery, authentication evidence, and reply routing.
          </p>
        </section>

        <section className="mt-16 md:mt-24">
          <p
            className="font-mono text-[11px] mb-3"
            style={{ color: "var(--fg-subtle)", letterSpacing: "0.08em" }}
          >
            KEEP GOING
          </p>
          <h2 className="text-[26px] font-semibold mb-5" style={{ letterSpacing: "-0.02em" }}>
            Connect the pieces
          </h2>
          <div className="grid sm:grid-cols-2 gap-3">
            {[
              ["Quickstart", "/docs", "Create an inbox and send your first message."],
              ["MCP server", "/mcp", "Give a local or hosted agent an inbox without an API key."],
              ["API reference", "/api-docs", "Explore the stable /v1 operations."],
              ["Blog tutorials", "/blog", "Follow framework-specific integration guides."],
            ].map(([label, href, text]) => (
              <Link
                key={href}
                href={href}
                className="p-4 transition hover:bg-[var(--bg-panel)]"
                style={{ border: "1px solid var(--border)", borderRadius: "var(--r-md)" }}
              >
                <div className="text-[14px] font-semibold mb-1">{label} →</div>
                <div className="text-[13px] leading-[1.5]" style={{ color: "var(--fg-muted)" }}>{text}</div>
              </Link>
            ))}
          </div>
        </section>

        <section className="mt-16 md:mt-24">
          <p
            className="font-mono text-[11px] mb-3"
            style={{ color: "var(--fg-subtle)", letterSpacing: "0.08em" }}
          >
            FAQ
          </p>
          <div>
            {useCase.faqs.map((faq) => (
              <div key={faq.question} className="py-5" style={{ borderTop: "1px solid var(--border)" }}>
                <h2 className="text-[15px] font-semibold mb-2">{faq.question}</h2>
                <p className="text-[14px] leading-[1.65]" style={{ color: "var(--fg-muted)" }}>{faq.answer}</p>
              </div>
            ))}
          </div>
        </section>

        <section className="mt-16 md:mt-24 text-center p-8 md:p-12" style={{ background: "var(--bg-elev)", border: "1px solid var(--border)", borderRadius: "var(--r-lg)" }}>
          <h2 className="text-[28px] font-semibold mb-3" style={{ letterSpacing: "-0.025em" }}>{useCase.cta}</h2>
          <p className="text-[15px] mb-6" style={{ color: "var(--fg-muted)" }}>Open-source, free to start, and ready for your first agent workflow.</p>
          <Link href="/get-started" className="inline-flex items-center gap-2 px-4 py-2.5 text-[14px] font-medium" style={{ background: "var(--fg)", color: "var(--bg)", borderRadius: "var(--r-md)" }}>
            Get started <span className="font-mono">→</span>
          </Link>
        </section>
      </main>
    </div>
  );
}
