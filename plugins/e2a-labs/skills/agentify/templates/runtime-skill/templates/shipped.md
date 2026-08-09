# shipped template (slice 3 — fix + release lanes)

Sent into the filer thread when a ticket reaches `shipped` (the fix PR
merged/released). The "here's how it works" prose is NOT free-form: it is the
`customer-note` block from the fix PR's description, approved as part of
normal PR review, and slotted here verbatim — the one place the agent
describes product behavior to a customer is always human-reviewed.

---
Subject: (auto `Re:` from the thread)

Good news — the thing you flagged shipped in our latest release.

{{customer-note: the verbatim block from the merged fix PR's description}}

Thanks for the report; it made the product better.

— {{product_name}} support (an assistant; a human reviewed what shipped)
Reply "stop" to mute updates.
---

Notes for the agent:
- Do NOT write the behavior description yourself — slot the PR's
  `customer-note` verbatim. If the PR has no `customer-note`, leave the
  ticket for a human rather than improvising product claims.
- Promise discipline holds: this fires at release, on a real status change.
