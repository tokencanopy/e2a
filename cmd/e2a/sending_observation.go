package main

import (
	"context"
	"log"
	"time"

	"github.com/tokencanopy/e2a/internal/sendingpolicy"
	"github.com/tokencanopy/e2a/internal/telemetry"
)

// Run per process, not as a shared River periodic: every serving slot must
// publish its own effective policy and freshness. Never put DB I/O on /metrics.
func observeSendingBudgets(ctx context.Context, module *sendingpolicy.Module, metrics telemetry.Metrics) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		sampleCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		snapshot, err := module.BudgetSnapshot(sampleCtx)
		cancel()
		if err == nil {
			metrics.SendingBudgetSnapshot(snapshot.UsedRatio, snapshot.Generation, snapshot.ConfigMismatch, float64(snapshot.ObservedAt.Unix()))
		} else if ctx.Err() == nil {
			// DB errors may include row values; no raw query/error text in this log.
			log.Printf("[sending-protection] budget observation failed; gauges retain their last successful sample")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
