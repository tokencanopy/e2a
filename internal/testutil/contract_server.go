package testutil

import (
	"context"
	"net"
	"net/http"
	"time"

	"github.com/gorilla/mux"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tokencanopy/e2a/internal/agent"
	"github.com/tokencanopy/e2a/internal/apiserver"
	"github.com/tokencanopy/e2a/internal/config"
	"github.com/tokencanopy/e2a/internal/idempotency"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/jobs"
	"github.com/tokencanopy/e2a/internal/limits"
	"github.com/tokencanopy/e2a/internal/outbound"
	"github.com/tokencanopy/e2a/internal/outboundsend"
	"github.com/tokencanopy/e2a/internal/relay"
	"github.com/tokencanopy/e2a/internal/unsubscribe"
	"github.com/tokencanopy/e2a/internal/usage"
	"github.com/tokencanopy/e2a/internal/webhook"
	"github.com/tokencanopy/e2a/internal/webhookpub"
	"github.com/tokencanopy/e2a/internal/ws"
)

type ContractServer struct {
	BaseURL    string
	APIKey     string
	UserID     string
	DBPool     *pgxpool.Pool
	Store      *identity.Store
	WSHub      *ws.Hub
	SMTPAddr   string
	httpServer *http.Server
	httpLn     net.Listener
	smtpServer *relay.Server
}

func StartContractServer(ctx context.Context, dbURL string) (*ContractServer, error) {
	pool, err := OpenPreparedTestDB(ctx, dbURL)
	if err != nil {
		return nil, err
	}
	if err := jobs.Migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	if err := resetRiverOperationalState(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}

	store := identity.NewStore(pool)
	managedUnsubscribeIssuer, err := unsubscribe.NewIssuer(TestHMACSecret, "http://127.0.0.1", false, store)
	if err != nil {
		pool.Close()
		return nil, err
	}
	smtpRelay := outbound.NewSMTPRelay(&config.OutboundSMTPConfig{})
	sender := outbound.NewSender(smtpRelay, "test.e2a.dev")
	noopUsage := usage.NewNoopUsageTracker()

	// Limits/usage/webhook components the /v1 Deps bind to. Caps are set
	// generously so contract scenarios exercise contract shape, not quota
	// enforcement (the 402 limit paths have dedicated httpapi unit tests).
	usageStore := usage.NewStore(pool)
	enforcer := limits.NewEnforcer(limits.NewStore(pool), usageStore, limits.Defaults{
		PlanCode: "contract_test", MaxAgents: 100000, MaxDomains: 100000,
		MaxMessagesMonth: 100000, MaxStorageBytes: 1 << 40,
	}, time.Minute)
	subscriberStore := webhook.NewSubscriberStore(pool)
	idempotencyStore := idempotency.NewStore(pool)
	outbox := webhookpub.NewOutbox(pool, webhookpub.StaticFlag(true))

	// Wire the real queue-first acceptance path, but deliberately do not start
	// workers: contract scenarios can prove accepted/scheduled persistence and
	// River enqueue semantics without submitting external email.
	outboundSendStore := agent.NewOutboundSendStore(store, outbox, noopUsage)
	store.SetScheduledSendFinalizer(outboundSendStore)
	outboundJobs := outboundsend.NewJobs(
		outboundSendStore,
		agent.NewOutboundDeliverer(sender),
		pool,
	)
	jobsClient, err := jobs.New(pool, jobs.Config{OutboundWorkers: 1}, outboundJobs)
	if err != nil {
		pool.Close()
		return nil, err
	}
	store.SetOutboundJobCanceller(jobsClient)
	outboundJobs.SetEnqueuer(jobsClient)

	router := mux.NewRouter()
	api := agent.NewAPI(store, sender, smtpRelay, nil, noopUsage, "e2a.dev", "test.e2a.dev", "agents.e2a.dev", "", false)
	api.SetIdempotencyStore(idempotencyStore)
	api.SetEnforcer(enforcer)
	api.SetUsageStore(usageStore)
	api.SetSubscriberStore(subscriberStore)
	api.SetOutbox(outbox)
	api.SetOutboundEnqueuer(outboundJobs)
	api.RegisterRoutes(router)

	wsHub := ws.NewHub()
	api.SetWebSocketHub(wsHub)
	wsHandler := ws.NewHandler(wsHub, store)

	// Wrap the legacy mux with the typed /v1 surface using the SAME builder
	// the production binary uses, so contract scenarios hit the real /v1
	// handler (and a dep prod wires but the harness forgets fails loudly here).
	v1 := apiserver.New(apiserver.Params{
		API: api, Store: store, Enforcer: enforcer, UsageStore: usageStore,
		SubscriberStore: subscriberStore, Idempotency: idempotencyStore, Pool: pool,
		SMTPDomain: "test.e2a.dev", SharedDomain: "agents.e2a.dev",
		PublicURL: "http://127.0.0.1", Production: false,
		EventsEnabled:            true,
		ManagedUnsubscribeIssuer: managedUnsubscribeIssuer,
		Legacy:                   router, WSHandle: wsHandler.ServeWithEmail,
	})

	httpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		pool.Close()
		wsHub.Close()
		return nil, err
	}

	httpServer := &http.Server{
		Handler:           v1,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		_ = httpServer.Serve(httpLn)
	}()

	smtpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = httpServer.Shutdown(context.Background())
		_ = httpLn.Close()
		pool.Close()
		wsHub.Close()
		return nil, err
	}
	smtpAddr := smtpListener.Addr().String()
	_ = smtpListener.Close()

	cfg := &config.Config{
		SMTP: config.SMTPConfig{
			ListenAddr: smtpAddr,
			Domain:     "test.e2a.dev",
		},
		Env: "development",
	}
	smtpServer := relay.NewServer(cfg, store, noopUsage, wsHub)
	go func() {
		_ = smtpServer.ListenAndServe()
	}()

	for i := 0; i < 50; i++ {
		conn, err := net.DialTimeout("tcp", smtpAddr, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	user, err := store.CreateOrGetUser(ctx, "contract@test.dev", "Contract Tester", "google-contract")
	if err != nil {
		_ = smtpServer.Close()
		_ = httpServer.Shutdown(context.Background())
		_ = httpLn.Close()
		wsHub.Close()
		pool.Close()
		return nil, err
	}
	key, err := store.CreateAPIKey(ctx, user.ID, "contract-key", nil)
	if err != nil {
		_ = smtpServer.Close()
		_ = httpServer.Shutdown(context.Background())
		_ = httpLn.Close()
		wsHub.Close()
		pool.Close()
		return nil, err
	}

	return &ContractServer{
		BaseURL:    "http://" + httpLn.Addr().String(),
		APIKey:     key.PlaintextKey,
		UserID:     user.ID,
		DBPool:     pool,
		Store:      store,
		WSHub:      wsHub,
		SMTPAddr:   smtpAddr,
		httpServer: httpServer,
		httpLn:     httpLn,
		smtpServer: smtpServer,
	}, nil
}

func (s *ContractServer) Close(ctx context.Context) error {
	var firstErr error
	if err := s.httpServer.Shutdown(ctx); err != nil && firstErr == nil {
		firstErr = err
	}
	if err := s.httpLn.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	if err := s.smtpServer.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	s.WSHub.Close()
	if err := truncateAll(ctx, s.DBPool); err != nil && firstErr == nil {
		firstErr = err
	}
	s.DBPool.Close()
	return firstErr
}
