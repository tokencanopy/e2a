# e2a Core and Labs Plugin Design

**Status:** Approved design; pending written-spec review

**Date:** 2026-08-08

## Summary

Split the current e2a plugin into a stable, developer-facing core plugin and an
explicitly experimental Labs plugin. The core plugin will make setup,
application integration, diagnosis, and everyday email operation easy to
discover and complete. The Labs plugin will contain the more opinionated
autonomous-agent workflows: Agentify, Autopilot, and Tether.

The core plugin will expose four focused skills:

- `e2a` for everyday inbox and email operations.
- `e2a-setup` for MCP connection, OAuth authorization, inbox readiness, and
  optional custom-domain setup.
- `e2a-integrate` for adding e2a to an application through an official SDK or
  the REST/OpenAPI surface.
- `e2a-doctor` for MCP-first diagnosis and confirmed guided repairs.

## Problem

The existing plugin combines the stable e2a operating model with three large,
experimental workflows. Agentify, Autopilot, and Tether demonstrate useful
agent-native patterns, but they are more complex, opinionated, and
security-sensitive than the everyday jobs most developers expect from an email
integration plugin. Their presence makes the core package harder to understand
and makes experimental behavior appear equally stable with basic e2a usage.

At the same time, the core plugin does not give the most common developer jobs
their own obvious entry points. A user should not need to navigate one long
operating manual to connect MCP, add e2a to a codebase, or diagnose a failed
delivery.

## Goals

- Give each common developer journey one recognizable skill and completion
  contract.
- Make MCP connection and OAuth authorization part of a guided setup flow.
- Make application integration language-aware without pretending every
  language has an official SDK.
- Make diagnosis available through the plugin's existing MCP authorization,
  without requiring the CLI or a second credential.
- Offer guided repairs while requiring confirmation for every state-changing
  repair.
- Keep experimental autonomous workflows available without presenting them as
  part of the stable core.
- Validate packaging, skill routing, safety invariants, and documentation drift
  in CI.

## Non-goals

- Adding a backend API or a new MCP tool.
- Purchasing or transferring domains.
- Making silent changes to MCP client configuration, DNS, inboxes, webhooks, or
  protection policy.
- Sending real email without an explicitly selected recipient.
- Building an exhaustive template library for every application framework.
- Creating unofficial SDKs for languages without an official e2a SDK.
- Requiring the e2a CLI for plugin setup or diagnosis.
- Delivering skills to Cursor through a mechanism Cursor does not support.
- Expanding Agentify, Autopilot, or Tether while moving them to Labs.

## Package architecture

### Stable core: `plugins/e2a/`

The core plugin owns:

- The hosted e2a MCP server configuration.
- Canonical setup, authentication, SDK, webhook, and operating documentation.
- The stable `e2a`, `e2a-setup`, `e2a-integrate`, and `e2a-doctor` skills.
- The stable plugin manifests and user-facing identity.

It contains no autonomous workflow scaffold or experimental runtime.

### Experimental package: `plugins/e2a-labs/`

Labs owns:

- Agentify and all of its templates, scripts, fixtures, and references.
- Autopilot and its runtime and tests.
- Tether and its runtime and tests.
- A Labs-specific README, Claude and Codex manifests, icon assets, validation,
  and version.

Labs does not duplicate the hosted e2a MCP configuration. It declares a core
e2a dependency where the client supports plugin dependencies. On clients that
do not enforce dependencies, its documentation and skills perform a clear
prerequisite check and point users to the core plugin. Installing Labs alone
must not create a second MCP server registration or silently edit client
configuration.

### Marketplaces and versions

The Claude and Codex marketplace manifests list both packages. Core is the
default recommendation; Labs is explicitly labeled experimental and must be
installed deliberately. The Cursor marketplace continues to list only core:
Cursor does not currently receive these plugin skills, and a skill-only Labs
entry would advertise a package it cannot use. If Cursor later gains a
supported skill-delivery path, Labs can add a Cursor surface then.

- Core advances from `0.6.0` to `0.7.0`.
- Labs begins at `0.1.0`.
- Release notes explain that Agentify, Autopilot, and Tether moved and provide
  the relevant Labs installation command.
- Core and Labs versions advance independently after the split.

The packaging change also corrects the stale MCP tool-count descriptions,
removes the Claude-unsupported `icon` field, and adds official strict Claude
manifest validation.

## Skill responsibilities and routing

### `e2a`: operate email

Use for reading, sending, replying, forwarding, attachments, contacts,
outreach, templates, and scheduled mail. It assumes e2a is connected. It does
not own initial connection, application code changes, or diagnosis.

Completion means the requested operation succeeded or the skill clearly
reported its durable state: sent, accepted, scheduled, pending review, or
failed. It retains the current load-bearing rules around threading,
`conversation_id`, async acceptance, and review holds.

### `e2a-setup`: establish service readiness

Use when a user wants to install or authorize the e2a MCP connection, select or
create an inbox, or configure a custom domain. It changes no application code.

Completion means:

- The e2a MCP server is registered in the current client.
- OAuth authorization succeeds.
- Credential scope and the selected inbox are understood.
- A harmless inbox read succeeds.
- Requested custom-domain work is complete or has a precise pending/manual
  action.
- The user receives a concise readiness summary.

The skill cannot bootstrap the plugin that contains it. Marketplace
installation remains the entry point. Once the plugin is present, Setup takes
the user from "plugin installed" to "MCP authorized and inbox ready."

### `e2a-integrate`: change an application

Use when a developer asks to add e2a, email sending, an agent inbox, inbound
webhooks, or polling to a codebase. The skill is language-aware:

- TypeScript/JavaScript and Python use the official e2a SDKs.
- Other server-side languages use idiomatic HTTP against the REST/OpenAPI
  contract.
- Browser-side secrets and invented unofficial SDKs are unsupported.

Completion means the requested application integration, configuration
documentation, security checks, and relevant tests are present and passing.

### `e2a-doctor`: diagnose and repair

Use when an existing connection, inbox, domain, webhook, or message flow is not
working. Doctor is MCP-first and read-only during diagnosis. It uses the e2a
CLI only when the user is specifically diagnosing CLI behavior, MCP is
unavailable but an authenticated CLI already exists, or a self-hosted
deployment needs local environment or SMTP visibility.

Completion means Doctor has produced evidence-backed ranked findings and
either verified the accepted repairs or left precise next actions.

### Trigger separation

Routing tests must distinguish at least these intents:

- "Set up e2a and give my agent an inbox" -> `e2a-setup`.
- "Add e2a webhooks to this application" -> `e2a-integrate`.
- "Reply to this email" -> `e2a`.
- "Why was this message not delivered?" -> `e2a-doctor`.
- "Make this repository autonomous" -> the Labs `agentify` skill.

The skills may link to one another, but each must be independently
understandable and must not require another skill's prose to operate safely.

## Setup flow

### MCP bootstrap

1. Inspect the current client's tool registry for e2a MCP tools.
2. If the server is absent, first check whether the plugin is disabled or the
   client needs a reload. Only when the plugin system has not registered the
   server should Setup offer the client's native method for registering
   `https://api.e2a.dev/mcp`; it must not create a duplicate registration.
3. Obtain one confirmation before changing client configuration.
4. Start the client's OAuth flow. Do not request an API key from an interactive
   user.
5. Explain any reload or restart required by the client.
6. Call e2a `whoami` to prove authorization.
7. Continue into inbox selection or creation.

Claude and Codex receive this skill through their plugin systems. Cursor and
other clients that do not receive plugin skills continue to use the equivalent
canonical setup documentation and ready-to-paste MCP configuration.

### Inbox selection and creation

1. Use the inbox bound to an agent-scoped credential.
2. For account scope, enumerate inboxes and honor an inbox already identified
   by the task.
3. Use the sole inbox when exactly one exists.
4. Ask the user to choose when several exist.
5. If none exists, offer an address on `agents.e2a.dev` and confirm the full
   address before creation.
6. Prove readiness with a harmless inbox listing.
7. Offer, but do not silently perform, a send test to a recipient selected by
   the user.

### Custom-domain branch

Setup explicitly asks whether the user wants the shared `agents.e2a.dev`
domain or a custom domain. The shared domain remains the recommended fast path.

For a custom domain:

1. Confirm that the user owns the domain and wants branded addresses.
2. Register the domain in e2a and retrieve the required records.
3. Ask which provider hosts authoritative DNS.
4. For Cloudflare, recommend or use the official Cloudflare API MCP server when
   available.
5. For GoDaddy, recommend the authenticated `gddy` agent skill or CLI. The
   official GoDaddy MCP server is read-only and must not be represented as able
   to modify DNS.
6. For other providers, present a clean, copyable record table.
7. Before a provider-assisted write, show the complete proposed DNS diff and
   obtain one confirmation for the whole record set.
8. Apply the records and request e2a verification.
9. If propagation is incomplete, explain the observed state and give a safe
   resume/retry path instead of blocking indefinitely.
10. Report inbound verification and outbound branded-sending readiness as
    separate capabilities.

## Integration flow

1. Determine whether the application needs outbound sending, inbound webhooks,
   polling, or a combination. Ask only when the requested behavior does not
   make the choice clear.
2. Inspect the repository's language, framework, package manager, environment
   conventions, module boundaries, and test runner.
3. Select an official SDK or the REST/OpenAPI path according to the support
   tiers above.
4. Add one application-owned e2a boundary rather than scattering raw SDK calls
   throughout the codebase.
5. Keep API credentials server-side and document required environment
   variables without creating production credentials.
6. For inbound webhooks, add signature verification before parsing or acting on
   the payload, typed event handling where the language permits it, and
   idempotent processing appropriate to the application's persistence model.
7. Add unit tests using synthetic addresses and payloads, then run the
   repository's relevant verification commands.
8. Offer a live smoke test separately. Do not insert real addresses,
   credentials, message contents, or production-derived identifiers into
   source or fixtures.

The skill adapts to the framework already present. It does not introduce a new
framework merely because a reference example uses one.

## Doctor flow

### Diagnostic hierarchy

1. Start from the user's symptom, inbox, domain, webhook, or message ID.
2. Use the existing e2a MCP authorization to inspect only relevant surfaces:
   `whoami`, inbox access, protection policy, suppressions, domain state,
   webhook configuration and delivery history, and message lifecycle.
3. Use read-only local DNS resolution when live public DNS evidence is needed.
4. Separate definite configuration failures from transient or asynchronous
   states such as queued delivery and DNS propagation.
5. Rank findings by likelihood and impact and attach concrete evidence.
6. Offer guided repairs after diagnosis.
7. Obtain confirmation for each state-changing repair, apply it through e2a or
   the relevant provider tool, and re-run the affected checks.

### CLI-assisted branch

If an authenticated e2a CLI is already the relevant surface, Doctor may run
`e2a doctor --json` and interpret its versioned `e2a.doctor/v1` report. It may
supplement that report with MCP-only checks such as a specific message
lifecycle. It never asks an OAuth-connected plugin user to install or configure
the CLI solely to run Doctor.

## Safety and failure behavior

- Never retry a send that returned `accepted`, `scheduled`, or
  `pending_review`.
- Never print, store, or commit credentials.
- Never put real customer or non-public production-derived data in public
  repository source, fixtures, logs, screenshots, commits, or reviews.
- Never change MCP configuration, DNS, an inbox, a webhook, or protection
  policy without the specified confirmation.
- Never claim a domain is ready from registration alone.
- Never claim a message was delivered from durable acceptance alone.
- Never send a real email without an explicitly selected recipient.
- Preserve existing configuration when a repair is declined or evidence is
  inconclusive.
- Use synthetic `.test`, `.invalid`, `example.com`, and fictional resource IDs
  in all examples and tests.

## Skill content architecture

Each skill keeps its primary `SKILL.md` focused on the decision flow and safety
rules. Detailed material lives in references loaded only when relevant:

- `e2a-setup/references/`: client bootstrap, inbox selection, custom-domain
  setup, and DNS-provider guidance.
- `e2a-integrate/references/`: integration modes, TypeScript/Python SDK
  recipes, generic REST/OpenAPI guidance, webhook verification, and test
  patterns.
- `e2a-doctor/references/`: MCP diagnostic matrix, reason-to-remediation map,
  local DNS checks, and repair rules.

The existing `e2a` skill is narrowed rather than duplicated. Shared canonical
facts belong in the core plugin documentation; references link to those facts
instead of copying volatile API counts or tool signatures.

## Labs release gate

Moving experimental skills does not make known defects acceptable. Labs cannot
be released until these review findings are resolved and covered by regression
tests:

- Agentify's unattended Claude workflows enforce a real capability boundary;
  an allow rule combined with `bypassPermissions` is not treated as an
  allowlist.
- Agentify conversation dedup matches the exact bot-authored footer or trusted
  ticket-card field, not an unanchored string inside quoted feedback.
- Agentify template rendering safely serializes YAML scalar values and rejects
  malformed generated configuration.
- Tether rejects timezone-less `--until` values and fails closed on malformed
  persisted expiries.

The move itself otherwise preserves the behavior and tests of Agentify,
Autopilot, and Tether.

## Verification strategy

### Packaging and manifests

- Run the repository validator and the official strict Claude plugin validator.
- Assert that core contains exactly the four stable skills.
- Assert that Labs contains exactly Agentify, Autopilot, and Tether.
- Assert that Labs does not define a duplicate e2a MCP server.
- Assert that all applicable marketplace sources, versions, descriptions, and
  dependency declarations agree, and that Cursor does not advertise the
  skill-only Labs package while it lacks a supported skill-delivery path.
- Derive or validate advertised MCP tool counts against
  `mcp/tool-names.v1.json`.

### Skill behavior

- Add trigger-routing fixtures for setup, integration, operation, diagnosis,
  and Labs requests.
- Add static invariant tests for confirmation boundaries, OAuth guidance,
  accepted-send behavior, secret handling, and synthetic examples.
- Add setup fixtures covering MCP absent, MCP unauthorized, agent scope,
  account scope, no inbox, multiple inboxes, shared domain, Cloudflare-assisted
  DNS, GoDaddy guidance, manual DNS, and propagation.
- Add integration guidance fixtures for TypeScript, Python, and generic REST
  repositories, including send-only and verified-webhook cases.
- Add Doctor fixtures for healthy, authentication, authorization, protection
  hold, suppression, custom-domain DNS, webhook delivery, and message-lifecycle
  failures.
- Prove that setup and Doctor do not mutate client configuration, DNS, inboxes,
  webhooks, or protection without confirmation.

### Existing Labs tests

- Move the current Agentify, Autopilot, and Tether tests with their skills.
- Update path-sensitive harnesses without weakening assertions.
- Add the adversarial permission, footer-spoofing, YAML-scalar, and expiry tests
  required by the Labs release gate.

## Rollout

Implement and review the design in three slices:

1. Create `e2a-labs`, move the experimental skills, resolve the Labs release
   gate, and add migration guidance.
2. Add the three new core skills and narrow the existing `e2a` skill.
3. Update all marketplace manifests, versions, documentation, routing
   fixtures, and strict validation gates.

Each slice must preserve the repository's public-data boundary, avoid unrelated
cleanup, and pass the relevant plugin and skill test suites before merge.
