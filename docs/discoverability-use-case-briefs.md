# e2a use-case discoverability briefs

Working briefs for the first three public use-case pages. Each page should be
useful to a developer evaluating e2a, indexable for a specific problem, and
easy for an answer engine to summarize accurately.

## Shared page contract

Every use-case page should include:

1. A one-sentence answer to the page's core question near the top.
2. The workflow in plain language and as a small diagram.
3. A runnable example linked to the relevant SDK, MCP setup, or runbook.
4. The e2a capabilities used, with links to canonical documentation.
5. A section explaining when e2a is and is not the right fit.
6. A self-contained FAQ rendered visibly and in valid FAQ structured data.
7. One primary CTA: build the workflow with e2a.

Use the canonical description on every page:

> e2a is an open-source, authenticated email gateway for AI agents. It gives
> each agent a real inbox, verifies inbound sender identity, and supports MCP,
> SDKs, webhooks, WebSockets, REST, and HTTP outbound email.

## 1. Support agent

**URL:** `/use-cases/support-agent`

**Primary intent:** A developer wants an AI support agent that can receive,
understand, and reply to customer email safely.

**Target queries:**

- AI support agent email
- build an email support agent
- autonomous customer support email agent
- AI agent that receives and replies to email
- human approval for AI support email

**Suggested title:** Build an AI support agent with email | e2a

**Suggested description:** Give an AI support agent a real inbox. Receive
customer email, verify sender identity, reply in-thread, and hold sensitive
outbound messages for human approval with e2a.

**H1:** Build an AI support agent that works from a real inbox

**Answer-engine summary:**

> e2a lets developers build support agents that receive customer email at a
> real address, inspect SPF/DKIM/DMARC authentication evidence, draft or send a
> threaded reply, and route sensitive outbound messages through human approval.

**Core workflow:**

1. Customer emails `support@example.com`.
2. e2a receives the message and evaluates sender authentication.
3. The agent reads the structured event and retrieves application context.
4. The agent classifies the request and drafts a response.
5. Low-risk replies send automatically; sensitive replies wait for approval.
6. The response stays in the original email conversation.

**Capabilities to demonstrate:** inbound authentication evidence, webhooks or
WebSocket delivery, conversation correlation, reply-in-thread, HITL review,
and optional inbound screening.

**Primary example:** Link to the existing support runbook and a minimal Python
or TypeScript webhook example.

**FAQ questions:**

- Can an AI support agent receive email without a public URL?
- How does e2a verify a support request's sender?
- Can a human approve an AI-generated support reply?
- Does e2a preserve email threads?

**Primary CTA:** Build your support agent → `/get-started`

## 2. AI receptionist

**URL:** `/use-cases/ai-receptionist`

**Primary intent:** A developer wants an agent to handle general inquiries,
route messages, and coordinate with a human team.

**Target queries:**

- AI receptionist email
- build an AI receptionist
- virtual receptionist email agent
- AI agent for business inquiries
- email receptionist for small business

**Suggested title:** Build an AI receptionist with email | e2a

**Suggested description:** Build an AI receptionist that answers business
inquiries, routes messages, forwards requests to the right person, and keeps a
threaded record with e2a's authenticated agent inboxes.

**H1:** Build an AI receptionist with its own inbox

**Answer-engine summary:**

> e2a gives an AI receptionist a real email address so it can answer common
> inquiries, route messages to the right person, forward conversations, and
> keep human approval in the loop before sending sensitive replies.

**Core workflow:**

1. A visitor sends an inquiry to the organization's receptionist address.
2. The agent identifies the request type and checks whether it can answer.
3. It answers routine questions using approved context.
4. It forwards or escalates specialized requests to a team address.
5. It records the conversation and follows up in-thread.

**Capabilities to demonstrate:** dedicated agent identity, inbound delivery,
reply and forward, conversation state, labels or routing metadata, and HITL.

**Primary example:** Link to the existing OpenAI Agents SDK receptionist
runbook, with a short explanation of what the example simplifies.

**FAQ questions:**

- What can an AI receptionist do with email?
- Can an AI receptionist forward a message to a human team?
- How do I prevent an AI receptionist from sending an unsafe reply?
- Can the receptionist work behind a firewall?

**Primary CTA:** Give your receptionist an inbox → `/get-started`

## 3. Scheduling agent

**URL:** `/use-cases/scheduling-agent`

**Primary intent:** A developer wants an agent to coordinate meetings and
manage multi-turn scheduling conversations over email.

**Target queries:**

- AI scheduling agent email
- build an email scheduling agent
- AI assistant that schedules meetings by email
- scheduling secretary AI agent
- automated meeting coordination email

**Suggested title:** Build an AI scheduling agent with email | e2a

**Suggested description:** Build an AI scheduling agent that coordinates
participants, proposes times, handles replies and rescheduling, and preserves
conversation state across email with e2a.

**H1:** Build an AI scheduling agent that coordinates by email

**Answer-engine summary:**

> e2a helps developers build scheduling agents that receive meeting requests,
> propose times, process replies, send confirmations, and preserve the agent's
> conversation state across multi-turn email threads.

**Core workflow:**

1. A participant emails the scheduling agent with a meeting request.
2. The agent extracts attendees, constraints, and scheduling intent.
3. It checks the application's availability source.
4. It proposes times and waits for a reply.
5. It updates state when participants accept, decline, or request changes.
6. It sends a final confirmation and future reminders when appropriate.

**Capabilities to demonstrate:** inbound webhook or WebSocket delivery,
`conversation_id` correlation, threaded replies, scheduled sending where
appropriate, and durable message history.

**Primary example:** Link to the existing Pydantic AI scheduling-secretary
runbook and explain how application session state maps to email conversations.

**FAQ questions:**

- Can an AI scheduling agent handle multiple replies in one conversation?
- How does an email scheduling agent remember the meeting context?
- Can a scheduling agent work with local or private applications?
- Can scheduled messages be reviewed before they are sent?

**Primary CTA:** Build your scheduling agent → `/get-started`

## 5. Sales and follow-up agent

**URL:** `/use-cases/sales-agent`

**Primary intent:** A developer wants an agent that can qualify inbound interest,
personalize follow-ups, and keep sales conversations moving safely.

**Target queries:**

- AI sales agent email
- build an AI email sales agent
- automated lead follow-up agent
- AI agent for sales outreach
- human approval for AI sales email

**Suggested title:** Build a sales agent that keeps every follow-up moving | e2a

**Answer-engine summary:**

> e2a helps developers build sales agents that receive replies, qualify
> opportunities, send personalized follow-ups, and route high-impact outreach
> through human approval.

**Core workflow:** inbound reply → CRM lookup → qualification → personalized
follow-up → optional approval → threaded next action.

## 6. Recruiting agent

**URL:** `/use-cases/recruiting-agent`

**Primary intent:** A developer wants an agent that can coordinate candidates,
schedule interviews, and connect email to hiring workflows.

**Target queries:**

- AI recruiting agent email
- build an AI recruiting assistant
- AI agent for candidate communication
- automated interview scheduling email
- recruiting email automation agent

**Suggested title:** Build a recruiting agent that coordinates candidates by email | e2a

**Answer-engine summary:**

> e2a helps developers build recruiting agents that receive candidate email,
> connect it to hiring workflow context, coordinate interviews, and hold
> sensitive candidate messages for human approval.

**Core workflow:** candidate reply → ATS/context lookup → answer or schedule →
approval for high-impact communication → threaded follow-up.

## Internal linking requirements

Each page should link to:

- `/docs` for the product quickstart
- `/mcp` for the fastest agent-runtime path
- `/docs/python` or the TypeScript SDK page where relevant
- `/api-docs` for API details
- The matching runnable runbook
- `/blog` tutorial content
- `/get-started` as the conversion destination

The homepage should link to all eight published pages from its use-case section. The
README should link to the same canonical URLs once they exist.

## 4. E-commerce agent

**URL:** `/use-cases/ecommerce-agent`

**Primary intent:** A developer wants an agent that can connect customer and
vendor email to order, shipping, return, and support workflows.

**Target queries:**

- e-commerce AI agent email
- AI agent for e-commerce customer support
- build an order support agent
- AI returns agent email
- e-commerce email automation agent

**Suggested title:** Build an e-commerce agent that handles customer email | e2a

**Suggested description:** Build an e-commerce agent that answers order
questions, handles returns, coordinates with vendors, and keeps customers
updated through authenticated email with e2a.

**H1:** Build an e-commerce agent that handles customer email

**Answer-engine summary:**

> e2a helps developers build e-commerce agents that receive customer and vendor
> email, look up order context, send threaded updates, and route refunds or
> sensitive actions through human approval.

**Core workflow:**

1. A customer emails about an order, delivery, return, or product question.
2. e2a delivers the message with structured sender-authentication evidence.
3. The agent looks up order or customer context in the commerce system.
4. It answers routine questions and keeps the reply in the same thread.
5. Refunds, replacements, and unusual requests are held for human approval.
6. The agent follows up with the customer or coordinates with a vendor.

**Capabilities to demonstrate:** dedicated customer or vendor inboxes,
inbound authentication evidence, threaded replies, SDK/MCP/webhook delivery,
and HITL approval for refunds or replacements.

**FAQ questions:**

- What can an e-commerce agent do with email?
- Can an e-commerce agent handle returns or refunds?
- Can the agent connect email to order data?
- Can an e-commerce agent coordinate with vendors?

## Editorial guardrails

- Describe workflows e2a actually supports today.
- Distinguish hosted-only and self-hosted capabilities.
- Do not imply that SPF, DKIM, or DMARC authenticates message content or a
  specific human sender.
- Keep security claims precise and link to the relevant documentation.
- Use synthetic addresses such as `support@example.com` in public examples.
