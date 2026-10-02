package main

import (
	"context"
	"testing"
	"time"

	"github.com/tokencanopy/e2a/internal/sendingpolicy"
	"github.com/tokencanopy/e2a/internal/telemetry"
	"github.com/tokencanopy/e2a/internal/testutil/testdb"
)

type budgetSnapshotRecorder struct {
	telemetry.NoOp
	samples chan int
	cancel  context.CancelFunc
}

func (r *budgetSnapshotRecorder) SendingBudgetSnapshot(used map[string]float64, _ int64, _ bool, _ float64) {
	r.samples <- len(used)
	r.cancel()
}

func TestSendingBudgetSamplerPublishesAndStops(t *testing.T) {
	pool := testdb.TestDB(t)
	module := sendingpolicy.NewPolicyModule(pool, sendingpolicy.Secrets{}, sendingpolicy.PolicySourceConfig, sendingpolicy.DisabledPolicy())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	recorder := &budgetSnapshotRecorder{samples: make(chan int, 1), cancel: cancel}
	done := make(chan struct{})
	go func() { defer close(done); observeSendingBudgets(ctx, module, recorder) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("sampler did not stop")
	}
	select {
	case count := <-recorder.samples:
		if count != 4 {
			t.Fatalf("got %d scopes", count)
		}
	default:
		t.Fatal("no sample")
	}
	// A failed sample must not overwrite the prior gauges with healthy zeros.
	observeSendingBudgets(ctx, module, recorder)
	select {
	case <-recorder.samples:
		t.Fatal("canceled read published a snapshot")
	default:
	}
}
