package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/tokencanopy/e2a/internal/config"
	"github.com/tokencanopy/e2a/internal/outbound"
	"github.com/tokencanopy/e2a/internal/sendingpolicy"
	"github.com/tokencanopy/e2a/internal/testutil/testdb"
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
	worker := composed.jobs.SendWorker()
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
