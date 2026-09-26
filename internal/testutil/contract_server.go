package testutil

import (
	"context"
	"fmt"
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
	"github.com/tokencanopy/e2a/internal/sendingpolicy"
	"github.com/tokencanopy/e2a/internal/testutil/testdb"
	"github.com/tokencanopy/e2a/internal/unsubscribe"
	"github.com/tokencanopy/e2a/internal/usage"
	"github.com/tokencanopy/e2a/internal/webhook"
	"github.com/tokencanopy/e2a/internal/webhookpub"
	"github.com/tokencanopy/e2a/internal/ws"
)

// CappedLimits are the plan caps of the secondary account described on
// ContractServer.CappedAPIKey. Caps are small enough that a scenario can
// consume the slots, be refused at the cap (402), free one, and succeed again
// — proving the limit is a cap and not an unconditional refusal — and
// deliberately DIFFERENT per resource, so no single hardcoded number can
// satisfy every assertion about the 402 envelope.
//
// Only the fields limits.Store.Upsert writes are capped here. max_webhooks,
// max_templates and max_contacts are separate columns this row does not touch,
// so the capped account still inherits their generous schema defaults; a
// future scenario covering those caps has to extend the row first.
var CappedLimits = limits.Limits{
	PlanCode:         "contract_capped",
	MaxAgents:        2,
	MaxDomains:       1,
	MaxMessagesMonth: 1,
	MaxStorageBytes:  1 << 20,
	UpgradeURL:       "https://e2a.dev/upgrade",
}

// OverCapLimits are applied to the third account (ContractServer.OverCapAPIKey)
// AFTER its domains/agents are already seeded past this cap, so every create
// attempt is refused with Current strictly greater than Limit.
var OverCapLimits = limits.Limits{
	PlanCode:         "contract_overcap",
	MaxAgents:        2,
	MaxDomains:       1,
	MaxMessagesMonth: 100000,
	MaxStorageBytes:  1 << 40,
	UpgradeURL:       "https://e2a.dev/upgrade",
}

type ContractServer struct {
	BaseURL string
	APIKey  string
	UserID  string
	// CappedAPIKey authenticates a SECOND account seeded with CappedLimits,
	// so scenarios can exercise quota enforcement without touching the
	// primary account's generous caps.
	//
	// A separate account rather than a "set the caps" scenario step: caps are
	// account-global, and the TS and Python runners silently ignore setup keys
	// they do not recognize, so a cap lowered for one scenario and restored by
	// a cleanup step that some runner skipped would 402 every scenario after
	// it. Nothing here is mutable, so nothing can leak. It also means the
	// enforcer's limits cache needs no special handling: the row is written
	// before the server accepts its first request and never changes, so there
	// is no staleness window for a scenario to race.
	CappedAPIKey string
	CappedUserID string
	// OverCapAPIKey authenticates the third account. See OverCapLimits.
	OverCapAPIKey string
	OverCapUserID string
	// RestrictedAPIKey authenticates the fourth account: the ONLY account
	// inside the external-sending-access cohort. The server enforces the
	// control with a far-future cohort cutoff and this account alone is
	// dated after it, so every other scenario keeps unrestricted sending.
	// It owns one shared-domain agent, a second sibling agent, and verified
	// owner-mailbox proof for its sign-in address.
	RestrictedAPIKey string
	RestrictedUserID string
	// DisposableTrashAPIKey and DisposableEraseAPIKey authenticate two
	// throwaway accounts that exist only to be deleted: the account-deletion
	// scenarios trash one (DELETE /v1/account) and permanently erase the
	// other (?permanent=true). Each can be deleted exactly once per server,
	// which is one contract run; no other scenario may use them.
	DisposableTrashAPIKey string
	DisposableEraseAPIKey string
	DBPool                *pgxpool.Pool
	Store                 *identity.Store
	WSHub                 *ws.Hub
	SMTPAddr              string
	httpServer            *http.Server
	httpLn                net.Listener
	smtpServer            *relay.Server
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
	// Mirrors production's boot-time call (cmd/e2a/main.go): the FK on
	// agent_identities.registered_domain needs a domains row for the shared
	// domain before any scenario can create a slug-based agent on it. The
	// migration seed only covers the hardcoded customer shared domain (see
	// EnsureSharedDomain's own doc comment), which no longer matches this
	// harness's "agents.localhost" SharedDomain.
	if err := store.EnsureSharedDomain(ctx, "agents.localhost"); err != nil {
		pool.Close()
		return nil, err
	}
	managedUnsubscribeIssuer, err := unsubscribe.NewIssuer(TestHMACSecret, "http://127.0.0.1", false, store)
	if err != nil {
		pool.Close()
		return nil, err
	}
	smtpRelay := outbound.NewSMTPRelay(&config.OutboundSMTPConfig{})
	sender := outbound.NewSender(smtpRelay, "test.e2a.dev")
	noopUsage := usage.NewNoopUsageTracker()

	// Limits/usage/webhook components the /v1 Deps bind to. These DEFAULTS are
	// generous on purpose: they apply to the primary contract account, whose
	// scenarios exercise contract shape and must never trip a quota. Quota
	// enforcement is exercised by the separate capped account seeded at the
	// bottom of this function (CappedAPIKey) — an account_limits row overrides
	// these defaults for that user alone.
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
	// The same composition production uses: a config-source gate running the
	// disabled policy (pass-through admission, every attempt still durable)
	// and the authorized submitter that refuses to dial without its token.
	//
	// External sending access is ENFORCED, with a far-future cohort cutoff:
	// only the restricted account below (dated after it) is in the cohort,
	// so every other scenario is unaffected while the restriction's contract
	// is exercised over the wire.
	sendingPolicy := sendingpolicy.DisabledPolicy()
	sendingPolicy.ExternalSendingAccess = &sendingpolicy.ExternalSendingAccessPolicy{
		Mode:                     sendingpolicy.ModeEnforce,
		AccountsCreatedAtOrAfter: ContractExternalAccessCutoff,
	}
	sendingModule := sendingpolicy.NewPolicyModule(pool, sendingpolicy.Secrets{}, sendingpolicy.PolicySourceConfig, sendingPolicy)
	var sendingGate sendingpolicy.Gate = sendingModule
	providerSubmitter := outbound.NewProviderSubmitter(smtpRelay, sendingGate)
	outboundJobs := outboundsend.NewJobs(
		outboundSendStore,
		agent.NewOutboundDeliverer(providerSubmitter),
		pool,
	).WithGate(sendingGate)
	jobsClient, err := jobs.New(pool, jobs.Config{OutboundWorkers: 1}, outboundJobs)
	if err != nil {
		pool.Close()
		return nil, err
	}
	store.SetOutboundJobCanceller(jobsClient)
	outboundJobs.SetEnqueuer(jobsClient)

	router := mux.NewRouter()
	api := agent.NewAPI(store, sender, smtpRelay, nil, noopUsage, "e2a.dev", "test.e2a.dev", "agents.localhost", "", false)
	api.SetProviderSubmitter(providerSubmitter, sendingGate)
	api.SetExternalAccess(sendingModule)
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
		SendingAccess: sendingModule,
		SMTPDomain:    "test.e2a.dev", SharedDomain: "agents.localhost",
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

	// The capped account. Seeded here, before the first request is served, so
	// its account_limits row is already in place the first time the enforcer
	// resolves it — no cache invalidation, no warm-up, no race.
	cappedUser, err := store.CreateOrGetUser(ctx, "capped@test.dev", "Contract Capped", "google-contract-capped")
	if err != nil {
		_ = smtpServer.Close()
		_ = httpServer.Shutdown(context.Background())
		_ = httpLn.Close()
		wsHub.Close()
		pool.Close()
		return nil, err
	}
	if err := limits.NewStore(pool).Upsert(ctx, cappedUser.ID, CappedLimits); err != nil {
		_ = smtpServer.Close()
		_ = httpServer.Shutdown(context.Background())
		_ = httpLn.Close()
		wsHub.Close()
		pool.Close()
		return nil, err
	}
	cappedKey, err := store.CreateAPIKey(ctx, cappedUser.ID, "contract-capped-key", nil)
	if err != nil {
		_ = smtpServer.Close()
		_ = httpServer.Shutdown(context.Background())
		_ = httpLn.Close()
		wsHub.Close()
		pool.Close()
		return nil, err
	}

	// The over-cap account: seed unlimited (maxDomains/maxAgents <= 0), then
	// apply OverCapLimits below so the downgrade lands on counts already over it.
	overCapUser, err := store.CreateOrGetUser(ctx, "overcap@test.dev", "Contract OverCap", "google-contract-overcap")
	if err != nil {
		_ = smtpServer.Close()
		_ = httpServer.Shutdown(context.Background())
		_ = httpLn.Close()
		wsHub.Close()
		pool.Close()
		return nil, err
	}
	if _, err := store.ClaimOrCreateDomain(ctx, "overcap-1.test.dev", overCapUser.ID); err != nil {
		_ = smtpServer.Close()
		_ = httpServer.Shutdown(context.Background())
		_ = httpLn.Close()
		wsHub.Close()
		pool.Close()
		return nil, err
	}
	if _, err := store.ClaimOrCreateDomain(ctx, "overcap-2.test.dev", overCapUser.ID); err != nil {
		_ = smtpServer.Close()
		_ = httpServer.Shutdown(context.Background())
		_ = httpLn.Close()
		wsHub.Close()
		pool.Close()
		return nil, err
	}
	for i := 1; i <= 3; i++ {
		agentEmail := fmt.Sprintf("overcap-bot-%d@agents.localhost", i)
		if _, err := store.CreateAgentWithLimit(ctx, agentEmail, "overcap-1.test.dev", "OverCap Bot", overCapUser.ID, 0); err != nil {
			_ = smtpServer.Close()
			_ = httpServer.Shutdown(context.Background())
			_ = httpLn.Close()
			wsHub.Close()
			pool.Close()
			return nil, err
		}
	}
	if err := limits.NewStore(pool).Upsert(ctx, overCapUser.ID, OverCapLimits); err != nil {
		_ = smtpServer.Close()
		_ = httpServer.Shutdown(context.Background())
		_ = httpLn.Close()
		wsHub.Close()
		pool.Close()
		return nil, err
	}
	overCapKey, err := store.CreateAPIKey(ctx, overCapUser.ID, "contract-overcap-key", nil)
	if err != nil {
		_ = smtpServer.Close()
		_ = httpServer.Shutdown(context.Background())
		_ = httpLn.Close()
		wsHub.Close()
		pool.Close()
		return nil, err
	}

	restrictedUser, restrictedKey, err := seedRestrictedAccount(ctx, pool, store)
	if err != nil {
		_ = smtpServer.Close()
		_ = httpServer.Shutdown(context.Background())
		_ = httpLn.Close()
		wsHub.Close()
		pool.Close()
		return nil, err
	}

	disposable := make([]string, 0, 2)
	for _, label := range []string{"trash", "erase"} {
		u, err := store.CreateOrGetUser(ctx, "disposable-"+label+"@test.dev", "Contract Disposable", "google-contract-disposable-"+label)
		if err == nil {
			var k *identity.APIKey
			k, err = store.CreateAPIKey(ctx, u.ID, "contract-disposable-"+label+"-key", nil)
			if err == nil {
				disposable = append(disposable, k.PlaintextKey)
			}
		}
		if err != nil {
			_ = smtpServer.Close()
			_ = httpServer.Shutdown(context.Background())
			_ = httpLn.Close()
			wsHub.Close()
			pool.Close()
			return nil, err
		}
	}

	return &ContractServer{
		DisposableTrashAPIKey: disposable[0],
		DisposableEraseAPIKey: disposable[1],
		RestrictedAPIKey:      restrictedKey,
		RestrictedUserID:      restrictedUser,
		BaseURL:               "http://" + httpLn.Addr().String(),
		APIKey:                key.PlaintextKey,
		UserID:                user.ID,
		CappedAPIKey:          cappedKey.PlaintextKey,
		CappedUserID:          cappedUser.ID,
		OverCapAPIKey:         overCapKey.PlaintextKey,
		OverCapUserID:         overCapUser.ID,
		DBPool:                pool,
		Store:                 store,
		WSHub:                 wsHub,
		SMTPAddr:              smtpAddr,
		httpServer:            httpServer,
		httpLn:                httpLn,
		smtpServer:            smtpServer,
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
	if err := testdb.Truncate(ctx, s.DBPool); err != nil && firstErr == nil {
		firstErr = err
	}
	s.DBPool.Close()
	return firstErr
}

// ContractExternalAccessCutoff is the contract server's external-sending-access
// cohort cutoff: far enough in the future that no ordinary account is in the
// cohort. The restricted account is dated after it.
const ContractExternalAccessCutoff = "2999-01-01T00:00:00Z"

// Synthetic fixtures of the restricted account. The owner address is
// verified; restricted-bot is the agent scenarios send from and
// restricted-peer a same-account sibling.
const (
	ContractRestrictedOwner = "restricted-owner@test.dev"
	ContractRestrictedAgent = "restricted-bot@agents.localhost"
	ContractRestrictedPeer  = "restricted-peer@agents.localhost"
)

func seedRestrictedAccount(ctx context.Context, pool *pgxpool.Pool, store *identity.Store) (string, string, error) {
	user, err := store.CreateOrGetUser(ctx, ContractRestrictedOwner, "Contract Restricted", "google-contract-restricted")
	if err != nil {
		return "", "", err
	}
	if _, err := pool.Exec(ctx, `
		UPDATE users
		   SET created_at = '3000-01-01T00:00:00Z',
		       owner_email_verified_at = now(),
		       owner_email_verified_address = lower(email),
		       owner_email_verified_source = 'google_oauth'
		 WHERE id = $1`, user.ID); err != nil {
		return "", "", err
	}
	for _, addr := range []string{ContractRestrictedAgent, ContractRestrictedPeer} {
		if _, err := store.CreateAgentWithLimit(ctx, addr, "agents.localhost", "Restricted Bot", user.ID, 0); err != nil {
			return "", "", err
		}
	}
	key, err := store.CreateAPIKey(ctx, user.ID, "contract-restricted-key", nil)
	if err != nil {
		return "", "", err
	}
	return user.ID, key.PlaintextKey, nil
}
