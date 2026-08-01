# Security Policy

e2a is an authenticated email gateway. A vulnerability in this codebase
can affect every deployment that uses it — including the ability to
forge inbound authentication verdicts, bypass HITL approval gates, exfil
inbound mail, or spoof agent identity. We take that responsibility
seriously and ask that you report issues privately rather than in
public GitHub issues.

For what e2a stores, how long it lives, log handling, and the user/operator data-rights surface, see [docs/data-handling.md](docs/data-handling.md).

## Reporting a vulnerability

Email **security@tokencanopy.com** with:

- A description of the issue
- Steps to reproduce (or a proof-of-concept)
- The version / commit you observed it on (the git SHA, release tag, or
  the `VERSION` file)
- Any mitigations you suggest

You can also use GitHub's [private vulnerability reporting](https://github.com/tokencanopy/e2a/security/advisories/new)
to file a draft advisory directly against this repo.

We aim to:

- **Acknowledge receipt** within 3 business days
- **Provide a substantive response** (preliminary assessment + next steps)
  within 7 business days
- **Ship a fix** for confirmed high-severity issues within 30 days,
  faster when active exploitation is plausible

We will credit reporters in the release notes unless you ask to remain
anonymous. We don't currently run a paid bounty program.

## Supported versions

| Version | Status |
|---------|--------|
| `1.x` (current line; `/v1` GA as of 1.5.0) | ✅ Receives security fixes |
| `0.x` (pre-GA) | ❌ Please upgrade to the latest 1.x release |

The `/v1` API is generally available as of e2a **1.5.0**; earlier `v1.0.x`
application and cherry-pick tags predate the API freeze and do not establish
the stable `/v1` API baseline. The latest `1.x` release receives security
fixes. Fixes are delivered in the latest `1.x` release (we don't backport to
older `1.x` patch versions). Self-hosters running pinned versions should plan
to upgrade promptly when an advisory is published.

## Scope

In scope (please report):

- Authentication bypass — anything that lets an unauthenticated caller
  reach an authenticated endpoint, or that lets one user act as another
- Causing forged or misaligned mail to receive a trusted DMARC verdict
- Bypassing HITL approval (sending an outbound message that should
  have been held for review)
- SPF/DKIM/DMARC verification flaws (causing inbound mail to be marked
  verified when it shouldn't, or vice versa)
- SSRF in webhook delivery, OAuth callback handling, or any other
  outbound HTTP path
- Privilege escalation across users, agents, or domains
- SQL injection, command injection, path traversal
- Memory-safety issues in the Go server (e.g. in the SMTP parser)
- Cryptographic flaws (HMAC misuse, weak random, predictable IDs)

Out of scope:

- Deployments running with the example `change-me-in-production` HMAC
  secret (the server refuses to start with it only when `env=production`;
  outside production the example value is the shipped default, and such a
  deployment must never be exposed to the network)
- Lack of features (e.g. "no rate limit on X" — open an issue or PR)
- Vulnerabilities in dependencies that don't have a reachable code
  path through e2a (please file with the upstream project)
- Issues only reproducible against `agents.e2a.dev` infrastructure
  rather than the OSS code (those go to the hosted-product security
  team via the same email)
- Email deliverability complaints, spam filter behavior

## Disclosure timeline

Once a fix is merged and released, we will publish a GitHub Security
Advisory describing the issue, affected versions, and remediation. We
ask that reporters hold public disclosure until the advisory is
published or 90 days after first contact, whichever is sooner.

## Bug-bounty alternatives

We don't pay bounties yet. If you'd like recognition beyond credit in
the advisory — a public thanks on the website, a swag pack — say so in
your initial report and we'll do our best.
