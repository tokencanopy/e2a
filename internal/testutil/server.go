package testutil

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/tokencanopy/e2a/internal/agent"
	"github.com/tokencanopy/e2a/internal/apiserver"
	"github.com/tokencanopy/e2a/internal/config"
	"github.com/tokencanopy/e2a/internal/emailauth"
	"github.com/tokencanopy/e2a/internal/idempotency"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/jobs"
	"github.com/tokencanopy/e2a/internal/limits"
	"github.com/tokencanopy/e2a/internal/outbound"
	"github.com/tokencanopy/e2a/internal/outboundsend"
	"github.com/tokencanopy/e2a/internal/relay"
	"github.com/tokencanopy/e2a/internal/sendingpolicy"
	"github.com/tokencanopy/e2a/internal/usage"
	"github.com/tokencanopy/e2a/internal/webhook"
	"github.com/tokencanopy/e2a/internal/webhookdelivery"
	"github.com/tokencanopy/e2a/internal/webhookpub"
	"github.com/tokencanopy/e2a/internal/ws"
)

const TestHMACSecret = "test-hmac-secret-for-testing"

// testServerOpts collects optional knobs callers can flip without
// growing the TestServer call site. Add fields here rather than new
// constructor parameters — every existing caller stays untouched.
type testServerOpts struct {
	outboundSMTPHost            string
	outboundSMTPPort            int
	outboundSMTPFromDomain      string
	outboundSMTPMessageIDDomain string
	manualJobs                  bool
	inboundAuthentication       *emailauth.Authentication
}

type TestServerOption func(*testServerOpts)

// WithOutboundSMTP wires an upstream relay for /send + HITL approve
// paths so they don't error with "outbound SMTP relay not configured".
// Tests that don't trigger outbound omit this. Pointing at Mailpit on
// localhost:1025 (started via `make docker-up`) is the typical pattern.
func WithOutboundSMTP(host string, port int, fromDomain string) TestServerOption {
	return func(o *testServerOpts) {
		o.outboundSMTPHost = host
		o.outboundSMTPPort = port
		o.outboundSMTPFromDomain = fromDomain
	}
}

// WithOutboundSMTPMessageIDDomain sets the provider Message-ID domain the
// relay appends to bare ids from the 250 response (config message_id_domain).
// Tests dial the relay at 127.0.0.1, so the SES-host derivation can never
// fire — this override is how a test exercises the qualification path the
// way production does against a real email-smtp.<region>.amazonaws.com host.
func WithOutboundSMTPMessageIDDomain(domain string) TestServerOption {
	return func(o *testServerOpts) {
		o.outboundSMTPMessageIDDomain = domain
	}
}

// WithManualJobs builds the shared River client but does NOT start it, so no
// queue is worked until the test calls StartJobs. Enqueue still works (an
// InsertTx is just a row), so an accepted send durably lands its outbound_send
// job and then SITS there — which is how a test pins the accept→submit window
// (delivery_status='accepted', provider_message_id still empty) with no sleeps
// and no race: an unstarted client has no producers, so the worker CANNOT run.
// Default (unset) keeps the production-shaped behavior every other test relies
// on: the client is started and drains the queue on its own.
func WithManualJobs() TestServerOption {
	return func(o *testServerOpts) { o.manualJobs = true }
}

// WithInboundAuthentication injects deterministic SMTP authentication
// evidence. It lets integration tests cover DMARC-pass policy paths without
// relying on mutable public DNS.
func WithInboundAuthentication(authentication *emailauth.Authentication) TestServerOption {
	return func(o *testServerOpts) { o.inboundAuthentication = authentication }
}

type E2ATestServer struct {
	HTTPServer *httptest.Server
	SMTPAddr   string
	Store      *identity.Store
	WSHub      *ws.Hub
	smtpServer *relay.Server

	// Webhooks-as-a-resource wiring (post-PR-180), now on the outbox + River
	// delivery path. The outbox is wired into both the agent API (so /send etc.
	// fire email.sent) and the SMTP server (so inbound mail fires email.received),
	// committing events to webhook_events. SubscriberStore lets tests insert /
	// inspect delivery rows. DrainAndDeliver() drives the outbox drain + River
	// DeliverWorker synchronously so tests get deterministic delivery without
	// waiting on any production tick.
	SubscriberStore *webhook.SubscriberStore

	pool          *pgxpool.Pool
	outboxWorker  *webhookpub.OutboxWorker
	deliverWorker *webhookdelivery.DeliverWorker

	// jobsClient is the shared River client. jobsStarted tracks whether it is
	// running so StartJobs is idempotent and cleanup only stops a started
	// client (see WithManualJobs).
	jobsClient  *jobs.Client
	jobsStarted bool
}

// StartJobs starts the shared River client for a server built with
// WithManualJobs, releasing every queued job to its worker. Idempotent, and a
// no-op on a server whose client is already running. Pair it with a bounded
// poll on the state the worker produces (e.g. provider_message_id becoming
// non-empty) rather than a sleep.
func (ts *E2ATestServer) StartJobs(t *testing.T, ctx context.Context) {
	t.Helper()
	if ts.jobsStarted {
		return
	}
	if err := ts.jobsClient.Start(ctx); err != nil {
		t.Fatalf("start River client: %v", err)
	}
	ts.jobsStarted = true
}

// DrainAndDeliver runs the outbox drain (webhook_events →
// webhook_subscriber_deliveries) then the River DeliverWorker over every pending
// delivery row, synchronously. It is the test-side replacement for the retired
// SubscriberRetryWorker.Tick — production drains via the OutboxWorker's LISTEN/
// poll loop and River's queue; tests drive both stages by hand for determinism.
// Per-row Work errors (retryable failures are expected in some tests, e.g. a 503
// receiver) are ignored so one failing subscriber doesn't stop the rest.
func (ts *E2ATestServer) DrainAndDeliver(ctx context.Context) {
	ts.outboxWorker.Tick(ctx)
	rows, err := ts.pool.Query(ctx,
		`SELECT id FROM webhook_subscriber_deliveries WHERE status = 'pending'`)
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		job := &river.Job[webhookdelivery.WebhookDeliverArgs]{
			JobRow: &rivertype.JobRow{
				Attempt:     1,
				MaxAttempts: webhookdelivery.MaxDeliveryAttempts,
				Kind:        webhookdelivery.WebhookDeliverArgs{}.Kind(),
			},
			Args: webhookdelivery.WebhookDeliverArgs{DeliveryID: id},
		}
		_ = ts.deliverWorker.Work(ctx, job)
	}
}

func TestServer(t *testing.T, pool *pgxpool.Pool, opts ...TestServerOption) *E2ATestServer {
	t.Helper()
	o := testServerOpts{}
	for _, opt := range opts {
		opt(&o)
	}

	store := identity.NewStore(pool)
	outboundCfg := &config.OutboundSMTPConfig{
		Host:            o.outboundSMTPHost,
		Port:            o.outboundSMTPPort,
		MessageIDDomain: o.outboundSMTPMessageIDDomain,
	}
	fromDomain := "test.e2a.dev"
	if o.outboundSMTPFromDomain != "" {
		fromDomain = o.outboundSMTPFromDomain
	}
	smtpRelay := outbound.NewSMTPRelay(outboundCfg)
	sender := outbound.NewSender(smtpRelay, fromDomain)
	sender.SetSendingStatusLookup(store)

	// Webhooks-resource (PR-180) wiring, on the outbox + River delivery path.
	// Events commit to webhook_events via the outbox; the OutboxWorker drains them
	// into webhook_subscriber_deliveries and the River DeliverWorker POSTs each
	// row. The subscriber store is what the /test + /deliveries handlers read. We
	// wire the outbox into both the API and the relay so trigger sites fire
	// events. Neither worker is started as a goroutine — tests call
	// DrainAndDeliver(ctx) directly for deterministic delivery without any tick.
	subscriberStore := webhook.NewSubscriberStore(pool)
	subscriberDeliverer := webhook.NewSubscriberDeliverer(false, "")
	outbox := webhookpub.NewOutbox(pool, webhookpub.StaticFlag(true))
	outboxWorker := webhookpub.NewOutboxWorker(pool, store)
	deliverWorker := webhookdelivery.NewDeliverWorker(subscriberStore, subscriberDeliverer, store)

	// HTTP server
	router := mux.NewRouter()
	noopUsage := usage.NewNoopUsageTracker()
	// Mirror production's canonical outbound composition root: every external
	// send is persisted and enqueued transactionally, then submitted by River.
	// Tests must never rely on the retired submit-inline fallback.
	if err := jobs.Migrate(context.Background(), pool); err != nil {
		t.Fatalf("migrate River schema: %v", err)
	}
	outboundSendStore := agent.NewOutboundSendStore(store, outbox, noopUsage)
	store.SetScheduledSendFinalizer(outboundSendStore)
	// The same composition production uses: a config-source gate running the
	// disabled policy (pass-through admission, every attempt still durable)
	// and the authorized submitter that refuses to dial without its token.
	sendingGate := sendingpolicy.NewGate(pool, sendingpolicy.Secrets{}, sendingpolicy.PolicySourceConfig, sendingpolicy.DisabledPolicy())
	outboundJobs := outboundsend.NewJobs(
		outboundSendStore,
		agent.NewOutboundDeliverer(outbound.NewProviderSubmitter(smtpRelay, sendingGate)),
		pool,
	).WithGate(sendingGate)
	jobsClient, err := jobs.New(pool, jobs.Config{OutboundWorkers: 2}, outboundJobs)
	if err != nil {
		t.Fatalf("build River client: %v", err)
	}
	store.SetOutboundJobCanceller(jobsClient)
	outboundJobs.SetEnqueuer(jobsClient)
	// Deferred under WithManualJobs so the test owns when queues start draining.
	if !o.manualJobs {
		if err := jobsClient.Start(context.Background()); err != nil {
			t.Fatalf("start River client: %v", err)
		}
	}
	usageStore := usage.NewStore(pool)
	// Generous caps — e2e exercises behavior, not quota enforcement.
	enforcer := limits.NewEnforcer(limits.NewStore(pool), usageStore, limits.Defaults{
		PlanCode: "test", MaxAgents: 100000, MaxDomains: 100000,
		MaxMessagesMonth: 100000, MaxStorageBytes: 1 << 40,
	}, time.Minute)
	idempotencyStore := idempotency.NewStore(pool)
	api := agent.NewAPI(store, sender, smtpRelay, nil, noopUsage, "e2a.dev", "test.e2a.dev", "agents.e2a.dev", "", false)
	api.SetIdempotencyStore(idempotencyStore)
	api.SetSubscriberStore(subscriberStore)
	api.SetOutbox(outbox)
	api.SetPoolForEvents(pool)
	api.SetEnforcer(enforcer)
	api.SetUsageStore(usageStore)
	api.SetOutboundEnqueuer(outboundJobs)
	api.RegisterRoutes(router)

	// WebSocket live-tail transport — wired as a /v1 route via WSHandle below.
	wsHub := ws.NewHub()
	api.SetWebSocketHub(wsHub)
	wsHandler := ws.NewHandler(wsHub, store)

	// Wrap the legacy mux with the typed /v1 surface (the same apiserver
	// builder prod + StartContractServer use) so e2e exercises the real /v1
	// handler; the remaining non-v1 routes (oauth/auth/health) fall through to the mux.
	v1 := apiserver.New(apiserver.Params{
		API: api, Store: store, Enforcer: enforcer, UsageStore: usageStore,
		SubscriberStore: subscriberStore, Idempotency: idempotencyStore, Pool: pool,
		SMTPDomain: "test.e2a.dev", SharedDomain: "agents.e2a.dev",
		PublicURL: "http://127.0.0.1", Production: false,
		EventsEnabled: true,
		Legacy:        router, WSHandle: wsHandler.ServeWithEmail,
	})

	httpServer := httptest.NewServer(v1)

	// SMTP server on random port
	smtpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen for SMTP: %v", err)
	}
	smtpAddr := smtpListener.Addr().String()
	smtpListener.Close()

	cfg := &config.Config{
		SMTP: config.SMTPConfig{
			ListenAddr: smtpAddr,
			Domain:     "test.e2a.dev",
		},
		Env: "development",
	}
	smtpServer := relay.NewServer(cfg, store, noopUsage, wsHub)
	if o.inboundAuthentication != nil {
		smtpServer.SetAuthenticationChecker(func(context.Context, net.IP, string, string, []byte, emailauth.AuthorIdentity) *emailauth.Authentication {
			return o.inboundAuthentication
		})
	}
	smtpServer.SetOutbox(outbox)

	go func() {
		if err := smtpServer.ListenAndServe(); err != nil {
			// Server closed is expected during cleanup
		}
	}()

	// Wait for SMTP server to be ready
	for i := 0; i < 50; i++ {
		conn, err := net.DialTimeout("tcp", smtpAddr, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	ts := &E2ATestServer{
		HTTPServer:      httpServer,
		SMTPAddr:        smtpAddr,
		Store:           store,
		WSHub:           wsHub,
		smtpServer:      smtpServer,
		SubscriberStore: subscriberStore,
		pool:            pool,
		outboxWorker:    outboxWorker,
		deliverWorker:   deliverWorker,
		jobsClient:      jobsClient,
		jobsStarted:     !o.manualJobs,
	}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// Stop only a started client — Stop on one that never started errors.
		if ts.jobsStarted {
			if err := jobsClient.Stop(ctx); err != nil {
				t.Errorf("stop River client: %v", err)
			}
		}
		httpServer.Close()
		smtpServer.Close()
		wsHub.Close()
	})

	return ts
}

type ReceivedPayload struct {
	Body    webhook.Payload
	Headers http.Header
	RawBody []byte
}

type WebhookReceiverResult struct {
	Server   *httptest.Server
	mu       sync.Mutex
	payloads []ReceivedPayload
}

func (w *WebhookReceiverResult) Payloads() []ReceivedPayload {
	w.mu.Lock()
	defer w.mu.Unlock()
	result := make([]ReceivedPayload, len(w.payloads))
	copy(result, w.payloads)
	return result
}

func (w *WebhookReceiverResult) WaitForPayloads(t *testing.T, count int, timeout time.Duration) []ReceivedPayload {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		payloads := w.Payloads()
		if len(payloads) >= count {
			return payloads
		}
		time.Sleep(50 * time.Millisecond)
	}
	payloads := w.Payloads()
	if len(payloads) < count {
		t.Fatalf("expected %d webhook payloads, got %d after %v", count, len(payloads), timeout)
	}
	return payloads
}

// SubscriberCaptured is one POST the SubscriberReceiver caught. The
// envelope is the parsed JSON body ({event, id, created_at, data})
// and RawBody is the verbatim bytes — useful for HMAC verification,
// which signs `t.body` and must use the exact bytes the worker POSTed.
type SubscriberCaptured struct {
	URL      string
	Envelope map[string]any
	RawBody  []byte
	Headers  http.Header
}

// SubscriberReceiverResult is a multi-path receiver for the new
// webhooks-as-a-resource path. Distinct from WebhookReceiverResult
// (which decodes the legacy webhook.Payload shape) because the new
// envelope is {event, id, created_at, data} and we need raw bytes
// for signature verification.
type SubscriberReceiverResult struct {
	Server   *httptest.Server
	mu       sync.Mutex
	captured []SubscriberCaptured
	// status is per-path: if absent, 200. Used by the auto-disable
	// test to force 503 on one route.
	statusByPath map[string]int
}

func (s *SubscriberReceiverResult) Captured() []SubscriberCaptured {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SubscriberCaptured, len(s.captured))
	copy(out, s.captured)
	return out
}

// SetStatus pins a non-200 response for the given path. Used by the
// auto-disable test to force the worker into the failure path.
func (s *SubscriberReceiverResult) SetStatus(path string, code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.statusByPath == nil {
		s.statusByPath = map[string]int{}
	}
	s.statusByPath[path] = code
}

// Reset clears captured payloads. Useful between phases of a long
// test so per-phase assertions don't see prior posts.
func (s *SubscriberReceiverResult) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.captured = nil
}

// WaitFor polls until predicate returns true or the timeout expires.
// Returns the captured list at the moment predicate first matched (or
// the last seen list on timeout). Tests typically call Tick(ctx) on
// the worker once then WaitFor(..., 0) for an immediate check; the
// timeout exists for cases where the publisher may still be in flight.
func (s *SubscriberReceiverResult) WaitFor(t *testing.T, timeout time.Duration, predicate func([]SubscriberCaptured) bool) []SubscriberCaptured {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		got := s.Captured()
		if predicate(got) {
			return got
		}
		if time.Now().After(deadline) {
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// SubscriberReceiver returns a multi-path HTTP receiver wired for the
// new webhook-resource envelope. Routes work as plain paths under the
// receiver's base URL — e.g. receiver.Server.URL + "/sent" + ".../fail".
func SubscriberReceiver(t *testing.T) *SubscriberReceiverResult {
	t.Helper()
	result := &SubscriberReceiverResult{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read err", 500)
			return
		}
		var env map[string]any
		_ = json.Unmarshal(raw, &env) // tolerate non-JSON for negative cases

		result.mu.Lock()
		result.captured = append(result.captured, SubscriberCaptured{
			URL:      r.URL.Path,
			Envelope: env,
			RawBody:  raw,
			Headers:  r.Header.Clone(),
		})
		status := 200
		if s, ok := result.statusByPath[r.URL.Path]; ok {
			status = s
		}
		result.mu.Unlock()
		w.WriteHeader(status)
	}))
	result.Server = server
	t.Cleanup(server.Close)
	return result
}

func WebhookReceiver(t *testing.T) *WebhookReceiverResult {
	t.Helper()

	result := &WebhookReceiverResult{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawBody, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read error", 500)
			return
		}
		var payload webhook.Payload
		if err := json.Unmarshal(rawBody, &payload); err != nil {
			http.Error(w, fmt.Sprintf("unmarshal error: %v", err), 400)
			return
		}

		result.mu.Lock()
		result.payloads = append(result.payloads, ReceivedPayload{
			Body:    payload,
			Headers: r.Header.Clone(),
			RawBody: rawBody,
		})
		result.mu.Unlock()

		w.WriteHeader(200)
	}))

	result.Server = server
	t.Cleanup(server.Close)

	return result
}
