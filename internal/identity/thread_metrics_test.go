//go:build integration

package identity_test

import (
	"context"
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
