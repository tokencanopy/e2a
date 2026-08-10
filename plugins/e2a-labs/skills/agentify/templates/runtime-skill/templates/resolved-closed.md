# resolved-closed template

The honest "we decided not to" — sent into the filer thread when a ticket
reaches `closed_wontfix` (a human/approver declined). Without this, the last
word after the ack is silence, which turns the ack into a broken promise.

---
Subject: (auto `Re:` from the thread)

An update on the feedback you sent: we've decided not to take this one
forward. {{the reason, slot-filled from the maintainer's decline/wontfix
note — plain and respectful}}.

{{If there is a workaround, one line: "In the meantime, {{workaround}}."}}

Thanks for taking the time to flag it.

— {{product_name}} support (an assistant; a human made this call)
Reply "stop" to mute updates.
---

Notes for the agent:
- The reason is the maintainer's words, lightly cleaned — do not invent
  rationale or argue the product's position.
- If the filer replies with a compelling case, that's an escalation: leave it
  for a human, don't re-litigate.
