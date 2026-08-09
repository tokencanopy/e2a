# triage-ack template

The first email to a filer — it acks AND informs in one message (no separate
"received" email). Sent by `comms_send.sh reply` into the filer's thread, so
it also becomes the reply channel. Slot-fill the bracketed parts; keep the
framing fixed. Promise discipline: commit only to a status-change
notification, never to shipping.

---
Subject: (auto `Re:` from the thread)

Thanks for the feedback — {{one line: what you did with it}}.

{{Pick the outcome line:
- tracked-as-new: "We're tracking this as {{kind}} and will look into it."
- duplicate: "This is the same as something we're already tracking, and
  we've added your details to it."
- question: "{{a direct, helpful answer to the question}}"}}

We'll email you on this thread when its status changes. You can just reply
here if you have more to add.

— {{product_name}} support (an assistant; a human reviews anything that ships)
Reply "stop" to mute updates.
---

Notes for the agent:
- State plainly why they're getting this (they filed feedback to
  {{product_name}}). Never promise a fix or a date — "status changes" only.
- Never put the filer's email address or any other ticket's content in here.
- For a question, the answer is the one place free prose is allowed; keep it
  factual and short.
