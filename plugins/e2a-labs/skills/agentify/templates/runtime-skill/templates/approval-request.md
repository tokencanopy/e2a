# approval-request template (fix_gate hitl)

The maintainer-approval email — the human gate that decides whether the
coding agent drafts a PR. Sent by `comms_send.sh approval` to
`fix_gate.approver` (config) ONLY; the reply (approve/decline) is routed back
by the comms lane's inbound pass. This `to` is never an address from email
content.

---
Subject: [{{product_name}}] Approve a fix for issue #{{issue_number}}?

Issue #{{issue_number}}: {{issue_title}}
{{one-paragraph neutral summary of what the fix would do}}

{{If a sensitive surface forced this gate even under mode:auto, name it:
"Flagged sensitive: {{surface}} — extra care on review."}}

Reply **approve** to have me open a draft PR for review, or **decline
<reason>** to skip it. No reply = nothing happens.

{{issue_url}}

— {{product_name}} autonomous-repo
---

Notes for the agent:
- The summary is YOUR neutral description, not quoted filer prose.
- Only an unambiguous "approve" / "decline" in the reply acts; anything else
  stays pending.
- A decline reason is slotted verbatim into the `resolved-closed` filer email.
