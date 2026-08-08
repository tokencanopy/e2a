//go:build integration

package identity_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/testutil"
)

type threadResolutionSink struct {
	mu     sync.Mutex
	counts map[string]int
}

func (s *threadResolutionSink) ThreadResolution(source string, count int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.counts == nil {
		s.counts = make(map[string]int)
	}
	s.counts[source] += count
}

func (s *threadResolutionSink) count(source string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[source]
}

func TestThreadResolutionMetricsEmitOnceAfterStoreTransactionCommit(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	metrics := &threadResolutionSink{}
	store.SetThreadMetrics(metrics)
	ctx := context.Background()
	agentID := convoTestSetup(t, store, "thread-resolution-metrics-commit")

	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		savepoint, err := tx.Begin(ctx)
		if err != nil {
			return err
		}
		defer savepoint.Rollback(ctx)
		if _, err := store.CreateOutboundMessageThreadedTx(
			ctx, savepoint, "", agentID, []string{"alice@example.net"}, nil, nil,
			"Fresh", "send", "smtp", "<metric-commit@example.net>", "",
			nil, "accepted", agentID, "relay",
		); err != nil {
			return err
		}
		if err := savepoint.Commit(ctx); err != nil {
			return err
		}
		if got := metrics.count("fresh_send"); got != 0 {
			t.Errorf("fresh_send count before commit = %d, want 0", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if got := metrics.count("fresh_send"); got != 1 {
		t.Fatalf("fresh_send count after commit = %d, want 1", got)
	}
}

func TestThreadResolutionMetricsDiscardAssignmentOnRollback(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	metrics := &threadResolutionSink{}
	store.SetThreadMetrics(metrics)
	ctx := context.Background()
	agentID := convoTestSetup(t, store, "thread-resolution-metrics-rollback")
	forcedRollback := errors.New("force rollback after assignment")

	err := store.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := store.CreateOutboundMessageThreadedTx(
			ctx, tx, "", agentID, []string{"alice@example.net"}, nil, nil,
			"Fresh", "send", "smtp", "<metric-rollback@example.net>", "",
			nil, "accepted", agentID, "relay",
		); err != nil {
			return err
		}
		return forcedRollback
	})
	if !errors.Is(err, forcedRollback) {
		t.Fatalf("WithTx error = %v, want forced rollback", err)
	}
	if got := metrics.count("fresh_send"); got != 0 {
		t.Fatalf("fresh_send count after rollback = %d, want 0", got)
	}

	var persisted int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM messages WHERE provider_message_id=$1`,
		"<metric-rollback@example.net>",
	).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	if persisted != 0 {
		t.Fatalf("persisted rolled-back messages = %d, want 0", persisted)
	}
}

func TestThreadResolutionMetricsDiscardLazyAdoptionOnRollback(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	ctx := context.Background()
	agentID := convoTestSetup(t, store, "thread-resolution-metrics-lazy-rollback")

	legacy, err := store.CreateInboundMessage(
		ctx, "", agentID, "legacy@example.net", agentID,
		"<metric-lazy-rollback@example.net>", "Legacy", "", "unread", nil, nil, nil,
		false, "", nil, nil, nil, identity.InboundScreening{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE messages SET thread_id=NULL WHERE id=$1`,
		legacy.ID,
	); err != nil {
		t.Fatal(err)
	}

	metrics := &threadResolutionSink{}
	store.SetThreadMetrics(metrics)
	forcedRollback := errors.New("force rollback after lazy adoption")
	err = store.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := store.CreateOutboundMessageThreadedTx(
			ctx, tx, legacy.ID, agentID, []string{"legacy@example.net"}, nil, nil,
			"Re: Legacy", "reply", "smtp", "<metric-lazy-child@example.net>", "",
			nil, "accepted", agentID, "relay",
		); err != nil {
			return err
		}
		if got := metrics.count("lazy_legacy_anchor"); got != 0 {
			t.Errorf("lazy_legacy_anchor count before rollback = %d, want 0", got)
		}
		return forcedRollback
	})
	if !errors.Is(err, forcedRollback) {
		t.Fatalf("WithTx error = %v, want forced rollback", err)
	}
	for _, source := range []string{"api_reply", "lazy_legacy_anchor"} {
		if got := metrics.count(source); got != 0 {
			t.Errorf("%s count after rollback = %d, want 0", source, got)
		}
	}

	var parentThreadID *string
	if err := pool.QueryRow(ctx,
		`SELECT thread_id FROM messages WHERE id=$1`,
		legacy.ID,
	).Scan(&parentThreadID); err != nil {
		t.Fatal(err)
	}
	if parentThreadID != nil {
		t.Fatalf("legacy parent thread_id after rollback = %q, want NULL", *parentThreadID)
	}
}

func TestThreadResolutionMetricsIgnoreExternallyManagedTransaction(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	metrics := &threadResolutionSink{}
	store.SetThreadMetrics(metrics)
	ctx := context.Background()
	agentID := convoTestSetup(t, store, "thread-resolution-metrics-external-tx")

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := store.CreateOutboundMessageThreadedTx(
		ctx, tx, "", agentID, []string{"alice@example.net"}, nil, nil,
		"Fresh", "send", "smtp", "<metric-external-tx@example.net>", "",
		nil, "accepted", agentID, "relay",
	); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	if got := metrics.count("fresh_send"); got != 0 {
		t.Fatalf("fresh_send count for externally managed transaction = %d, want 0", got)
	}
}

func TestThreadResolutionMetricsCoverWriteAndLazyAdoptionPaths(t *testing.T) {
	pool := testutil.TestDB(t)
	store := identity.NewStore(pool)
	metrics := &threadResolutionSink{}
	store.SetThreadMetrics(metrics)
	ctx := context.Background()
	agentID := convoTestSetup(t, store, "thread-resolution-metrics")

	root, err := store.CreateInboundMessage(
		ctx, "", agentID, "alice@example.net", agentID,
		"<metric-root@example.net>", "Root", "", "unread", nil, nil, nil,
		false, "", nil, nil, nil, identity.InboundScreening{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateOutboundMessage(
		ctx, agentID, []string{"alice@example.net"}, nil, nil,
		"Fresh", "send", "smtp", "<metric-fresh@example.net>", "", nil,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateOutboundMessage(
		ctx, agentID, []string{"bob@example.net"}, nil, nil,
		"Forward", "forward", "smtp", "<metric-forward@example.net>", "", nil,
	); err != nil {
		t.Fatal(err)
	}
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		_, txErr := store.CreateOutboundMessageThreadedTx(
			ctx, tx, root.ID, agentID, []string{"alice@example.net"}, nil, nil,
			"Re: Root", "reply", "smtp", "<metric-api-reply@example.net>", "",
			nil, "accepted", agentID, "relay",
		)
		return txErr
	}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		messageID string
		evidence  identity.InboundThreadEvidence
	}{
		{
			messageID: "<metric-in-reply-to@example.net>",
			evidence: identity.InboundThreadEvidence{InReplyTo: []identity.RFCMessageIDCandidate{
				{Original: "<metric-root@example.net>", Canonical: "<metric-root@example.net>"},
			}},
		},
		{
			messageID: "<metric-references@example.net>",
			evidence: identity.InboundThreadEvidence{References: []identity.RFCMessageIDCandidate{
				{Original: "<metric-root@example.net>", Canonical: "<metric-root@example.net>"},
			}},
		},
	} {
		if err := store.WithTx(ctx, func(tx pgx.Tx) error {
			_, txErr := store.CreateInboundMessageAuthenticatedThreadedInTx(
				ctx, tx, "", agentID,
				identity.InboundAuth{HeaderFrom: "alice@example.net", EnvelopeFrom: "alice@example.net"},
				agentID, tc.messageID, "Re: Root", "", "unread", nil, false, "",
				nil, nil, nil, identity.InboundScreening{}, tc.evidence,
			)
			return txErr
		}); err != nil {
			t.Fatal(err)
		}
	}

	legacy, err := store.CreateInboundMessage(
		ctx, "", agentID, "legacy@example.net", agentID,
		"<metric-legacy@example.net>", "Legacy", "", "unread", nil, nil, nil,
		false, "", nil, nil, nil, identity.InboundScreening{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE messages SET thread_id=NULL WHERE id=$1`, legacy.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		_, txErr := store.CreateOutboundMessageThreadedTx(
			ctx, tx, legacy.ID, agentID, []string{"legacy@example.net"}, nil, nil,
			"Re: Legacy", "reply", "smtp", "<metric-lazy-reply@example.net>", "",
			nil, "accepted", agentID, "relay",
		)
		return txErr
	}); err != nil {
		t.Fatal(err)
	}

	for _, source := range []string{
		"no_anchor", "fresh_send", "forward", "api_reply",
		"rfc_in_reply_to", "rfc_references", "lazy_legacy_anchor",
	} {
		if metrics.count(source) == 0 {
			t.Errorf("resolution source %q was not counted", source)
		}
	}
}
