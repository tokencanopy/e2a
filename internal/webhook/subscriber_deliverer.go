package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

// SubscriberDeliverer performs the HTTP POST for a
// webhook_subscriber_deliveries row, signs the request with the
// per-webhook HMAC secret, and reports success / failure to the
// caller. Distinct from the retired legacy per-agent delivery path.
//
// Slice 1 carries only the current secret. Slice 4 will extend this
// to dual-sign during the 24h rotation grace window.
type SubscriberDeliverer struct {
	client       *http.Client
	requireHTTPS bool

	// internalSinkURL, when non-empty, is a single trusted internal sink URL
	// (the e2a-prober's /sink) that is EXEMPT from the production HTTPS +
	// SSRF-dial guards; exemptClient serves only that URL. See
	// NewSubscriberDeliverer for the safety argument.
	internalSinkURL string
	exemptClient    *http.Client
}

// NewSubscriberDeliverer constructs the deliverer with the 15s
// per-attempt timeout chosen in design decision #6.
//
// requireHTTPS gates against plaintext URLs in production. The same flag
// installs a dial-time IP guard (guardedDialControl): registration-time
// ValidateWebhookURL validates DNS once, but a hostname can re-resolve to an
// internal IP before delivery (DNS rebinding). The guard re-checks the actual
// resolved IP at connect time, closing that window. It is gated to production
// so local/CI deliveries to 127.0.0.1 still work.
//
// internalSinkURL (usually empty) names ONE trusted internal sink — the
// e2a-prober's /sink — reached over plain HTTP on an internal host. Deliveries
// to that EXACT URL bypass the HTTPS + SSRF guards via a separate exemptClient.
// This is safe because: (1) the value is server-operator config, never attacker
// input; (2) it is matched by exact string equality, so it grants access to no
// other internal address; and (3) the probe webhook that targets it is created
// by the privileged prober `seed`, not the public registration API (which
// rejects http:// + private hosts). Empty disables the exemption entirely.
func NewSubscriberDeliverer(requireHTTPS bool, internalSinkURL string) *SubscriberDeliverer {
	// Refuse redirects to prevent SSRF — same defense the legacy Deliverer
	// uses. A registered HTTPS URL that 301s to 127.0.0.1 would otherwise let
	// an attacker reach internal services. Applied to BOTH clients.
	noRedirect := func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}
	client := &http.Client{
		Timeout:       15 * time.Second,
		CheckRedirect: noRedirect,
	}
	if requireHTTPS {
		client.Transport = &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
				Control:   guardedDialControl,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		}
	}
	d := &SubscriberDeliverer{
		client:          client,
		requireHTTPS:    requireHTTPS,
		internalSinkURL: internalSinkURL,
	}
	// The exempt client keeps the no-redirect defense but uses the default
	// transport (no guardedDialControl), so it can reach the internal sink's
	// private IP. It is used ONLY for deliveries whose URL exactly equals
	// internalSinkURL.
	if internalSinkURL != "" {
		d.exemptClient = &http.Client{
			Timeout:       15 * time.Second,
			CheckRedirect: noRedirect,
		}
	}
	return d
}

// transportErrorLabel maps an HTTP-client transport error to one of a
// small set of customer-safe strings. The classification order matters:
// timeout first (a DNS lookup can time out — "timed out" is the more
// actionable fact), then DNS, then TLS, then the generic connection label.
func transportErrorLabel(err error) string {
	if errors.Is(err, ErrDisallowedWebhookIP) {
		// Deliberately does not echo the IP: it is the second line of the
		// SSRF defense, and the resolved address may be internal.
		return "delivery blocked: URL resolved to a disallowed IP"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "request timed out"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "request timed out"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "DNS resolution failed"
	}
	// crypto/tls error types are various (RecordHeaderError,
	// CertificateVerificationError, alert values…); they consistently
	// render with a "tls:" or "x509:" prefix somewhere in the chain.
	if s := err.Error(); strings.Contains(s, "tls:") || strings.Contains(s, "x509:") {
		return "TLS handshake failed"
	}
	return "connection failed"
}

// DeliveryOutcome is what the deliverer returns to the caller for
// status accounting. statusCode is 0 when there was no HTTP response
// (connection error, timeout, DNS failure).
type DeliveryOutcome struct {
	Success    bool
	StatusCode int
	Error      string
}

// Deliver performs one POST attempt. It signs the request body with
// the supplied HMAC secret in Stripe-style header format:
//
//	X-E2A-Signature: t=<unix>,v1=<hex(hmac-sha256(secret, "<t>.<body>"))>
//
// secretPrev (if non-empty) adds a second v1=... signature for the
// receiver to verify against during the 24h rotation grace window.
// Slice 1 always passes secretPrev="" (no grace logic yet); slice 4
// wires this up.
//
// 2xx responses are success. Anything else (including 3xx, since
// redirects are blocked) is a failure with the HTTP status code
// reported back. Connection errors return Success=false and StatusCode=0.
func (d *SubscriberDeliverer) Deliver(ctx context.Context, url string, body []byte, secret, secretPrev, eventType, schemaVersion string) DeliveryOutcome {
	client := d.client
	if d.internalSinkURL != "" && url == d.internalSinkURL {
		// Trusted internal sink (see NewSubscriberDeliverer): exempt from the
		// HTTPS + SSRF guards. exemptClient is always non-nil when
		// internalSinkURL is set.
		client = d.exemptClient
	} else if d.requireHTTPS && !strings.HasPrefix(url, "https://") {
		return DeliveryOutcome{Success: false, Error: "webhook URL must use HTTPS in production"}
	}

	timestamp := time.Now().Unix()
	signatureValue := buildSignatureHeader(timestamp, body, secret, secretPrev)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return DeliveryOutcome{Success: false, Error: fmt.Sprintf("build request: %v", err)}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-E2A-Signature", signatureValue)
	req.Header.Set("User-Agent", "e2a-webhooks/1")
	// Event-type + schema-version headers let a consumer route/version a delivery
	// before parsing the body (both values also live in the JSON envelope). The
	// schema version is stamped at delivery time from the current constant, so a
	// redelivery of an event stored before schema_version existed still carries it.
	if eventType != "" {
		req.Header.Set("X-E2A-Event-Type", eventType)
	}
	if schemaVersion != "" {
		req.Header.Set("X-E2A-Schema-Version", schemaVersion)
	}

	resp, err := client.Do(req)
	if err != nil {
		// NEVER store raw err.Error(): last_error is customer-facing (the
		// delivery-history API, auto_disable_reason, the health emails), and
		// Go transport errors embed internal detail — DNS failures carry the
		// resolver address ("lookup x on 127.0.0.11:53"), and with
		// ProxyFromEnvironment a configured proxy's address would appear in
		// dial errors. Map to a small fixed vocabulary; the raw detail goes
		// to the process log only.
		label := transportErrorLabel(err)
		log.Printf("[webhook-deliver] transport error (stored as %q): %v", label, err)
		return DeliveryOutcome{Success: false, Error: label}
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return DeliveryOutcome{Success: true, StatusCode: resp.StatusCode}
	}
	return DeliveryOutcome{
		Success:    false,
		StatusCode: resp.StatusCode,
		Error:      fmt.Sprintf("HTTP %d", resp.StatusCode),
	}
}

// buildSignatureHeader formats the X-E2A-Signature header value. The
// header carries one v1= signature in the normal case, and two during
// the rotation grace window (separated by ',') so receivers can verify
// with either secret.
//
// The signed string is "<t>.<body>" — Stripe's exact format. The
// timestamp prevents simple replay (receivers should check that t is
// recent; the design uses a 10-minute tolerance).
func buildSignatureHeader(timestamp int64, body []byte, secret, secretPrev string) string {
	current := signPayload(timestamp, body, secret)
	parts := []string{fmt.Sprintf("t=%d", timestamp), "v1=" + current}
	if secretPrev != "" {
		parts = append(parts, "v1="+signPayload(timestamp, body, secretPrev))
	}
	return strings.Join(parts, ",")
}

// signPayload computes hex(hmac-sha256(secret, "<t>.<body>")).
func signPayload(timestamp int64, body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%d.", timestamp)
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}
