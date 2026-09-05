package main

import (
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tokencanopy/e2a/internal/agent"
	"github.com/tokencanopy/e2a/internal/outbound"
	"github.com/tokencanopy/e2a/internal/outboundsend"
	"github.com/tokencanopy/e2a/internal/sendingpolicy"
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
