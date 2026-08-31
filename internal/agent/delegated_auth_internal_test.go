package agent

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestIsDelegatedRequestCancellation(t *testing.T) {
	t.Run("active request preserves store cancellation as a real failure", func(t *testing.T) {
		if isDelegatedRequestCancellation(context.Background(), context.Canceled) {
			t.Fatal("active request must not suppress a store-originated cancellation")
		}
	})

	t.Run("client cancellation is suppressed", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if !isDelegatedRequestCancellation(ctx, fmt.Errorf("lookup: %w", context.Canceled)) {
			t.Fatal("wrapped client cancellation was not recognized")
		}
	})

	t.Run("request deadline remains an availability failure", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(context.Background(), time.Unix(1, 0))
		defer cancel()
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("ctx.Err() = %v, want deadline exceeded", ctx.Err())
		}
		if isDelegatedRequestCancellation(ctx, fmt.Errorf("lookup: %w", context.DeadlineExceeded)) {
			t.Fatal("request deadline must not be suppressed as a caller disconnect")
		}
	})

	t.Run("unrelated store error still records", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if isDelegatedRequestCancellation(ctx, errors.New("database unavailable")) {
			t.Fatal("unrelated store error must not be suppressed")
		}
	})
}
