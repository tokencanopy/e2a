# e2a discoverability release checklist

Use this checklist when the website and package metadata changes are approved
for outward release. The public GitHub repository boundary still applies: use
only synthetic examples and public product metadata.

## Before merge

- [ ] Review the eight use-case pages:
  - [ ] Support
  - [ ] AI receptionist
  - [ ] Scheduling
  - [ ] E-commerce
  - [ ] Sales
  - [ ] Recruiting
  - [ ] Voice follow-up
  - [ ] Procurement
- [ ] Review the `/use-cases` hub and homepage links.
- [ ] Review the six workflow tutorials in `/blog`.
- [ ] Confirm every page has a canonical URL, title, description, FAQ/HowTo
      data where appropriate, and a working CTA.
- [ ] Confirm `sitemap.xml`, `robots.txt`, `llms.txt`, and `llms-full.txt` are
      generated and current.
- [ ] Run `npm run build` in `web/`.
- [ ] Run `node scripts/validate-plugin.mjs`.
- [ ] Run `node scripts/sync-agent-docs.mjs --check`.
- [ ] Run `git diff --check`.

## Website release

- [ ] Deploy the web app through the normal hosted release path.
- [ ] Verify these public URLs return 200 after deployment:
  - [ ] `https://e2a.dev/use-cases`
  - [ ] `https://e2a.dev/use-cases/support-agent`
  - [ ] `https://e2a.dev/use-cases/ai-receptionist`
  - [ ] `https://e2a.dev/use-cases/scheduling-agent`
  - [ ] `https://e2a.dev/use-cases/ecommerce-agent`
  - [ ] `https://e2a.dev/use-cases/sales-agent`
  - [ ] `https://e2a.dev/use-cases/recruiting-agent`
  - [ ] `https://e2a.dev/use-cases/voice-agent`
  - [ ] `https://e2a.dev/use-cases/procurement-agent`
- [ ] Verify `https://e2a.dev/sitemap.xml` includes the new routes.
- [ ] Verify `https://e2a.dev/llms.txt` includes the use cases and tutorials.
- [ ] Submit the sitemap or request indexing in Google Search Console.

## Public source and package surfaces

- [ ] Merge the README use-case links.
- [ ] Publish updated plugin documentation/manifests through the normal release
      process.
- [ ] Publish the updated TypeScript SDK metadata with its next approved SDK
      release.
- [ ] Publish the updated Python SDK metadata with its next approved SDK
      release.
- [ ] Publish the updated MCP package metadata only if an MCP package release
      is still part of the supported distribution path.
- [ ] Update external directories only with the canonical description and
      public use-case URLs.

## Distribution copy

Use this short description consistently:

> e2a is an open-source, authenticated email gateway for AI agents. It gives
> each agent a real inbox, verifies inbound sender identity, and supports MCP,
> SDKs, webhooks, WebSockets, REST, and HTTP outbound email.

Use this use-case sentence when a directory allows more detail:

> Developers use e2a to build support, receptionist, scheduling, e-commerce,
> sales, recruiting, voice follow-up, and procurement agents that receive email,
> take action, and reply in persistent threads with optional human approval.

## After release

- [ ] Capture the first Search Console baseline using
      `docs/discoverability-measurement.md`.
- [ ] Run the ten AI-answer evaluation prompts in a fresh conversation.
- [ ] Record which assistants mention e2a, which pages they cite, and any
      inaccurate capability claims.
- [ ] Share the use-case hub and one relevant tutorial in approved channels.
- [ ] Add UTM parameters using the convention in the measurement document.
- [ ] Recheck indexing and answer quality after 28 days.

## Approval boundary

Merging code and preparing drafts are local/reversible. Deployment, package
publishing, external directory edits, public announcements, and paid promotion
are outward-facing actions and require Josh's approval before execution.
