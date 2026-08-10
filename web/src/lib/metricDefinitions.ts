// Plain-language definitions for every metric shown on /metrics.
//
// These are deliberately paraphrases of the `doc:` strings on the Go view
// structs (internal/httpapi/metrics.go), not new prose. The API, the CLI, the
// MCP tool descriptions, and this page must agree about what a number means —
// if a definition changes on the server, it changes here in the same PR.
//
// Every rate definition NAMES ITS DENOMINATOR, because delivered_rate is over
// accepted while bounce_rate is over submitted. Two numbers on one page that
// don't reconcile is the fastest way to lose trust in all of them, and the
// denominators are the reconciliation.

export const METRIC_HELP = {
  // ── Rates ──────────────────────────────────────────────
  deliveredRate:
    "Of everything your agents asked to send OUTWARD, the share a recipient server accepted. Mail stopped by your own review or suppression list still counts against it — this is the honest 'did my mail arrive' number. Mail that actually went agent-to-agent is excluded — it never reaches a recipient server, so it can neither succeed nor fail on this measure. A send stopped before it went anywhere (review-rejected, cancelled) stays in the denominator whether its recipient was local or remote, exactly as a rejected external send does.",
  bounceRate:
    "Bounced mail as a share of what was actually submitted to a provider — not of everything accepted, and not counting agent-to-agent mail. That denominator is deliberate: it matches the basis mailbox providers use for the thresholds that can put a sending account under review.",
  complaintRate:
    "Recipients who marked a message as spam, as a share of delivered mail. Mailbox providers compute spam rate over delivered mail, so this denominator matches theirs. This is the number most likely to get sending paused, so the threshold is far lower than it looks.",
  suppressionBlockRate:
    "The share of requested sends that never left e2a because the recipient was on a suppression list. These produce no bounce and no reply, so nothing else on this page will flag them — watch this for silent losses.",

  // ── Funnel stages ──────────────────────────────────────
  accepted:
    "Outbound messages e2a accepted from the send API. Counts sends only — the arriving copy of agent-to-agent mail is counted under Received instead.",
  submitted:
    "Messages an upstream provider accepted for delivery. The gap between Accepted and Submitted is mail your own policy stopped: held for review, blocked by suppression, or failed before send.",
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
