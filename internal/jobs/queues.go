package jobs

import "github.com/riverqueue/river"

// Named queues. Jobs are assigned a queue at enqueue time (InsertOpts.Queue);
// the shared client works all of these. Separate queues give INDEPENDENT
// concurrency per lane, so a backlog in one (e.g. a slow customer webhook
// endpoint) can never starve another (e.g. outbound sends) — the isolation the
// hand-rolled queues couldn't express cleanly. Keep these names stable: they are
// persisted on river_job rows.
const (
	// QueueOutbound carries outbound send jobs (API → SES).
	QueueOutbound = "outbound"
	// QueueInbound carries inbound message-processing jobs (accepted raw MIME →
	// parse/screen/persist/deliver). Isolated so an inbound spike (incl. the
	// per-message Gemini screen) can't starve outbound sends or webhook delivery.
	QueueInbound = "inbound"
	// QueueWebhook carries customer webhook-delivery jobs. Isolated from outbound
	// so a slow/failing endpoint's backlog never delays sends.
	QueueWebhook = "webhook"
	// QueueMaintenance carries low-urgency periodic/janitor work (reapers,
	// hold-TTL resolution, auto-disable sweeps).
	QueueMaintenance = "maintenance"
	// QueueNotify carries HITL approval-notification emails (the owner's review
	// alert when an outbound message enters pending_review). Small, isolated pool
	// so a burst of held sends notifying — or a stuck notification to a bad owner
	// address retrying — never competes with customer outbound delivery.
	QueueNotify = "notify"
	// QueueSenderIdentityV2 is versioned as well as its job kinds. During a
	// blue/green rollout the old binary does not listen to this queue, so it
	// cannot claim and error a new desired-state job as an unknown kind.
	QueueSenderIdentityV2 = "sender_identity_v2"
	// QueueDefault is River's built-in default — anything not explicitly routed.
	QueueDefault = river.QueueDefault
)

// defaultQueueConfig is the concurrency map the shared client works. Per-lane
// MaxWorkers is deliberately generous for I/O-bound work (SMTP / HTTP); tune
// against real throughput. Every queue a domain enqueues into MUST appear here or
// its jobs sit unworked.
func defaultQueueConfig(cfg Config) map[string]river.QueueConfig {
	return map[string]river.QueueConfig{
		QueueOutbound:         {MaxWorkers: cfg.OutboundWorkers},
		QueueInbound:          {MaxWorkers: cfg.InboundWorkers},
		QueueWebhook:          {MaxWorkers: cfg.WebhookWorkers},
		QueueMaintenance:      {MaxWorkers: cfg.MaintenanceWorkers},
		QueueNotify:           {MaxWorkers: cfg.NotifyWorkers},
		QueueSenderIdentityV2: {MaxWorkers: cfg.SenderIdentityWorkers},
		QueueDefault:          {MaxWorkers: cfg.DefaultWorkers},
	}
}
