package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/riverqueue/river"

	"github.com/tokencanopy/e2a/internal/agent"
	"github.com/tokencanopy/e2a/internal/config"
	"github.com/tokencanopy/e2a/internal/outbound"
	"github.com/tokencanopy/e2a/internal/sendingpolicy"
	"github.com/tokencanopy/e2a/internal/testutil/testdb"
	"github.com/tokencanopy/e2a/internal/usage"
)

// TestSendingPolicyWiring builds the production outbound composition from
// synthetic inputs and proves the registered send path holds the concrete
// Gate and the authorized submitter. It exists so that a refactor that
// reintroduced a raw sender or a direct ramp gate in the worker's path could
// not pass CI: the only deliverer the composition root may produce is the one
// over outbound.ProviderSubmitter, and the only admission authority is the
// sendingpolicy module.
func TestSendingPolicyWiring(t *testing.T) {
	pool := testdb.TestDB(t)
	relay := outbound.NewSMTPRelay(&config.OutboundSMTPConfig{Host: "relay.invalid", Port: 587, FromDomain: "test.e2a.dev"})

	composed := newOutboundSending(outboundSendingDeps{
		pool:         pool,
		store:        nil, // the store is not exercised by construction
		relay:        relay,
		secrets:      sendingpolicy.Secrets{},
		source:       sendingpolicy.PolicySourceConfig,
		policy:       sendingpolicy.DisabledPolicy(),
		sesConfigSet: "e2a-delivery-test",
	})

	if _, ok := composed.gate.(*sendingpolicy.Module); !ok {
		t.Fatalf("gate is %T, want the concrete *sendingpolicy.Module", composed.gate)
	}
	if composed.submitter == nil {
		t.Fatal("no authorized submitter composed")
	}
	if got := composed.submitter.SESConfigurationSet(); got != "e2a-delivery-test" {
		t.Fatalf("submitter configuration set = %q, want the deployment's — delivery feedback must stay on", got)
	}
	if composed.jobs.Gate() != composed.gate {
		t.Fatal("the jobs bundle does not hold the composed gate")
	}
	// The worker RegisterJobs registers is what runs in production; it, not
	// the bundle, must carry the gate and the legacy resolver. Without the
	// resolver every job in flight at cutover would fail closed.
	// Register exactly as main does and inspect what River received — the
	// constructor alone would not catch a RegisterJobs that bypassed it.
	composed.jobs.RegisterJobs(river.NewWorkers())
	worker := composed.jobs.RegisteredSendWorker()
	if worker == nil {
		t.Fatal("RegisterJobs registered no send worker")
	}
	if worker.Gate() != composed.gate {
		t.Fatal("the registered send worker does not hold the composed gate")
	}
	if !worker.HasOperationResolver() {
		t.Fatal("the registered send worker has no legacy operation resolver")
	}
	if composed.jobs.TerminalReconcileWorker() == nil {
		t.Fatal("no terminal reconciler composed")
	}
	if got := fmt.Sprintf("%T", composed.jobs.Deliverer()); !strings.HasSuffix(got, "agent.outboundDeliverer") {
		t.Fatalf("worker deliverer is %s, want the ProviderSubmitter-backed agent.outboundDeliverer", got)
	}

	// The composed gate is live: a config-source module answers policy reads
	// against the real database, which is what the worker will do.
	if _, err := composed.gate.LookupOperation(context.Background(), "op_wiring_probe"); err == nil {
		t.Fatal("a never-prepared operation resolved")
	}
}

// TestNotificationAndPlatformMailWiring pins the three composition-root
// edges the AST closure guard cannot see: both notification bundles hold the
// gate (so their enqueues prepare operations and their workers authorize),
// and the API holds the submitter + gate for public feedback. Dropping any
// of them fails closed at runtime with an opaque "authorization required"
// error; this is where it fails loudly instead.
func TestNotificationAndPlatformMailWiring(t *testing.T) {
	pool := testdb.TestDB(t)
	relay := outbound.NewSMTPRelay(&config.OutboundSMTPConfig{Host: "relay.invalid", Port: 587, FromDomain: "test.e2a.dev"})
	composed := newOutboundSending(outboundSendingDeps{
		pool:    pool,
		relay:   relay,
		secrets: sendingpolicy.Secrets{},
		source:  sendingpolicy.PolicySourceConfig,
		policy:  sendingpolicy.DisabledPolicy(),
	})

	n := newNotificationJobs(notificationDeps{pool: pool, gate: composed.gate, hitlEnabled: true, webhookEnabled: true})
	if n.hitl == nil || n.hitl.Gate() != composed.gate {
		t.Fatal("hitl notification bundle does not hold the composed gate")
	}
	if n.webhook == nil || n.webhook.Gate() != composed.gate {
		t.Fatal("webhook notification bundle does not hold the composed gate")
	}
	// The registered workers are what run; they must carry the gate too.
	if w := n.hitl.NotifyWorker(); w == nil || w.Gate() != composed.gate {
		t.Fatal("hitl notify worker registered without the gate")
	}
	if w := n.webhook.NotifyWorker(); w == nil || w.Gate() != composed.gate {
		t.Fatal("webhook notify worker registered without the gate")
	}

	off := newNotificationJobs(notificationDeps{pool: pool, gate: composed.gate})
	if off.hitl != nil || off.webhook != nil {
		t.Fatal("unconfigured notifications must register nothing")
	}

	api := agent.NewAPI(nil, nil, relay, nil, usage.NewNoopUsageTracker(), "e2a.dev", "test.e2a.dev", "agents.e2a.dev", "", false)
	if api.ProviderSubmitterWired() {
		t.Fatal("a fresh API must not claim a submitter")
	}
	composed.armAPI(api)
	if !api.ProviderSubmitterWired() {
		t.Fatal("armAPI did not hand the API the submitter and gate")
	}
}
