# e2a discoverability measurement

This is the recurring scorecard for traditional search and AI-answer
discovery. It deliberately separates visibility from activation: being named by
an assistant is useful only if the answer is accurate and leads a developer to
an actionable page.

## North-star outcome

When a developer asks how to give an AI agent a real email inbox, or describes
one of e2a's target workflows, e2a should be discoverable, accurately described,
and linked to a relevant use-case or integration page.

## Search measurement

Use Google Search Console for the verified e2a.dev property. Review the last
28 days and compare against the previous 28 days.

### Query groups

**Category**

- email for AI agents
- authenticated email gateway for AI agents
- AI agent email API
- email infrastructure for AI agents
- give an AI agent an email address

**Integration**

- email MCP server
- Python email API for AI agents
- TypeScript email SDK for AI agents
- OpenClaw email integration
- Google ADK email agent

**Use case**

- AI support agent email
- AI receptionist email
- AI scheduling agent email
- e-commerce AI agent email
- AI sales agent email
- AI recruiting agent email

### Search KPIs

- Non-branded impressions by query group
- Non-branded clicks by query group
- Average position for the canonical category queries
- Click-through rate for use-case pages
- Landing-page sessions from organic search
- Organic visits that reach `/get-started`, `/docs`, or `/mcp`
- Signups or first-inbox creation from organic sessions

Do not optimize for impressions alone. A useful page should move a developer
from a query to a workflow page, then to a runnable setup.

## AI-answer measurement

Once per month, run the following prompts in at least three major AI assistants
with web access enabled. Use a fresh conversation for each prompt and record the
date, assistant, answer, cited sources, and whether the answer links to e2a.dev.

### Discovery prompts

1. “What are the best ways to give an AI agent its own email address?”
2. “What open-source email gateways are built for AI agents?”
3. “How can I add email to a local AI agent without exposing a public URL?”
4. “What is an email MCP server for AI agents?”

### Use-case prompts

5. “How would I build an AI support agent that receives and replies to email?”
6. “How would I build an AI receptionist that routes email to a human team?”
7. “How can an AI agent coordinate meetings over email?”
8. “How would I build an e-commerce agent for order questions and returns?”
9. “How can an AI sales agent send follow-ups while requiring approval?”
10. “How can an AI recruiting agent coordinate candidates by email safely?”

### Score each answer

Use a 0–2 score for each dimension:

| Dimension | 0 | 1 | 2 |
| --- | --- | --- | --- |
| Mentioned | Not mentioned | Mentioned as an aside | Recommended or clearly relevant |
| Accurate category | Wrong or vague | Partly right | Authenticated email gateway for AI agents |
| Capability accuracy | Materially wrong | Minor omission | Correctly describes inbox, delivery, and controls |
| Use-case fit | Wrong workflow | Generic fit | Links or points to the matching use-case page |
| Source quality | No source | Weak directory source | e2a.dev, GitHub, docs, or official registry |
| Actionability | No next step | Generic advice | Runnable setup or relevant docs link |

The maximum score is 12 per prompt. Track both the total and the individual
failure modes. A high mention rate with low accuracy is a content problem, not a
growth win.

## Attribution

Use a consistent UTM convention for links shared in external content:

```text
utm_source=<source>
utm_medium=<channel>
utm_campaign=discoverability-2026
utm_content=<page-or-post>
```

Examples:

- `utm_source=github&utm_medium=readme&utm_campaign=discoverability-2026&utm_content=use-cases`
- `utm_source=ai-directory&utm_medium=listing&utm_campaign=discoverability-2026&utm_content=ecommerce-agent`
- `utm_source=hn&utm_medium=community&utm_campaign=discoverability-2026&utm_content=build-sales-agent`

Never put customer, agent, or production-derived identifiers in URLs or public
analytics labels. Use synthetic content slugs only.

## Monthly review

1. Export Search Console query and page data.
2. Group queries into category, integration, and use case.
3. Run the ten AI-answer prompts and score them.
4. Inspect queries with impressions but low click-through rate.
5. Inspect pages with organic visits but weak signup or docs progression.
6. Fix the largest accuracy or conversion gap with one content change.
7. Record the before/after date and change in the growth log.

## First baseline

Capture the first baseline after the use-case pages and tutorials are deployed.
Do not compare pre-launch and post-launch performance as if they were identical
content sets. Label the deployment date and the set of URLs included in the
baseline.
