package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/mux"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/tokencanopy/e2a/internal/agent"
	"github.com/tokencanopy/e2a/internal/agentauth"
	"github.com/tokencanopy/e2a/internal/apiserver"
	"github.com/tokencanopy/e2a/internal/approvaltoken"
	"github.com/tokencanopy/e2a/internal/auth"
	"github.com/tokencanopy/e2a/internal/config"
	"github.com/tokencanopy/e2a/internal/contactdue"
	"github.com/tokencanopy/e2a/internal/delivery"
	"github.com/tokencanopy/e2a/internal/eventpayload"
	"github.com/tokencanopy/e2a/internal/hitlnotify"
	"github.com/tokencanopy/e2a/internal/hitlworker"
	"github.com/tokencanopy/e2a/internal/idempotency"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/inboundprocess"
	"github.com/tokencanopy/e2a/internal/janitor"
	"github.com/tokencanopy/e2a/internal/jobs"
	"github.com/tokencanopy/e2a/internal/limits"
	"github.com/tokencanopy/e2a/internal/oauth"
	"github.com/tokencanopy/e2a/internal/outbound"
	"github.com/tokencanopy/e2a/internal/outboundsend"
	"github.com/tokencanopy/e2a/internal/relay"
	"github.com/tokencanopy/e2a/internal/senderidentity"
	"github.com/tokencanopy/e2a/internal/sendramp"
	"github.com/tokencanopy/e2a/internal/sendrate"
	"github.com/tokencanopy/e2a/internal/telemetry"
	"github.com/tokencanopy/e2a/internal/unsubscribe"
	"github.com/tokencanopy/e2a/internal/usage"
	"github.com/tokencanopy/e2a/internal/webhook"
	"github.com/tokencanopy/e2a/internal/webhookdelivery"
	"github.com/tokencanopy/e2a/internal/webhooknotify"
	"github.com/tokencanopy/e2a/internal/webhookpub"
	"github.com/tokencanopy/e2a/internal/ws"
	"github.com/tokencanopy/e2a/migrations"
)

// senderIdentityEventFirer adapts the webhooks publisher to the
// senderidentity.EventFirer hook: it publishes domain.sending_verified /
// domain.sending_failed to the owning user's webhook subscribers when a
// domain's sending identity reaches a terminal state (decision 4 / Slice 4).
// deliveryEventFirer adapts the webhooks publisher to delivery.Firer: it
// publishes email.delivered/bounced/complained + domain.suppression_added to
// the owning user's subscribers (decision 9 / Slice 4b). The event id is
// derived deterministically from dedupKey so duplicate SNS notifications
// produce the same id — receivers dedup on it (at-least-once delivery).
func deliveryEventFirer(outbox webhookpub.Outbox) delivery.Firer {
	return func(ctx context.Context, tx pgx.Tx, e delivery.FiredEvent) error {
		return outbox.PublishTx(ctx, tx, webhookpub.Event{
			ID:             webhookpub.DeterministicEventID(e.DedupKey),
			Type:           e.Type,
			CreatedAt:      e.OccurredAt,
			UserID:         e.UserID,
			AgentID:        e.AgentID,
			ConversationID: e.ConversationID,
			MessageID:      e.MessageID,
			Data:           e.Data,
		})
	}
}

func senderIdentityEventFirer(pub webhookpub.Publisher) senderidentity.EventFirer {
	return func(ctx context.Context, domain, userID string, status senderidentity.Status, errMsg string) {
		// Canonical typed payloads (contract freeze PR-2) — golden-fixture-locked.
		if status == senderidentity.StatusFailed {
			pub.Publish(ctx, webhookpub.NewEvent(webhookpub.EventDomainSendingFailed, userID,
				eventpayload.DomainSendingFailedData{
					Domain:        domain,
					SendingStatus: string(status),
					Reason:        errMsg,
				}))
			return
		}
		pub.Publish(ctx, webhookpub.NewEvent(webhookpub.EventDomainSendingVerified, userID,
			eventpayload.DomainSendingVerifiedData{
				Domain:        domain,
				SendingStatus: string(status),
			}))
	}
}

func main() {
	// Load .env file if present (development convenience, not required)
	godotenv.Load()

	configPath := flag.String("config", "config.yaml", "path to config file")
	bootstrapEmail := flag.String("bootstrap-email", "", "create a user with this email and print a fresh API key, then exit (for self-host first-run)")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Trash retention (trash.retention_days / E2A_TRASH_RETENTION_DAYS,
	// default 30, validated ≥1 by config.Load). Applied before any store,
	// janitor, or worker is constructed — every consumer (the janitor's
	// purge SQL, PurgeDeletedAgents/PruneExpired) reads the package var at
	// query time, so a single startup assignment governs the deployment.
	identity.TrashRetention = time.Duration(cfg.Trash.RetentionDays) * 24 * time.Hour

	// Database
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, cfg.Database.URL)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		log.Fatalf("Failed to ping database: %v", err)
	}

	// Apply any pending schema migrations before anything else touches
	// the DB. E2A_MIGRATION_MODE controls behavior — auto (default)
	// applies pending; verify refuses to apply and errors if any are
	// pending; skip is a no-op for emergency surgery. See
	// internal/identity/migrate.go for the contract.
	migrationMode, err := identity.ParseMigrationMode(os.Getenv("E2A_MIGRATION_MODE"))
	if err != nil {
		log.Fatalf("Invalid E2A_MIGRATION_MODE: %v", err)
	}
	if err := identity.RunMigrations(ctx, pool, migrations.FS, migrationMode); err != nil {
		log.Fatalf("Schema migration failed: %v", err)
	}
	// River's own schema (the shared job queue, internal/jobs). Tracked in
	// River's river_migration table, separate from e2a's schema_migrations;
	// idempotent. Applied unconditionally so river_job exists for every domain
	// that registers on the shared client.
	if err := jobs.Migrate(ctx, pool); err != nil {
		log.Fatalf("River schema migration failed: %v", err)
	}

	// Bootstrap mode: create a user + API key and exit. Used by self-host
	// operators to get their first key without needing Google OAuth.
	if *bootstrapEmail != "" {
		store := identity.NewStore(pool)
		user, err := store.BootstrapUser(ctx, *bootstrapEmail)
		if err != nil {
			log.Fatalf("Failed to bootstrap user: %v", err)
		}
		key, err := store.CreateAPIKey(ctx, user.ID, "bootstrap", nil)
		if err != nil {
			log.Fatalf("Failed to create API key: %v", err)
		}
		fmt.Printf("User:    %s (id=%s)\nAPI key: %s\n", user.Email, user.ID, key.PlaintextKey)
		fmt.Fprintln(os.Stderr, "save the API key now — it will not be shown again")
		return
	}

	log.Println("Connected to database")
	log.Printf("Config: env=%s domain=%s", cfg.Env, cfg.SMTP.Domain)

	// Services
	store := identity.NewStore(pool)
	// Envelope-encrypt DKIM private keys at rest (#144 / M4). In production the
	// signing secret is enforced ≥32 bytes so the cipher is always configured and
	// the startup backfill encrypts any legacy plaintext keys; in a weak-secret
	// dev deploy we log and fall back to plaintext storage.
	if dkimCipher, derr := identity.NewDKIMCipher([]byte(cfg.Signing.HMACSecret)); derr != nil {
		log.Printf("[identity] DKIM key encryption-at-rest disabled: %v", derr)
	} else {
		store.SetDKIMCipher(dkimCipher)
		if n, berr := store.EncryptLegacyDKIMKeys(ctx); berr != nil {
			log.Fatalf("DKIM key encryption backfill failed: %v", berr)
		} else if n > 0 {
			log.Printf("[identity] encrypted %d legacy DKIM private key(s) at rest", n)
		}
	}
	if err := store.EnsureSharedDomain(ctx, cfg.SharedDomain); err != nil {
		log.Fatalf("Failed to seed shared domain row: %v", err)
	}
	// deliveryStore backs the legacy webhook_deliveries table. The
	// legacy per-agent push path (Deliverer/RetryWorker) is gone — push
	// now flows exclusively through the /v1/webhooks subscriber resource
	// — but the store is retained so the cleanup janitor can keep draining
	// any rows left over from before the cutover.
	deliveryStore := webhook.NewDeliveryStore(pool)

	// Webhooks-as-a-resource pipeline. Events flow through the outbox
	// (webhook_events) → drain → River delivery; the legacy in-process
	// publisher is retired.
	subscriberStore := webhook.NewSubscriberStore(pool)
	subscriberDeliverer := webhook.NewSubscriberDeliverer(cfg.IsProduction(), cfg.Webhook.InternalSinkURL)

	// The transactional outbox is now UNCONDITIONAL (webhook-delivery→River
	// migration): every event commits to webhook_events in the message tx, so
	// webhook_events is the sole, always-durable event log and at-least-once no
	// longer depends on a flag. WEBHOOKS_OUTBOX_ENABLED is removed; the legacy
	// fire-and-forget publisher path never fires (it was only the flag-off
	// fallback) and is being retired. The outbox drain worker (below) has shipped,
	// so unconditional is safe.
	webhookOutbox := webhookpub.NewOutbox(pool, webhookpub.StaticFlag(true))
	// Outbox-backed publisher for the post-side-effect event sources that used to
	// bypass webhook_events via the legacy publisher (senderidentity domain.*,
	// SNS delivery feedback email.delivered/bounced/complained, hitlworker TTL
	// resolution). Routing them through the outbox makes ALL events flow
	// webhook_events → drain → delivery, so they get a River delivery job like
	// every other event (previously they bypassed the outbox and were stranded).
	outboxPublisher := webhookpub.NewOutboxPublisher(webhookOutbox, pool)
	// Outbox drain worker. Drains webhook_events into
	// webhook_subscriber_deliveries via LISTEN + 1s fallback poll, enqueuing a
	// River delivery job in the same tx (WithDeliveryEnqueuer, below).
	//
	// Telemetry backend: log-based by default; metrics.enabled in config (or
	// E2A_METRICS_ENABLED) swaps in the Prometheus backend and binds the
	// separate exposition listener below. Every instrumented call site reads
	// through this interface so the switch is non-invasive.
	var metrics telemetry.Metrics = telemetry.NewLog()
	var promBackend *telemetry.Prom
	if cfg.Metrics.Enabled {
		promBackend = telemetry.NewProm(cfg.Metrics.Build)
		metrics = promBackend
	}
	store.SetThreadMetrics(metrics)
	outboxWorker := webhookpub.NewOutboxWorker(pool, store).WithMetrics(metrics)
	smtpRelay := outbound.NewSMTPRelay(&cfg.OutboundSMTP)
	sender := outbound.NewSenderWithDKIM(smtpRelay, cfg.OutboundSMTP.FromDomain, store)
	// Own-address From gating (decision 4 / Slice 4): the sender consults the
	// domain's sending_status and uses the agent's own address as From only
	// when "verified" (fail-closed; a missing/none/pending/failed status keeps
	// the relay From). Wiring this is behavior-neutral until a domain verifies.
	sender.SetSendingStatusLookup(store)
	// Delivery feedback (decision 9 / Slice 4b): tag outbound with the SES
	// configuration set so SES publishes delivery/bounce/complaint events.
	// Empty = off (no header, no events).
	sender.SetSESConfigurationSet(cfg.DeliveryFeedback.SESConfigurationSet)
	// Outbound branding/disclosure footer (`outbound_footer:` config block):
	// the CONTENT lives on the sender; the per-send DECISION is stamped on
	// each request by the agent layer (SetOutboundFooterEnabled below +
	// account_limits.outbound_footer_enabled). Empty content = inert.
	sender.SetOutboundFooter(cfg.OutboundFooter.Text, cfg.OutboundFooter.HTML)

	// Sender-identity manager (decision 4 / Slice 4). Only wired when SES is
	// configured: domain verify enqueues a BYODKIM provision job + reconciler;
	// domain delete enqueues a teardown job in the delete tx. Without SES the
	// interface stays a true nil (sending_status never leaves "none").
	var senderEnqueuer apiserver.SenderIdentityEnqueuer
	// jobsClient is the shared River client, built ONCE from whatever registers on
	// it (senderidentity when SES is configured; webhook delivery when
	// delivery_engine=river) — no longer gated on any single subsystem. Hoisted so
	// the shutdown sequence drains it under the shared deadline.
	var jobsClient *jobs.Client
	var registrars []jobs.Registrar

	// Usage tracking is hosted-deployment infrastructure (counts every
	// inbound/outbound message into usage_events + usage_summaries for downstream
	// billing reconciliation). Self-hosters get the no-op tracker by default — the
	// writes are dead weight without an external reader. Set E2A_USAGE_TRACKING=true
	// to enable. Built here (before the River registrars) because the async-send
	// worker's store adapter meters through it.
	var usageTracker usage.UsageTracker = usage.NewNoopUsageTracker()
	if os.Getenv("E2A_USAGE_TRACKING") == "true" {
		usageTracker = usage.NewUsageTracker(usage.NewStore(pool))
		log.Printf("Usage tracking enabled (writing to usage_events + usage_summaries)")
	}

	// Outbound delivery is queue-first and at-least-once for GA. The accept-tx
	// enqueues an outbound_send job in the same transaction as the message row;
	// there is no submit-inline fallback.
	rampStore := sendramp.NewStore(pool)
	outboundRamp := agent.NewOutboundRampGate(
		rampStore,
		sendramp.NewSchedule(cfg.SendingRamp.StartDaily, cfg.SendingRamp.TargetDaily, cfg.SendingRamp.RampDays),
		cfg.SendingRamp.Enabled,
	)
	if cfg.SendingRamp.Enabled {
		log.Printf("Outbound sending ramp enabled: %d→%d recipients over %d qualified days", cfg.SendingRamp.StartDaily, cfg.SendingRamp.TargetDaily, cfg.SendingRamp.RampDays)
	}
	outboundSendStore := agent.NewOutboundSendStore(store, webhookOutbox, usageTracker)
	store.SetScheduledSendFinalizer(outboundSendStore)
	outboundJobs := outboundsend.NewJobs(
		outboundSendStore,
		agent.NewOutboundDeliverer(sender),
		pool,
		outboundRamp,
	).WithMetrics(metrics).
		// Fire-time per-agent rate limit (60 submissions/min/agent sliding
		// window, durable in Postgres): the cross-replica counterpart of the
		// acceptance-time in-memory limiter, enforced immediately before
		// provider submission so scheduled-send bursts can't exceed it.
		WithRateGate(sendrate.NewStore(pool, time.Minute, 60))
	registrars = append(registrars, outboundJobs)
	registrars = append(registrars, sendramp.NewMaintenanceJobs(rampStore))
	// Queue depth/age gauges: a 30s maintenance periodic sampling river_job
	// per queue+state (docs/observability.md).
	registrars = append(registrars, jobs.NewQueueStatsJobs(pool, metrics))

	// Async inbound pipeline (inbound-message-pipeline-river.md), gated by
	// E2A_INBOUND_MODE=async. The InboundProcessWorker registers on the shared River
	// client; the SMTP accept-tx enqueues an inbound_process job in the same tx as
	// the intake row. The Processor (the relay Server) is injected later via
	// SetProcessor — the relay is built below, after the River client. nil ⇒ the
	// synchronous inline path (unchanged).
	var inboundJobs *inboundprocess.Jobs
	if cfg.Inbound.Mode == "async" {
		inboundJobs = inboundprocess.NewJobs(store).WithMetrics(metrics)
		registrars = append(registrars, inboundJobs)
	}

	// Durable HITL approval-notification on River (docs/design/hitl-notify-river.md).
	// The NotifyWorker registers on the shared client; the hold accept-tx enqueues a
	// hitl_notify job in the same tx as the pending_review row. The concrete Deliverer
	// (the Notifier, which needs the relay + signer gating resolved below) is injected
	// later via SetDeliverer — mirrors inbound's late-bound Processor. Gated on the
	// same relay+public-URL config as the notifier itself; when unconfigured, no jobs
	// register and the hold takes the plain path (no notification).
	var notifyJobs *hitlnotify.Jobs
	notifierEnabled := cfg.OutboundSMTP.FromDomain != "" && cfg.HTTP.PublicURL != ""
	if notifierEnabled {
		notifyJobs = hitlnotify.NewJobs(store)
		registrars = append(registrars, notifyJobs)
	}

	// Webhook health notifications on River (docs/design/
	// 2026-08-08-webhook-health-notifications.md): the maintenance sweep
	// enqueues webhook_notify jobs (warning / disabled) in the same tx as the
	// state transition; the NotifyWorker sends the email. Same two-phase
	// wiring as hitlnotify. Gated on the relay from_domain alone — unlike
	// HITL magic links, these emails degrade gracefully without a public URL
	// (generic dashboard copy instead of a link). When unconfigured, no jobs
	// register and the sweep transitions state without notifications
	// (pre-feature behavior).
	var webhookNotifyJobs *webhooknotify.Jobs
	if cfg.OutboundSMTP.FromDomain != "" {
		webhookNotifyJobs = webhooknotify.NewJobs(store).WithMetrics(metrics)
		registrars = append(registrars, webhookNotifyJobs)
	}

	var senderMgr *senderidentity.Manager
	if region := cfg.SenderIdentity.SESRegion; region != "" {
		provider, perr := senderidentity.NewSESProviderFromConfig(ctx, region)
		if perr != nil {
			log.Fatalf("sender identity: build SES provider: %v", perr)
		}
		senderMgr = senderidentity.NewManager(
			senderidentity.NewStoreAdapter(store),
			provider,
			senderIdentityEventFirer(outboxPublisher),
			senderidentity.Config{LegacyJobCompat: cfg.SenderIdentity.LegacyJobCompat},
		)
		registrars = append(registrars, senderMgr)
	}

	// Webhook delivery → River (docs/design/webhook-delivery-river-migration.md).
	// River is now the SOLE delivery engine: the DeliverWorker registers
	// unconditionally, the outbox drain enqueues delivery jobs transactionally,
	// and a one-shot cutover drains any pre-existing pending rows. The legacy
	// hand-rolled SubscriberRetryWorker is gone.
	webhookDeliveryJobs := webhookdelivery.NewJobs(subscriberStore, subscriberDeliverer, store, pool).WithMetrics(metrics)
	registrars = append(registrars, webhookDeliveryJobs)

	// Webhook fan-out (webhook_events → webhook_subscriber_deliveries) on River,
	// replacing the legacy in-process webhookpub.OutboxWorker drain. Gated by
	// E2A_WEBHOOK_FANOUT_MODE=river (webhook-fanout-river-migration.md). When enabled:
	// the FanOutWorker + reconciler are registered, PublishTx/PublishBestEffortTx
	// enqueue a fan-out job in the event tx (SetFanOutEnqueuer, below), and the legacy
	// OutboxWorker is NOT started. It reuses webhookDeliveryJobs as its delivery
	// enqueuer, so the Layer 2→3 delivery path is unchanged. Default (legacy) leaves
	// everything on the OutboxWorker.
	fanoutRiver := cfg.WebhookFanout.Mode == "river"
	var fanoutJobs *webhookpub.FanOutJobs
	if fanoutRiver {
		fanoutJobs = webhookpub.NewFanOutJobs(pool, store, webhookDeliveryJobs, metrics)
		registrars = append(registrars, fanoutJobs)
	}

	// Webhook janitor (auto-disable failing webhooks + warn on attempt-level
	// failure bursts + clear expired prev secrets) as a River periodic on
	// QueueMaintenance — replaces the hand-rolled 5-min ticker. Reuses
	// AutoDisableWorker.Tick as the sweep body. The notifier seam is wired
	// here (nil-safe) so each disable/warn transition enqueues its health
	// email atomically; the shared client reaches the Jobs via SetEnqueuer
	// below, well before the first tick (RunOnStart:false ⇒ +5min).
	webhookSweep := webhook.NewAutoDisableWorker(store)
	if webhookNotifyJobs != nil {
		webhookSweep.SetNotifier(webhookNotifyJobs)
	}
	webhookMaint := webhookdelivery.NewMaintenanceJobs(webhookSweep)
	registrars = append(registrars, webhookMaint)

	// HITL TTL expiration sweep as a River periodic on QueueMaintenance — replaces
	// the hand-rolled 60s ticker. Transitions pending_review messages past their TTL
	// into review_expired_approved (auto-send/release) or review_expired_rejected per
	// the owning agent's hitl_expiration_action. Reuses RunOnce as the sweep body.
	// SetPublisher is called immediately below (before jobs.New, and the registrar
	// holds this same *Worker pointer), so the publisher is wired well before the
	// first tick (RunOnStart:false ⇒ first sweep at +60s).
	managedUnsubscribeIssuer, managedUnsubscribeErr := unsubscribe.NewIssuer(cfg.Signing.HMACSecret, cfg.HTTP.APIURL, cfg.IsProduction(), store)
	if managedUnsubscribeErr != nil {
		log.Fatalf("Managed unsubscribe issuer configuration is invalid: %v", managedUnsubscribeErr)
	}
	hitlWorker := hitlworker.New(store, sender, usageTracker, cfg.OutboundSMTP.FromDomain)
	hitlWorker.SetManagedUnsubscribeIssuer(managedUnsubscribeIssuer)
	// Fire review_approved/review_rejected on TTL auto-resolution, so a hold resolved
	// by timeout notifies subscribers exactly like a human-resolved one (same legacy
	// publisher as the agent API). Load-bearing for inbound approve: a TTL-released
	// inbound message has no other push signal.
	hitlWorker.SetPublisher(outboxPublisher)
	// Self-send auto-approval is a local DB delivery, so its Sent/Inbox rows and
	// email.sent/email.received outcome records share one transaction.
	hitlWorker.SetOutbox(webhookOutbox)
	// Route the sweep's auto-approve send onto QueueOutbound (the
	// SendWorker does the SMTP submit) instead of a blocking in-process send, so the
	// sweep is DB-only. Two-phase like the API's SetOutboundEnqueuer: pass the
	// *outboundsend.Jobs pointer now; its shared client is injected below via
	// outboundJobs.SetEnqueuer, live well before the first +60s tick.
	hitlWorker.SetOutboundEnqueuer(outboundJobs)
	registrars = append(registrars, hitlworker.NewMaintenanceJobs(hitlWorker))

	// Hourly cleanup janitor (expired messages/sessions/webhook delivery
	// records/webhook events/OAuth rows/idempotency keys) as a River periodic on
	// QueueMaintenance — replaces the hand-rolled time.Ticker(1h) goroutine that
	// used to live in the shutdown block. Two of its dependencies (the OAuth
	// storage and the idempotency store) were previously constructed further
	// down; they only need `pool`, so they're built here so every janitor dep is
	// in scope before jobs.New consumes the registrars. Their downstream wiring
	// (api.SetOAuthProvider / api.SetIdempotencyStore, which need `api`) stays
	// below. oauthStorage is nil when the OAuth provider is disabled (no
	// public_url); the janitor skips that pass, preserving the old
	// `if oauthStorage != nil` guard.
	var oauthStorage *oauth.Storage
	if cfg.HTTP.PublicURL != "" {
		oauthStorage = oauth.NewStorage(pool)
	}
	idempotencyStore := idempotency.NewStore(pool)
	var oauthPruner janitor.OAuthPruner
	if oauthStorage != nil {
		oauthPruner = oauthStorage
	}
	cleanupJanitor := janitor.New(store, deliveryStore, subscriberStore, webhookOutbox, oauthPruner, idempotencyStore, metrics)
	// contact.due wake-up: its own River periodic on the maintenance lane, not
	// part of the janitor. It is a scheduled product event with user-visible
	// latency, not a prune, so it gets its own interval and metrics.
	contactDueSweeper := contactdue.NewSweeper(
		contactdue.NewOutboxPublisher(pool, store, webhookOutbox),
		metrics,
	)
	registrars = append(registrars, contactdue.NewJobs(contactDueSweeper))
	registrars = append(registrars, janitor.NewMaintenanceJobs(cleanupJanitor))

	if len(registrars) > 0 {
		jc, jerr := jobs.New(pool, jobs.Config{}, registrars...)
		if jerr != nil {
			log.Fatalf("jobs: build shared river client: %v", jerr)
		}
		jobsClient = jc
		store.SetOutboundJobCanceller(jobsClient)
		if senderMgr != nil {
			senderMgr.SetEnqueuer(jobsClient)
			senderEnqueuer = senderMgr
			log.Printf("[sender-identity] SES provisioning enabled (region=%s)", cfg.SenderIdentity.SESRegion)
		}
		webhookDeliveryJobs.SetEnqueuer(jobsClient)
		outboxWorker.WithDeliveryEnqueuer(webhookDeliveryJobs)
		// One-shot cutover: the legacy SubscriberRetryWorker is gone, so enqueue
		// every pre-existing pending row now — idempotent (job_id IS NULL guard),
		// harmless for rows already carrying a job.
		if res, cerr := webhookDeliveryJobs.ReconcilePending(ctx, pool); cerr != nil {
			log.Printf("[webhook-delivery] cutover: %v", cerr)
		} else if res.Total() > 0 {
			log.Printf("[webhook-delivery] cutover enqueued %d pending deliveries (%d never-enqueued, %d dead-job rescues)",
				res.Total(), res.Enqueued, res.Rescued)
		}
		log.Printf("[webhook-delivery] engine=river")
		// Webhook fan-out cutover (E2A_WEBHOOK_FANOUT_MODE=river): wire the shared
		// client into the fan-out enqueuer, arm the outbox to enqueue a fan-out job in
		// each event tx (SetFanOutEnqueuer), and re-drive any pending events without a
		// job — rows the legacy OutboxWorker was mid-draining at cutover, or stranded by
		// a best-effort-publish enqueue failure. Idempotent (fanout_job_id IS NULL
		// guard). Order matters: SetFanOutEnqueuer BEFORE the servers start (below) so
		// no trigger fires an un-enqueued event; ReconcilePending covers the window
		// before that.
		if fanoutJobs != nil {
			fanoutJobs.SetEnqueuer(jobsClient)
			webhookOutbox.SetFanOutEnqueuer(fanoutJobs)
			if res, cerr := fanoutJobs.ReconcilePending(ctx, pool); cerr != nil {
				log.Printf("[webhook-fanout] cutover: %v", cerr)
			} else if res.Total() > 0 {
				log.Printf("[webhook-fanout] cutover enqueued %d pending events (%d never-enqueued, %d dead-job rescues)",
					res.Total(), res.Enqueued, res.Rescued)
			}
			log.Printf("[webhook-fanout] engine=river")
		}
		// Outbound send cutover: wire the shared client into the
		// accept-tx enqueuer and re-drive any accepted-but-unenqueued rows (stranded
		// by an accept-tx that crashed between message insert and job commit).
		outboundJobs.SetEnqueuer(jobsClient)
		if n, cerr := outboundJobs.ReconcilePending(ctx, pool); cerr != nil {
			log.Printf("[outbound-send] cutover: %v", cerr)
		} else if n > 0 {
			log.Printf("[outbound-send] cutover enqueued %d stranded sends", n)
		}
		log.Printf("[outbound-send] engine=river (async accept, at-least-once)")
		// HITL approval-notification cutover: wire the shared client into the hold
		// accept-tx enqueuer and re-drive any pending_review rows without a
		// notification job (rows held before this shipped, or stranded by a crash
		// between message insert and job commit). Idempotent (notify_job_id IS NULL
		// guard). The concrete Deliverer is bound just below at the notifier gating.
		if notifyJobs != nil {
			notifyJobs.SetEnqueuer(jobsClient)
			if n, cerr := notifyJobs.ReconcilePending(ctx, pool); cerr != nil {
				log.Printf("[hitl-notify] cutover: %v", cerr)
			} else if n > 0 {
				log.Printf("[hitl-notify] cutover enqueued %d un-notified holds", n)
			}
			log.Printf("[hitl-notify] engine=river")
		}
		// Webhook health notifications: wire the shared client into the sweep's
		// enqueuer. No reconciler — the sweep only enqueues on a state
		// transition it commits in the same tx, so there is nothing to
		// re-drive at startup (a transition without its job cannot exist).
		if webhookNotifyJobs != nil {
			webhookNotifyJobs.SetEnqueuer(jobsClient)
			log.Printf("[webhook-notify] engine=river")
		}
		// Stop is driven from the shutdown sequence under the shared deadline.
		if serr := jobsClient.Start(ctx); serr != nil {
			log.Fatalf("jobs: start shared river client: %v", serr)
		}
	}

	// User auth (Google OAuth for agent developers)
	userAuth := auth.NewUserAuth(&cfg.OAuth, store, cfg.IsProduction())
	// Generic OIDC Authorization Code login. Disabled configurations perform
	// no discovery and leave both OIDC routes unregistered. Enabled
	// configurations construct synchronously (no network call) and discover
	// the issuer in a background goroutine with retry/backoff, so an
	// unreachable identity provider never blocks or fails server boot; the
	// OIDC routes fail closed with 503 until discovery completes. err is
	// retained for any future static/constructor-time failure mode, but
	// today NewOIDCAuth only fails to construct if E2A is misconfigured in a
	// way config.Validate() would already have caught.
	oidcCtx, oidcCancel := context.WithCancel(ctx)
	oidcAuth, err := auth.NewOIDCAuth(oidcCtx, cfg.OIDC, store, cfg.IsProduction(), cfg.HTTP.PublicURL)
	if err != nil {
		log.Fatalf("Failed to initialize OIDC login: %v", err)
	}
	if oidcAuth != nil {
		log.Printf("[auth] OIDC login enabled (issuer=%s); discovering issuer in the background", cfg.OIDC.IssuerURL)
	}

	// HTTP API
	router := mux.NewRouter()
	api := agent.NewAPI(store, sender, smtpRelay, userAuth, usageTracker, cfg.SMTP.Domain, cfg.OutboundSMTP.FromDomain, cfg.SharedDomain, cfg.HTTP.PublicURL, cfg.IsProduction())
	// The programmatic API host (OAuth issuer + token/jwks). Defaults to
	// public_url; set api_url to serve the API/MCP on a different host than
	// the web app (the authorization_endpoint + login/consent stay on
	// public_url because they need the web app's session cookie).
	api.SetAPIURL(cfg.HTTP.APIURL)
	api.SetPollRateLimit(cfg.RateLimits.PollPerMinute)
	// HITL magic-link token signer reuses the shared HMAC secret so operators
	// don't have to configure a second key.
	approvalSigner := approvaltoken.NewSigner(cfg.Signing.HMACSecret)
	api.SetApprovalSigner(approvalSigner)
	// auth.md agent-token signer (Slice 5b). A malformed key is fatal (fail
	// fast at startup); an empty key yields a disabled signer (JWKS serves an
	// empty set) so deployments not using agent identity run unchanged.
	jwtSigner, err := agentauth.NewSigner(cfg.OAuth.SigningKey, cfg.OAuth.SigningKID)
	if err != nil {
		log.Fatalf("Failed to load OAuth signing key: %v", err)
	}
	if jwtSigner.Enabled() {
		log.Printf("[agentauth] JWT signing enabled (kid=%s)", jwtSigner.KeyID())
	} else {
		log.Printf("[agentauth] JWT signing disabled (E2A_OAUTH_SIGNING_KEY not set); /.well-known/jwks.json serves an empty set")
	}
	api.SetSigner(jwtSigner)
	api.SetOIDCAuth(oidcAuth)
	// HITL reviewer-notification emails. Requires both a configured
	// outbound SMTP relay (to actually send) and a public base URL (so
	// the magic links in the email are absolute and clickable from any
	// mail client). Without PublicURL, links would be relative and
	// unusable — safer to skip the notifier entirely and log a warning
	// so operators notice the misconfiguration.
	if cfg.OutboundSMTP.FromDomain == "" {
		log.Printf("[hitl] notifier disabled: outbound_smtp.from_domain is not set")
	} else if cfg.HTTP.PublicURL == "" {
		log.Printf("[hitl] notifier disabled: http.public_url is not set (magic links require an absolute base URL)")
	} else if notifyJobs == nil {
		// notifierEnabled matches these same two config checks, so this is
		// unreachable in practice — kept as a defensive guard against future drift.
		log.Printf("[hitl] notifier disabled: notification job pipeline not registered")
	} else {
		notifier := hitlnotify.New(store, smtpRelay, approvalSigner, cfg.OutboundSMTP.FromDomain, cfg.Notifications.FromAddress, cfg.Notifications.ReplyTo, cfg.HTTP.PublicURL).WithDKIM(store)
		// Late-bind the concrete Deliverer onto the registered NotifyWorker (which
		// has been running since jobsClient.Start; jobs enqueued before this bind
		// simply retry) and give the hold path its accept-tx enqueuer. The HTTP
		// server isn't accepting requests yet, so no hold can miss the enqueuer.
		notifyJobs.SetDeliverer(notifier)
		api.SetNotifyEnqueuer(notifyJobs)
	}

	// Webhook health-notification sender. Same late-bind as the HITL notifier:
	// jobs enqueued before this point simply retry against the nil-deliverer
	// guard. The from-address is notifications.from_address when set (hosted:
	// a replyable support address), else the fixed no-reply local part on the
	// relay from_domain — configuration, never a constant, because a hardcoded
	// operator address would break every self-host (they don't own it).
	// The notifier DKIM-signs in-process for the From-header domain (same
	// store-backed key lookup the outbound Sender uses): the relay never
	// signs, and an upstream like SES only signs identities it manages, so
	// a BYODKIM custom from-address domain is signed here or not at all.
	// Fail-open — no stored key (self-host default) sends unsigned.
	if webhookNotifyJobs != nil {
		whNotifier := webhooknotify.New(store, smtpRelay, cfg.OutboundSMTP.FromDomain, cfg.Notifications.FromAddress, cfg.Notifications.ReplyTo, cfg.HTTP.PublicURL).WithDKIM(store)
		webhookNotifyJobs.SetDeliverer(whNotifier)
		log.Printf("[webhook-notify] enabled (from=%s)", whNotifier.FromAddress())
	} else {
		log.Printf("[webhook-notify] disabled: outbound_smtp.from_domain is not set")
	}

	// OAuth 2.1 / fosite-backed authorization server. Needs the same
	// HMAC secret (signing.hmac_secret) for token HMAC signing and the
	// public URL as the canonical issuer. Without PublicURL, RFC 9207
	// `iss` emission + discovery would emit empty/inconsistent values
	// — skip wiring so /oauth2/* return 404 and operators get a
	// loud signal that the deployment needs http.public_url set.
	// oauthStorage was constructed above (near the janitor wiring) so the cleanup
	// periodic can prune expired OAuth rows; here we wire the provider on top of
	// it when a public URL is configured.
	if cfg.HTTP.PublicURL == "" {
		log.Printf("[oauth] provider disabled: http.public_url is not set (required for issuer identity)")
	} else {
		// Issuer = api_url (defaults to public_url). fosite stamps it into
		// token `iss` and the RFC 9207 response, so it must match what
		// discovery advertises and what oauthIssuer signs/verifies.
		oauthProvider, err := oauth.NewProvider(oauthStorage, cfg.HTTP.APIURL, []byte(cfg.Signing.HMACSecret))
		if err != nil {
			log.Fatalf("[oauth] provider wiring failed: %v", err)
		}
		api.SetOAuthProvider(oauthProvider)
		// Consent handler also needs the storage pool for the cross-
		// package transaction (agent insert + auth-code insert atomic).
		api.SetOAuthStorage(oauthStorage)
		log.Printf("[oauth] provider enabled: issuer=%s", cfg.HTTP.APIURL)
	}

	// Idempotency-Key support on POST /v1/agents/{email}/messages (send) and .../reply.
	// Replays the cached response on retry; closes the double-send window for callers
	// behind at-least-once delivery (job queues, agent frameworks that retry tool calls,
	// model-driven re-invocations). Always wired in production — keeping it optional in
	// the agent package surfaces a clearer 5xx path for environments that don't run
	// against this codebase's postgres.
	// idempotencyStore was constructed above (near the janitor wiring) so the
	// cleanup periodic can sweep expired keys; here we bind it onto the API.
	api.SetIdempotencyStore(idempotencyStore)

	// Resource-limits enforcer. The OSS server is plan-agnostic: it
	// reads the per-user caps from account_limits and enforces them at
	// agent-create / domain-register / message-send / inbound RCPT TO.
	// What "Free" or "Pro" mean (and how Stripe plumbs those into
	// account_limits) is the responsibility of an external provisioner
	// (hosted billing sidecar, admin tooling). Self-hosters who don't
	// run a provisioner get the generous config defaults applied to
	// every user — effectively unlimited unless they tighten the
	// `limits:` config block.
	usageStore := usage.NewStore(pool)
	enforcer := limits.NewEnforcer(
		limits.NewStore(pool),
		usageStore,
		limits.Defaults{
			PlanCode:         cfg.Limits.PlanCode,
			MaxAgents:        cfg.Limits.MaxAgents,
			MaxDomains:       cfg.Limits.MaxDomains,
			MaxMessagesMonth: cfg.Limits.MaxMessagesMonth,
			MaxStorageBytes:  cfg.Limits.MaxStorageBytes,
			// Row-less fallback for the outbound-footer entitlement — the
			// `outbound_footer:` block's default_enabled, not `limits:`.
			OutboundFooterEnabled: cfg.OutboundFooter.DefaultEnabled,
		},
		time.Duration(cfg.Limits.CacheTTLSeconds)*time.Second,
	)
	api.SetEnforcer(enforcer)
	// Master switch for the outbound footer; the enforcer above carries the
	// per-account entitlement + row-less default the decision reads.
	api.SetOutboundFooterEnabled(cfg.OutboundFooter.Enabled)
	// The TTL sweep composes held mail at auto-approve time, so it needs the
	// same per-account footer decision the human-approve funnel resolves —
	// otherwise an expires-to-approve hold ships unfootered while the identical
	// human-approved hold does not.
	hitlWorker.SetOutboundFooterResolver(api.OutboundFooterForAccount)
	// Fire-time monthly-cap gate for scheduled sends: enforce the cap in the
	// month a scheduled send actually fires (accept-time enforcement can't know a
	// future fire month). Over-cap → refuse terminally; a transient lookup error
	// fails open so a glitch never drops a legitimate send.
	outboundSendStore.SetScheduledSendQuota(func(ctx context.Context, userID string) (bool, error) {
		return scheduledSendMonthlyQuotaResult(enforcer.CheckMessageSend(ctx, userID))
	})
	api.SetUsageStore(usageStore)
	api.SetInternalAPISecret(cfg.Limits.InternalAPISecret)
	api.ConfigureProvisioning(cfg.Provisioning.Enabled, cfg.Provisioning.Secret)
	api.SetBillingHookURL(cfg.Limits.BillingHookURL)
	api.SetSubscriberStore(subscriberStore)
	// Account-delete cascade (decision 4 / Slice 4): when SES is configured,
	// DELETE /account enqueues an SES teardown job for every owned domain in
	// the delete tx, after stamping each domain's ledger tombstone so no
	// audit/sweep finalizes it inside this delete's drain window. Per-domain
	// DELETE teardown is wired the same way in apiserver.
	if senderEnqueuer != nil {
		api.SetDomainTeardownHook(func(ctx context.Context, tx pgx.Tx, domain string) error {
			// Enqueue first, stamp second — the stamp should sit as close to
			// commit as the tx allows (see TouchSendingIdentityTombstoneTx).
			if err := senderEnqueuer.EnqueueDeprovisionTx(ctx, tx, domain); err != nil {
				return err
			}
			return store.TouchSendingIdentityTombstoneTx(ctx, tx, domain)
		})
	}
	api.SetOutbox(webhookOutbox)
	// The outbound accept-tx enqueuer is mandatory: DeliverOutbound always
	// persists+enqueues and returns accepted before provider submission.
	api.SetOutboundEnqueuer(outboundJobs)
	// Slices 6 + 7: customer-facing events API needs the raw pool to
	// query webhook_events and write webhook_subscriber_deliveries on
	// replay. Kept as a separate setter so a future refactor can route
	// through a higher-level abstraction.
	api.SetPoolForEvents(pool)
	api.SetMetrics(metrics)

	api.RegisterRoutes(router)

	// draining flips at the start of graceful shutdown (see the signal block
	// below): /readyz reports 503 while /api/health stays green.
	var draining atomic.Bool
	// /readyz — instance-local readiness (DB reachable + migrations applied).
	// Distinct from /api/health (shallow liveness); operational, not part of the
	// /v1 contract, so it lives on this mux and never enters api/openapi.yaml.
	// The verdict is refreshed on a background ticker, so this handler never
	// competes for a pooled connection — see readyz.go for why that matters.
	readiness := newReadinessMonitor(pool, &draining)
	defer readiness.Stop()
	router.HandleFunc("/readyz", readiness.handler()).Methods(http.MethodGet)
	// /selftest — deep dependency diagnostics (health+json), auth-gated by the
	// internal API secret. Operational, not part of the /v1 contract. The full
	// SMTP→webhook round-trip lives in the external e2a-prober, not here.
	router.HandleFunc("/selftest", selftestHandler(pool, cfg.SMTP.ListenAddr, cfg.Limits.InternalAPISecret, !cfg.IsProduction())).Methods(http.MethodGet)

	// Public SES-over-SNS delivery notifications endpoint (decision 9 / Slice
	// 4b). Fail-closed: the SNS signature is verified and the TopicArn must be
	// in the configured allow-list (empty allow-list → every message is
	// rejected, so this is inert until ops wires the topic).
	deliveryConsumer := delivery.NewConsumer(store, deliveryEventFirer(webhookOutbox), outboundSendStore.FinalizeProviderAcceptedTx)
	deliveryVerifier := delivery.NewVerifier(cfg.DeliveryFeedback.SNSTopicARNs, delivery.HTTPCertFetcher)
	// Public webhook receiver for AWS SNS (SES delivery/bounce/complaint). Named
	// /webhooks/<provider> — it's an inbound third-party callback, not an internal
	// RPC, and must be internet-reachable for SNS to POST to it. Protected by the
	// SNS signature + TopicArn allow-list above, not by network isolation.
	router.HandleFunc("/webhooks/ses", delivery.Handler(deliveryVerifier, deliveryConsumer)).Methods(http.MethodPost)

	// WebSocket live-tail transport. Registered as a first-class /v1 route
	// on the chi root via WSHandle below (see apiserver.Params); the hub +
	// handler are constructed here and threaded in.
	wsHub := ws.NewHub()
	defer wsHub.Close()
	wsHub.SetMetrics(metrics)
	api.SetWebSocketHub(wsHub)
	hitlWorker.SetWebSocketHub(wsHub)
	wsHandler := ws.NewHandler(wsHub, store)
	wsHandler.SetMetrics(metrics)

	// v1 contract layer (api-v1-redesign Slice 1). The new chi + Huma surface
	// owns the `/v1` prefix (OpenAPI-as-source-of-truth, standardized error
	// envelope + cursor pagination + X-Request-Id), and falls back to the
	// legacy gorilla/mux for the remaining non-v1 routes (OAuth, session auth,
	// health/feedback). The HITL magic-link pages (/v1/approve, /v1/reject)
	// are registered directly on the chi root via the MagicLink* deps.
	// The `/api/v1` surface is fully retired.
	// The chi root is the process HTTP handler.
	v1 := apiserver.New(apiserver.Params{
		API:                       api,
		Store:                     store,
		Enforcer:                  enforcer,
		UsageStore:                usageStore,
		SubscriberStore:           subscriberStore,
		Idempotency:               idempotencyStore,
		Pool:                      pool,
		SMTPDomain:                cfg.SMTP.Domain,
		SESRegion:                 cfg.SenderIdentity.SESRegion,
		SharedDomain:              cfg.SharedDomain,
		PublicURL:                 cfg.HTTP.PublicURL,
		SigningSecret:             cfg.Signing.HMACSecret,
		EventsEnabled:             webhookOutbox.Enabled(),
		Production:                cfg.IsProduction(),
		Legacy:                    router,
		WSHandle:                  wsHandler.ServeWithEmail,
		Metrics:                   metrics,
		SenderIdentity:            senderEnqueuer,
		ManagedUnsubscribeIssuer:  managedUnsubscribeIssuer,
		AgentSuppressionAddedHook: agent.AgentSuppressionAddedHook(webhookOutbox),
		// River is the sole webhook delivery engine: the /test + redelivery
		// endpoints insert a delivery row directly (bypassing the outbox drain),
		// so they must enqueue the River job themselves or the row never delivers.
		EnqueueDelivery: func(ctx context.Context, deliveryID string) error {
			return webhookDeliveryJobs.EnqueueDelivery(ctx, pool, deliveryID)
		},
	})

	httpServer := newHTTPServer(cfg.HTTP.ListenAddr, v1)

	// SMTP Relay
	smtpServer := relay.NewServer(cfg, store, usageTracker, wsHub)
	smtpServer.SetEnforcer(enforcer)
	smtpServer.SetOutbox(webhookOutbox)
	smtpServer.SetMetrics(metrics)

	// Wire the async inbound pipeline into the relay (E2A_INBOUND_MODE=async): the
	// Processor is the relay Server itself; the accept-tx enqueues via the shared
	// client. Done here because the relay is constructed after the River client — the
	// worker late-binds the Processor (SetProcessor) and tolerates the brief startup
	// window before it's set.
	if inboundJobs != nil {
		inboundJobs.SetProcessor(smtpServer)
		inboundJobs.SetEnqueuer(jobsClient)
		smtpServer.SetInboundEnqueuer(inboundJobs)
		// Cutover: enqueue any accepted-but-unenqueued intake rows (pre-async rows at
		// the mode-flip, or rows stranded by a crash between insert and enqueue).
		// Idempotent (process_job_id IS NULL guard). Runs after SetProcessor so the
		// re-driven jobs find a wired processor.
		if n, cerr := inboundJobs.ReconcilePending(ctx, pool); cerr != nil {
			log.Printf("[inbound-process] cutover: %v", cerr)
		} else if n > 0 {
			log.Printf("[inbound-process] cutover enqueued %d stranded intakes", n)
		}
		log.Printf("[inbound-process] engine=river (async accept, E2A_INBOUND_MODE=async)")
	}

	// Graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Metrics exposition listener (Prometheus backend only). A SEPARATE
	// listener — never the public API handler — so /metrics can't be exposed
	// through the API host; default bind is loopback-only (config metrics:).
	var metricsServer *http.Server
	if promBackend != nil {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promBackend.Handler())
		metricsServer = &http.Server{
			Addr:              cfg.Metrics.ListenAddr,
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
		}
		if host, _, err := net.SplitHostPort(cfg.Metrics.ListenAddr); err == nil {
			if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
				log.Printf("WARNING: metrics listener bound to non-loopback %s — GET /metrics is UNAUTHENTICATED; ensure network policy restricts access", cfg.Metrics.ListenAddr)
			}
		}
		go func() {
			log.Printf("metrics listening on %s", cfg.Metrics.ListenAddr)
			if err := metricsServer.ListenAndServe(); err != http.ErrServerClosed {
				log.Fatalf("metrics server error: %v", err)
			}
		}()
	}

	go func() {
		log.Printf("HTTP API listening on %s", cfg.HTTP.ListenAddr)
		if err := httpServer.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	go func() {
		if err := smtpServer.ListenAndServe(); err != nil {
			log.Fatalf("SMTP server error: %v", err)
		}
	}()

	// workerWG tracks every background goroutine that needs to drain
	// before the process exits on SIGTERM. Without it the main
	// goroutine would return as soon as httpServer.Shutdown returns,
	// dropping in-flight webhook deliveries mid-iteration. bgCancel signals
	// the remaining background worker(s) to stop; wg.Wait() at the end of
	// shutdown blocks for the current iteration to settle.
	//
	// (The HITL TTL sweep, the hourly cleanup janitor, and the HITL
	// approval-notification email are all River jobs now — QueueMaintenance
	// periodics and QueueNotify respectively — drained by the shared
	// jobsClient under the shutdown deadline, not hand-rolled goroutines.)
	var workerWG sync.WaitGroup

	// Webhook subscriber delivery now runs entirely on River's DeliverWorker
	// (registered above). The legacy in-process SubscriberRetryWorker is gone.
	// bgCtx is the shared cancel for the remaining webhook-delivery-adjacent
	// background worker (the outbox drain) so a single shutdown signal stops it.
	// (The auto-disable janitor is now a River periodic on QueueMaintenance; the
	// legacy SubscriberRetryWorker is gone.)
	bgCtx, bgCancel := context.WithCancel(context.Background())

	// Outbox publisher worker: drains webhook_events → subscriber_deliveries and
	// enqueues River delivery jobs in-tx. Skipped when fan-out runs on River
	// (E2A_WEBHOOK_FANOUT_MODE=river) — the FanOutWorker + in-tx enqueue replace it;
	// running both would double-fan-out (harmless via the (event_id, webhook_id) dedup,
	// but wasteful). Default (legacy) starts it as before.
	if !fanoutRiver {
		workerWG.Add(1)
		go func() {
			defer workerWG.Done()
			outboxWorker.Start(bgCtx)
		}()
	} else {
		log.Printf("[webhook-fanout] legacy OutboxWorker not started (engine=river)")
	}

	// (The HITL TTL expiration sweep is now a River periodic on QueueMaintenance,
	// registered above via hitlworker.NewMaintenanceJobs and drained by the shared
	// jobsClient under the shutdown deadline — no separate goroutine/cancel needed.)

	// The hourly cleanup janitor (expired messages/sessions/webhook records/
	// OAuth rows/idempotency keys) is now a River periodic on QueueMaintenance
	// (janitor.MaintenanceJobs, registered above). It drains on shutdown via the
	// shared jobsClient.Stop below — no separate cancel-context needed.

	<-sigCh
	log.Println("Shutting down...")

	// Flip /readyz to 503 immediately so load balancers stop routing to this
	// instance while in-flight work drains below. Liveness stays green.
	draining.Store(true)

	// Stop OIDC issuer discovery promptly, including an in-flight attempt.
	oidcCancel()

	// Signal the remaining background workers to stop. Their inner ctx-select
	// branches return on the next iteration; work already in flight finishes
	// its current row before the goroutine exits.
	bgCancel()

	// SMTP server: close the listener so no new connections, but
	// existing connections finish their DATA per the relay's own
	// connection lifecycle.
	smtpServer.Close()

	// Single shared deadline for both HTTP shutdown and worker drain.
	// 30s matches Kubernetes' default terminationGracePeriodSeconds.
	// A naive `httpServer.Shutdown(30s)` followed by a second 30s
	// `workerWG.Wait()` would budget 60s total — past the platform's
	// SIGKILL window, the kernel reaps us before the drain phase even
	// runs. Sharing one deadline guarantees we don't outlast the
	// platform grace period, with whichever phase finishes first
	// donating the remainder to the other.
	//
	// Operators wanting longer drain (e.g. SMTP send to slow recipient
	// mid-flight) should bump terminationGracePeriodSeconds AND the
	// constant below in lockstep.
	const shutdownBudget = 30 * time.Second
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownBudget)
	defer shutdownCancel()

	// Drain the shared River client concurrently with HTTP shutdown, under the
	// SAME deadline. Stop() halts claiming new jobs immediately (so a
	// terminating replica stops picking up fresh work, like the cancelled
	// workers above) and drains in-flight jobs; bounding it by shutdownCtx means
	// a job stuck in an SES/network call is abandoned when the shared budget
	// expires rather than blocking process exit past the platform grace period.
	riverDone := make(chan struct{})
	go func() {
		if jobsClient != nil {
			if err := jobsClient.Stop(shutdownCtx); err != nil && shutdownCtx.Err() == nil {
				log.Printf("River client stop: %v", err)
			}
		}
		close(riverDone)
	}()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("HTTP server shutdown error: %v", err)
	}
	// The metrics listener outlives the API server deliberately: the drain
	// window is exactly when the gauges matter most to an observer.
	if metricsServer != nil {
		if err := metricsServer.Shutdown(shutdownCtx); err != nil {
			log.Printf("metrics server shutdown error: %v", err)
		}
	}

	// Wait for workers' current iteration to settle, bounded by the
	// REMAINING share of the same deadline (whatever Shutdown didn't
	// consume). Past it, fall through and let the goroutines die
	// with the process.
	drainDone := make(chan struct{})
	go func() {
		workerWG.Wait()
		close(drainDone)
	}()
	select {
	case <-drainDone:
		log.Println("Background workers drained cleanly.")
	case <-shutdownCtx.Done():
		log.Println("Background workers did not drain within shutdown budget; exiting anyway.")
	}
	// River drain is bounded by the same shutdownCtx, so this returns by the
	// deadline regardless.
	<-riverDone
}

// scheduledSendMonthlyQuotaResult narrows the general send-limit check to the
// fire-time concern for scheduled messages. The message is already persisted,
// so a later storage-cap breach must not cancel it; only the monthly message
// cap for the month in which it fires is terminal.
func scheduledSendMonthlyQuotaResult(err error) (bool, error) {
	if err == nil {
		return false, nil
	}
	if limitErr, ok := limits.IsLimitExceeded(err); ok {
		return limitErr.Resource == "messages_month", nil
	}
	return false, err
}
