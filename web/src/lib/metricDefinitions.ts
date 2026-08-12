// Plain-language definitions for every metric shown on /metrics.
//
// These are deliberately paraphrases of the `doc:` strings on the Go view
// structs (internal/httpapi/metrics.go), not new prose. The API, the CLI, the
// MCP tool descriptions, and this page must agree about what a number means —
// if a definition changes on the server, it changes here in the same PR.
//
// Every rate definition NAMES ITS DENOMINATOR, because delivered_rate is over
// accepted minus loopback while bounce_rate is over submitted minus loopback.
// Two numbers on one page that don't reconcile is the fastest way to lose
// trust in all of them, and the denominators are the reconciliation.

export const METRIC_HELP = {
  // ── Rates ──────────────────────────────────────────────
  deliveredRate:
    "Delivered divided by Accepted minus Loopback. This measures outward sends only; local agent-to-agent delivery is excluded because it reaches no recipient server.",
  bounceRate:
    "Hard, soft, and undetermined bounces divided by Submitted minus Loopback. This measures only mail actually submitted to an upstream provider.",
  complaintRate:
    "Complaints divided by Delivered. Local agent-to-agent delivery is excluded because it cannot produce provider complaint feedback.",
  suppressionBlockRate:
    "Suppressed sends divided by Accepted minus Loopback. This is the share of requested outward sends that never left e2a because the recipient was suppressed.",

  // ── Funnel stages ──────────────────────────────────────
  accepted:
    "Messages e2a accepted from your agents. External-delivery rates use Accepted minus Loopback, so local agent-to-agent delivery never counts as an external success or failure.",
  submitted:
    "Messages an upstream provider or the local loopback path accepted for delivery. Provider-outcome rates use Submitted minus Loopback; the remaining gap from Accepted is mail stopped before submission.",
  delivered:
    "Messages a recipient's mail server accepted. This is server acceptance, not inbox placement — no email provider can tell you whether a message landed in the inbox or in spam.",
  bounced:
    "Mail the recipient's server rejected. Hard bounces are permanent (bad address) and add the recipient to your suppression list; soft bounces are temporary (full mailbox, throttling); undetermined is kept separate rather than folded into soft, which would understate hard-bounce risk.",
  complained:
    "Recipients who reported a message as spam. Each one is a strong negative signal to the mailbox provider, and they add up faster than volume does.",
  suppressed:
    "Sends blocked before submission because the recipient was on a suppression list. The agent believes it sent; nothing left the building.",
  sendFailed:
    "Messages that reached a terminal failure at submission: a provider rejection, exhausted local retries, or a policy cancellation. These have three different owners — expand the reason-code detail to tell them apart.",

  loopback:
    "Messages delivered agent-to-agent without ever leaving this deployment. They are excluded from every rate above: there is no recipient server to accept them, so counting them as delivered would overstate delivery while counting them as failures would understate it. This counts mail that reached local delivery — a self-send stopped earlier by review is not counted here.",

  // ── Inbound ────────────────────────────────────────────
  received:
    "Inbound messages accepted: arrivals over SMTP plus the delivered copy of agent-to-agent mail that never left this deployment.",
  dmarc:
    "How inbound senders authenticated. Pass means an aligned DMARC pass — the sending domain is verified. Fail is an actual mismatch. None means the sender publishes no DMARC policy at all, which is common and NOT itself suspicious. Error means the check could not be completed.",

  // ── Review ─────────────────────────────────────────────
  review:
    "Human-in-the-loop review outcomes. Expired means a hold aged out without anyone deciding — that is lost work, not an approval, and a rising count means nobody is working the queue.",

  // ── Webhooks ───────────────────────────────────────────
  webhookSuccess:
    "The share of webhook deliveries your endpoints accepted. This is a different question from email delivery: it asks whether YOUR CODE received the event, not whether the mail reached a recipient. Deliveries still retrying are excluded from the denominator, because a delivery mid-retry has not failed yet.",
  webhookGrain:
    "One row per event per subscriber. An account with three webhooks matching the same event produces three delivery rows for one message, so these counts legitimately exceed message counts — it does not mean mail was duplicated.",
  webhookFailures:
    "Two kinds, and they have different owners. 'Endpoint answered' means your endpoint returned a non-2xx — a constant 401 or 405 usually names the fix exactly. 'No response' means nothing ever answered: a connect, DNS or TLS failure, a blocked URL, or a delivery that expired while waiting. That second bucket is mostly an unreachable endpoint but can include rare e2a-side failures, so it is not a clean fault split.",
  webhookAutoDisabled:
    "e2a stops delivering to an endpoint that keeps failing. While it is disabled, events are DROPPED rather than retried — this is the most urgent state on this page. Re-enable the webhook once the endpoint is healthy.",
  webhookRetention:
    "Delivery history is kept for 30 days, unlike message history. A window reaching further back reports on rows that have already been pruned, so a drop-off there is a retention boundary rather than a fall in webhook volume.",

  // ── Meta ───────────────────────────────────────────────
  window:
    "Messages belong to this window by their own send time, not by when each observation landed — otherwise a rate's numerator and denominator would describe different populations. The cost is that recent days keep moving: bounce and complaint feedback arrives for up to 72 hours.",
  coverage:
    "How many messages in this window carry a lifecycle record. When it is lower than the total, part of the window predates the lifecycle ledger and every counter below undercounts by that difference. This is a recording gap, not lost mail.",
  observations:
    "Ledger rows. Retryable steps are recorded once per attempt, so this exceeds the message count whenever the pipeline retried. Every headline number on this page uses the message count instead, so a single retried message can never inflate a stage above the one that feeds it.",
} as const;

export type MetricHelpKey = keyof typeof METRIC_HELP;
