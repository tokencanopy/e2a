package webhook

import (
	"context"
	"fmt"
	"net/url"
	"time"
)

// MaxDeliveryMetricsEndpoints bounds the per-endpoint breakdown.
const MaxDeliveryMetricsEndpoints = 50

// DeliveryRetention mirrors the 30-day expires_at default on
// webhook_subscriber_deliveries (migration 025) and the janitor that enforces
// it. Unlike messages — which are retained until trashed — delivery history is
// genuinely pruned, so a window reaching further back than this is reporting
// on rows that no longer exist.
const DeliveryRetention = 30 * 24 * time.Hour

// DeliveryOutcomeCounts is one population's delivery tally.
//
// The two failure buckets are what the TABLE can actually prove, which is not
// the same split the internal telemetry makes. The worker labels its metrics
// endpoint_failure vs e2a_failure, but both write status='failed' with no
// stored discriminator, so that attribution cannot be recovered here. What
// survives is whether the endpoint ANSWERED:
//
//   - EndpointRejected: the endpoint returned a non-2xx (last_status_code is
//     a real HTTP status). Unambiguously the subscriber's own response.
//   - NoResponse: no HTTP response was ever received — connect/DNS/TLS
//     failure, an SSRF-blocked URL, a delivery that expired while pending, or
//     (rarely) an e2a-side error. Predominantly an unreachable endpoint, but
//     NOT a clean "your fault" bucket, and it must not be presented as one.
type DeliveryOutcomeCounts struct {
	Total            int64
	Delivered        int64
	Pending          int64
	EndpointRejected int64
	NoResponse       int64
}

// Failed is the terminal failure count across both attributable buckets.
func (c DeliveryOutcomeCounts) Failed() int64 { return c.EndpointRejected + c.NoResponse }

// EndpointDeliveryMetrics is one subscriber endpoint's slice.
type EndpointDeliveryMetrics struct {
	WebhookID string
	// URLHost is the endpoint's host only. The full URL can carry credentials
	// in its path or query string, and a metrics payload is the last place
	// that should be re-emitted — the ID identifies the webhook well enough.
	URLHost string
	Counts  DeliveryOutcomeCounts
	// LastStatusCode is the most recent HTTP status observed for this
	// endpoint in the window, or nil when nothing ever answered. A constant
	// 405 or 401 tells a customer exactly what to fix.
	LastStatusCode *int32

	// Enabled / AutoDisabledAt / AutoDisableReason mirror the webhook's health
	// state. An endpoint e2a auto-disabled after sustained failure is the most
	// actionable fact on this whole page — it means events are being dropped
	// right now, not merely retried — so it travels with the counts rather
	// than living only on the webhooks screen.
	Enabled           bool
	AutoDisabledAt    *time.Time
	AutoDisableReason string
}

// AccountDeliveryMetrics is the account-wide webhook aggregate.
type AccountDeliveryMetrics struct {
	Totals    DeliveryOutcomeCounts
	Endpoints []EndpointDeliveryMetrics
	// EndpointsTruncated reports that the account has more endpoints with
	// traffic than Endpoints contains. Totals stay complete.
	EndpointsTruncated bool
	// EndpointsAutoDisabled counts endpoints e2a has auto-disabled. Computed
	// across every endpoint the account owns, not just the listed ones, and
	// independent of whether the endpoint had traffic in this window.
	EndpointsAutoDisabled int64

	// WindowExceedsRetention is true when the requested window starts before
	// the delivery-retention horizon, so older rows have been pruned and the
	// counts below understate that stretch. Without this a 90-day view looks
	// like a collapse in webhook volume rather than a retention boundary.
	WindowExceedsRetention bool
}

// CountDeliveriesForAccount aggregates webhook delivery outcomes for one
// account over a window.
//
// The window is anchored on the DELIVERY row's own created_at, not on the
// originating message's. Deliveries outlive their messages by design —
// message_id is ON DELETE SET NULL precisely so history survives the message
// janitor — so anchoring on the message would silently drop every delivery
// whose message had been pruned.
//
// The grain is one row per (event, subscriber) pair: an account with three
// webhooks matching the same event produces three rows for one message. These
// counts therefore exceed message counts and must never be presented as
// duplicate mail.
func (s *SubscriberStore) CountDeliveriesForAccount(ctx context.Context, userID string, start, end time.Time) (AccountDeliveryMetrics, error) {
	var metrics AccountDeliveryMetrics
	if userID == "" {
		return metrics, fmt.Errorf("webhook delivery metrics: user id required")
	}
	if !end.After(start) {
		return metrics, fmt.Errorf("webhook delivery metrics: end must be after start")
	}
	metrics.WindowExceedsRetention = start.Before(time.Now().Add(-DeliveryRetention))

	// A non-2xx status code is proof the endpoint answered; 0/NULL means
	// nothing ever did. This is the only attribution the table supports.
	const outcomeSQL = `
		count(*)::bigint,
		count(*) FILTER (WHERE d.status = 'delivered')::bigint,
		count(*) FILTER (WHERE d.status = 'pending')::bigint,
		count(*) FILTER (WHERE d.status = 'failed'
		                   AND coalesce(d.last_status_code, 0) >= 400)::bigint,
		count(*) FILTER (WHERE d.status = 'failed'
		                   AND coalesce(d.last_status_code, 0) < 400)::bigint`

	err := s.pool.QueryRow(ctx, `
		SELECT `+outcomeSQL+`
		FROM webhook_subscriber_deliveries d
		JOIN webhooks w ON w.id = d.webhook_id
		WHERE w.user_id = $1 AND d.created_at >= $2 AND d.created_at < $3
	`, userID, start, end).Scan(
		&metrics.Totals.Total, &metrics.Totals.Delivered, &metrics.Totals.Pending,
		&metrics.Totals.EndpointRejected, &metrics.Totals.NoResponse,
	)
	if err != nil {
		return metrics, fmt.Errorf("aggregate webhook delivery metrics: %w", err)
	}
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*)::bigint FROM webhooks
		WHERE user_id = $1 AND auto_disabled_at IS NOT NULL
	`, userID).Scan(&metrics.EndpointsAutoDisabled); err != nil {
		return metrics, fmt.Errorf("count auto-disabled webhooks: %w", err)
	}

	if metrics.Totals.Total == 0 {
		return metrics, nil
	}

	rows, err := s.pool.Query(ctx, `
		SELECT d.webhook_id, w.url, `+outcomeSQL+`,
		       (ARRAY_AGG(d.last_status_code ORDER BY d.last_attempt_at DESC NULLS LAST)
		          FILTER (WHERE coalesce(d.last_status_code, 0) > 0))[1],
		       w.enabled, w.auto_disabled_at, coalesce(w.auto_disable_reason, '')
		FROM webhook_subscriber_deliveries d
		JOIN webhooks w ON w.id = d.webhook_id
		WHERE w.user_id = $1 AND d.created_at >= $2 AND d.created_at < $3
		GROUP BY d.webhook_id, w.url, w.enabled, w.auto_disabled_at, w.auto_disable_reason
		ORDER BY count(*) DESC, d.webhook_id ASC
		LIMIT $4
	`, userID, start, end, MaxDeliveryMetricsEndpoints+1)
	if err != nil {
		return metrics, fmt.Errorf("aggregate webhook delivery endpoints: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var endpoint EndpointDeliveryMetrics
		var rawURL string
		var lastStatus *int32
		if err := rows.Scan(&endpoint.WebhookID, &rawURL,
			&endpoint.Counts.Total, &endpoint.Counts.Delivered, &endpoint.Counts.Pending,
			&endpoint.Counts.EndpointRejected, &endpoint.Counts.NoResponse, &lastStatus,
			&endpoint.Enabled, &endpoint.AutoDisabledAt, &endpoint.AutoDisableReason); err != nil {
			return metrics, fmt.Errorf("scan webhook delivery endpoint: %w", err)
		}
		endpoint.URLHost = hostOnly(rawURL)
		endpoint.LastStatusCode = lastStatus
		metrics.Endpoints = append(metrics.Endpoints, endpoint)
	}
	if err := rows.Err(); err != nil {
		return metrics, fmt.Errorf("iterate webhook delivery endpoints: %w", err)
	}
	if len(metrics.Endpoints) > MaxDeliveryMetricsEndpoints {
		metrics.Endpoints = metrics.Endpoints[:MaxDeliveryMetricsEndpoints]
		metrics.EndpointsTruncated = true
	}
	return metrics, nil
}

// hostOnly reduces a subscriber URL to its host. A webhook URL may carry a
// shared secret in its path or query string; the metrics payload is the last
// place that should be echoed back, and the host is all a reader needs to
// recognise which endpoint a row describes.
func hostOnly(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return ""
	}
	return parsed.Host
}
