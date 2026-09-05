package main

import (
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tokencanopy/e2a/internal/agent"
	"github.com/tokencanopy/e2a/internal/hitlnotify"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/outbound"
	"github.com/tokencanopy/e2a/internal/outboundsend"
	"github.com/tokencanopy/e2a/internal/sendingpolicy"
	"github.com/tokencanopy/e2a/internal/webhooknotify"
)

// outboundSendingDeps is everything the outbound composition root needs. It
// is a struct rather than positional arguments so the wiring test can build
// the production composition from synthetic inputs and inspect the result.
type outboundSendingDeps struct {
	pool         *pgxpool.Pool
	store        outboundsend.Store
	relay        *outbound.SMTPRelay
	secrets      sendingpolicy.Secrets
	source       sendingpolicy.PolicySource
	policy       sendingpolicy.RuntimePolicy
	sesConfigSet string
	metrics      outboundsend.Metrics
	rate         outboundsend.RateGate
}

// outboundSending is the composed outbound send path.
type outboundSending struct {
	gate      sendingpolicy.Gate
	submitter *outbound.ProviderSubmitter
	jobs      *outboundsend.Jobs
}

// newOutboundSending is the ONE composition root for provider-bound customer
// mail. The gate is the deployment's policy authority; the submitter is the
// only object that opens a socket to the provider and it refuses to do so
// without a token from that gate; the jobs bundle prepares an operation at
// enqueue and authorizes every worker execution through the same gate. No
// raw sender and no direct ramp store reach the worker from here.
func newOutboundSending(d outboundSendingDeps) outboundSending {
	gate := sendingpolicy.NewGate(d.pool, d.secrets, d.source, d.policy)
	submitter := outbound.NewProviderSubmitter(d.relay, gate)
	// Delivery feedback: tag outbound with the SES configuration set so SES
	// publishes delivery/bounce/complaint events. Empty = off.
	submitter.SetSESConfigurationSet(d.sesConfigSet)
	jobs := outboundsend.NewJobs(d.store, agent.NewOutboundDeliverer(submitter), d.pool).
		WithGate(gate).
		WithMetrics(d.metrics).
		WithRateGate(d.rate)
	return outboundSending{gate: gate, submitter: submitter, jobs: jobs}
}

// notificationDeps is what the notification composition needs: the same gate
// and pool the customer path uses, plus the two config gates main applies.
type notificationDeps struct {
	store          *identity.Store
	pool           *pgxpool.Pool
	gate           sendingpolicy.Gate
	metrics        webhooknotify.Metrics
	hitlEnabled    bool // outbound_smtp.from_domain and http.public_url set
	webhookEnabled bool // outbound_smtp.from_domain set
}

// notificationJobs are the two notification job bundles, nil when their
// feature is unconfigured (no worker registers, the sweep/hold take the
// plain path).
type notificationJobs struct {
	hitl    *hitlnotify.Jobs
	webhook *webhooknotify.Jobs
}

// newNotificationJobs composes the notification bundles over the ONE gate.
// Every enqueue prepares a customer_notification operation in the source
// transaction and every worker execution authorizes through the gate; a
// bundle built any other way would fail closed at runtime (empty token) with
// an error that says nothing about wiring, which is why the composition is
// factored here and pinned by the wiring test.
func newNotificationJobs(d notificationDeps) notificationJobs {
	var n notificationJobs
	if d.hitlEnabled {
		n.hitl = hitlnotify.NewJobs(d.store).WithGate(d.gate, d.pool)
	}
	if d.webhookEnabled {
		n.webhook = webhooknotify.NewJobs(d.store).WithMetrics(d.metrics).WithGate(d.gate, d.pool)
	}
	return n
}

// armAPI hands the API the authorized seam for the platform mail it sends
// itself (public feedback).
func (s outboundSending) armAPI(api *agent.API) {
	api.SetProviderSubmitter(s.submitter, s.gate)
}
