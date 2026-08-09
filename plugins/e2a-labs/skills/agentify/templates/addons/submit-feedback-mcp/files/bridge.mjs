// bridge.mjs — pure logic for the submit_feedback email-bridge.
//
// submit_feedback does NOT create a ticket: it drops a structured feedback
// email into the SAME support mailbox the triage lane already drains, so
// there is one intake path and zero triage-lane changes. This module holds
// the parts worth unit-testing (validation, email composition, status
// derivation); server.mjs wires them to the MCP runtime + the e2a REST API.

export const KINDS = ['bug', 'feature', 'other'];
export const LIMITS = { title: 200, body: 20000 };

// validateFeedback: returns { ok: true } or { ok: false, error: 'INVALID_FEEDBACK: ...' }.
// Machine-branchable error prefix (house convention). Validate-before-charge:
// the caller checks this BEFORE consuming a rate-limit slot.
export function validateFeedback({ kind, title, body } = {}) {
  if (!KINDS.includes(kind)) {
    return { ok: false, error: `INVALID_FEEDBACK: kind must be one of ${KINDS.join(', ')}` };
  }
  if (typeof title !== 'string' || !title.trim() || title.length > LIMITS.title) {
    return { ok: false, error: `INVALID_FEEDBACK: title must be 1-${LIMITS.title} chars` };
  }
  if (typeof body !== 'string' || !body.trim() || body.length > LIMITS.body) {
    return { ok: false, error: `INVALID_FEEDBACK: body must be 1-${LIMITS.body} chars` };
  }
  return { ok: true };
}

// composeFeedbackEmail: the structured email dropped into the support mailbox.
// The body is untrusted text — it is sent as DATA; the triage lane fences it
// (the bridge never interprets it). NEVER include a caller-supplied contact
// address here (spoof/spam vector): replies route to the bridge's mailbox and
// the filer reads progress via feedback_status.
export function composeFeedbackEmail({ kind, title, body }) {
  // Strip CR/LF/control chars from the title before it goes in the SUBJECT —
  // defense-in-depth against header injection if a downstream mailer splats
  // the subject into a MIME header. The body stays raw (it is the email body,
  // not a header) and is opaque data the triage lane fences.
  const cleanTitle = String(title).replace(/[\r\n\t\x00-\x1f]+/g, ' ').trim();
  const subject = `[feedback:${kind}] ${cleanTitle}`.slice(0, 240);
  const text = `kind: ${kind}\n\n${body}`;
  return { subject, text };
}

// isValidFeedbackId: a feedback id is an e2a conversation id (conv_<...>).
// feedback_status MUST check this before building the REST path —
// encodeURIComponent leaves `.`/`..` intact, and the URL parser would
// normalize dot-segment ids onto unintended (same-host) endpoints.
export function isValidFeedbackId(id) {
  return typeof id === 'string' && /^conv_[A-Za-z0-9_-]+$/.test(id);
}

// statusFromThread: derive a coarse, HONEST status from the e2a thread the
// bridge owns (zero-backend: precise lifecycle lives in the GitHub ticket-card,
// not here). `messages` is the conversation, chronological. Status is
// "received" until support replies, then "answered" — the agent reads the
// thread for detail.
export function statusFromThread(messages = []) {
  const inbound = messages.filter((m) => m.direction === 'inbound'); // FROM support, TO the bridge
  const replies = inbound.length;
  const last = messages.length ? messages[messages.length - 1] : null;
  return {
    status: replies > 0 ? 'answered' : 'received',
    replies,
    last_update: last ? last.received_at || last.created_at || null : null,
  };
}
