// Package oauth wires the e2a authorization server using ory/fosite
// as the protocol layer.
//
// We delegate to fosite for all of the RFC-correctness corners:
// PKCE-S256 enforcement, redirect_uri matching (including RFC 8252
// §7.3 loopback ports), authorization-code single-use semantics with
// RFC 6749 §10.5 reuse defense, refresh-token rotation with §10.4
// chain revocation, RFC 9207 issuer identifier in authorization
// responses, RFC 6749 §5.2 error-shape (ASCII descriptions, JSON
// envelope, Cache-Control: no-store on /token), and so on. We keep
// the surface that is genuinely deployment-specific:
//
//   - Dynamic Client Registration (RFC 7591) — fosite ships a
//     stub via compose.OAuth2ClientCredentialsGrantFactory but not
//     the registration endpoint itself; we implement it.
//   - The consent UI handler — fosite hands us an
//     fosite.AuthorizeRequester, we decide based on the user's
//     session + the consent form, and either issue a code (with
//     fosite.WriteAuthorizeResponse) or reject.
//   - Auto-create-agent inside the consent flow, with the agent
//     row creation and the authorization code insert sharing one
//     pgx transaction so a partial failure doesn't leak phantom
//     agents.
//   - Discovery (RFC 8414) — we hand-roll the document because the
//     values are all deployment-static and fosite's helper inverts
//     the dependency we want.
//
// The intended call graph at request time looks like:
//
//	incoming request -> agent handler -> oauth.Provider methods
//	                                   -> fosite -> Storage (this pkg)
//	                                             -> pgxpool -> Postgres
//
// Storage implements the fosite-defined interfaces (OAuth2Storage,
// PKCERequestStorage, ClientManager, TokenRevocationStorage). The
// e2a-specific bits (agent_email binding on a session, slug auto-
// create) live in the per-endpoint handlers under internal/agent/
// and reach into the same Storage when they need to.
package oauth

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Token-prefix constants. fosite by default doesn't prefix the strings
// it issues; we wrap its strategy with one that prepends these so
// the bearer-dispatch in authenticateUser can route by prefix
// (ate2a_/rte2a_ → fosite, e2a_ → API key path).
const (
	ClientIDPrefix     = "mcp_"
	AuthCodePrefix     = "oace_"
	AccessTokenPrefix  = "ate2a_"
	RefreshTokenPrefix = "rte2a_"
)

// Storage adapts our Postgres pool to the fosite-defined storage
// interfaces. Methods land in subsequent slices; this skeleton is
// here so other packages can take a *Storage handle without circular
// dependency churn later.
type Storage struct {
	pool *pgxpool.Pool
}

// NewStorage returns a Storage bound to the given pool. Caller is
// responsible for the pool's lifecycle; this struct doesn't own it.
func NewStorage(pool *pgxpool.Pool) *Storage {
	return &Storage{pool: pool}
}

// Pool exposes the underlying pgxpool. Reserved for callers that need
// the raw pool — for example, opening a transaction that this package
// then participates in via WithTx. Prefer WithTx over direct pool use:
// it's the supported atomicity pattern across the oauth/agent
// boundary.
func (s *Storage) Pool() *pgxpool.Pool { return s.pool }

// txCtxKey is the canonical context key for a pgx.Tx that should be
// used by storage methods. Unexported so the only way to populate it
// is through WithTx; storage.go's db(ctx) is the only consumer.
type txCtxKey struct{}

// WithTx returns a derived context that routes oauth Storage method
// reads/writes through the given pgx.Tx instead of the pool. The
// consent handler is the motivating caller: it opens a transaction
// on Storage.Pool(), inserts an agent row via the agent package
// (which threads the same tx), then hands the tx-carrying context
// to fosite (which calls back into Storage methods). Storage.db(ctx)
// reads the same key, so all writes land in the one transaction.
//
// Storage.BeginTX is the inverse path: when fosite itself opens the
// tx, it uses BeginTX which delegates to WithTx — one canonical key
// regardless of who started the transaction.
func WithTx(ctx context.Context, tx pgx.Tx) context.Context {
	return context.WithValue(ctx, txCtxKey{}, tx)
}

// TxFromContext returns the pgx.Tx stashed by WithTx, if any. The
// second return is false when no tx is present (the pool will be
// used instead). Callers that need to share a transaction across
// package boundaries can use this to detect and join a parent tx.
func TxFromContext(ctx context.Context) (pgx.Tx, bool) {
	tx, ok := ctx.Value(txCtxKey{}).(pgx.Tx)
	return tx, ok
}
