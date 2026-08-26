package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/ory/fosite"
	"github.com/ory/fosite/handler/oauth2"
	"github.com/ory/fosite/handler/pkce"
	fositestorage "github.com/ory/fosite/storage"
)

// Compile-time interface assertions: *Storage must satisfy every
// fosite storage interface we plug into compose.Compose. If fosite
// adds methods between versions, the build fails here rather than at
// runtime when a /token request comes in.
var (
	_ fosite.Storage                = (*Storage)(nil)
	_ fosite.ClientManager          = (*Storage)(nil)
	_ oauth2.CoreStorage            = (*Storage)(nil)
	_ oauth2.AuthorizeCodeStorage   = (*Storage)(nil)
	_ oauth2.AccessTokenStorage     = (*Storage)(nil)
	_ oauth2.RefreshTokenStorage    = (*Storage)(nil)
	_ oauth2.TokenRevocationStorage = (*Storage)(nil)
	_ pkce.PKCERequestStorage       = (*Storage)(nil)
	_ fositestorage.Transactional   = (*Storage)(nil)
)

// ───────────────────────── Transactional ─────────────────────────
//
// fosite's auth-code-exchange and refresh-rotation flows call
// storage.MaybeBeginTx / MaybeCommitTx so the (invalidate-code +
// issue-access + issue-refresh) sequence or the (rotate-refresh +
// revoke-paired-access) sequence runs atomically. Storages that
// don't implement Transactional get no-ops — fatal: a crash between
// statements leaves the DB in a half-issued state.
//
// We stash the pgx.Tx on the context. Every storage method routes
// its query through db(ctx) which prefers the tx when present and
// falls back to the pool otherwise. The tx is reused across the
// whole flow; Commit/Rollback are called by fosite's handler at the
// end.

// dbExecutor is the subset of pgxpool.Pool and pgx.Tx that the
// storage methods touch. Lets db(ctx) return either without forcing
// callers to type-switch.
type dbExecutor interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// db returns the active transaction if one is on the context (set by
// BeginTX or by an out-of-package caller via WithTx), otherwise the
// pool. Every read/write in this file must route through this so
// transactional flows — fosite-driven or cross-package — are atomic.
func (s *Storage) db(ctx context.Context) dbExecutor {
	if tx, ok := TxFromContext(ctx); ok {
		return tx
	}
	return s.pool
}

// BeginTX starts a new pgx transaction and returns a context carrying
// it. fosite's handlers pass this context to every subsequent storage
// call until Commit or Rollback. Uses WithTx so the key is the same
// one cross-package callers (e.g. the consent handler) use to thread
// their own transactions through.
func (s *Storage) BeginTX(ctx context.Context) (context.Context, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return ctx, err
	}
	return WithTx(ctx, tx), nil
}

func (s *Storage) Commit(ctx context.Context) error {
	tx, ok := TxFromContext(ctx)
	if !ok {
		// No tx on context — caller didn't BeginTX, or already
		// committed. Treat as no-op (matches Memory store's behavior;
		// fosite's MaybeCommitTx never reaches here without a tx).
		return nil
	}
	return tx.Commit(ctx)
}

func (s *Storage) Rollback(ctx context.Context) error {
	tx, ok := TxFromContext(ctx)
	if !ok {
		return nil
	}
	return tx.Rollback(ctx)
}

// ───────────────────────── ClientManager ─────────────────────────

// GetClient loads the client by ID. Returns fosite.ErrInvalidClient
// when the row doesn't exist so fosite's error mapping can produce the
// right RFC 6749 §5.2 error code at the endpoint.
func (s *Storage) GetClient(ctx context.Context, id string) (fosite.Client, error) {
	var c Client
	c.ID = id
	var secretHash *string
	err := s.db(ctx).QueryRow(ctx, `
		SELECT client_name, redirect_uris, grant_types, response_types,
		       scopes, audiences, token_endpoint_auth_method,
		       client_secret_hash, public
		FROM oauth_clients WHERE client_id = $1
	`, id).Scan(
		&c.Name, &c.RedirectURIs, &c.GrantTypeStrings, &c.ResponseTypeStrings,
		&c.ScopeStrings, &c.AudienceStrings, &c.TokenEndpointAuthMethodS,
		&secretHash, &c.Public,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fosite.ErrInvalidClient
	}
	if err != nil {
		return nil, err
	}
	if secretHash != nil {
		c.SecretHash = []byte(*secretHash)
	}
	return &c, nil
}

// ClientAssertionJWTValid is for the JWT-Bearer client authentication
// method (RFC 7523). We don't support that auth method — public
// clients only, no JWT assertions. Returning a non-nil error here is
// defense in depth: if a future compose misconfiguration ever enables
// the JWT-Bearer factory, this storage call will refuse rather than
// silently accept every JTI as valid. The /token endpoint will still
// reject the auth method earlier; this is the second line.
func (s *Storage) ClientAssertionJWTValid(ctx context.Context, jti string) error {
	return errors.New("oauth: jwt-bearer client auth not supported")
}

func (s *Storage) SetClientAssertionJWT(ctx context.Context, jti string, exp time.Time) error {
	return errors.New("oauth: jwt-bearer client auth not supported")
}

// ───────────────────────── AuthorizeCodeStorage ─────────────────────────

// persistedRequest is what we serialize into the `request` JSONB
// column. Mirrors fosite.Request minus the session, which we round-trip
// via the caller-provided pointer in GetXxxSession.
type persistedRequest struct {
	ID                string              `json:"id"`
	RequestedAt       time.Time           `json:"requested_at"`
	ClientID          string              `json:"client_id"`
	RequestedScope    []string            `json:"requested_scope"`
	GrantedScope      []string            `json:"granted_scope"`
	Form              map[string][]string `json:"form"`
	RequestedAudience []string            `json:"requested_audience"`
	GrantedAudience   []string            `json:"granted_audience"`
	Session           json.RawMessage     `json:"session"`
}

// marshalRequest serializes the fosite.Requester sans-session-instance
// for persistence. The session bytes are stashed alongside; on lookup
// we hand them to the caller's session pointer (see hydrate).
func marshalRequest(req fosite.Requester) ([]byte, error) {
	sessBytes, err := json.Marshal(req.GetSession())
	if err != nil {
		return nil, fmt.Errorf("marshal session: %w", err)
	}
	p := persistedRequest{
		ID:                req.GetID(),
		RequestedAt:       req.GetRequestedAt(),
		ClientID:          req.GetClient().GetID(),
		RequestedScope:    []string(req.GetRequestedScopes()),
		GrantedScope:      []string(req.GetGrantedScopes()),
		Form:              req.GetRequestForm(),
		RequestedAudience: []string(req.GetRequestedAudience()),
		GrantedAudience:   []string(req.GetGrantedAudience()),
		Session:           sessBytes,
	}
	return json.Marshal(p)
}

// hydrate decodes a persisted request row into a fresh fosite.Request,
// loading the linked Client from oauth_clients and unmarshaling the
// session bytes into the caller-provided session value.
func (s *Storage) hydrate(ctx context.Context, raw []byte, session fosite.Session) (fosite.Requester, error) {
	var p persistedRequest
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("unmarshal request: %w", err)
	}
	if len(p.Session) > 0 && session != nil {
		if err := json.Unmarshal(p.Session, session); err != nil {
			return nil, fmt.Errorf("unmarshal session: %w", err)
		}
	}
	client, err := s.GetClient(ctx, p.ClientID)
	if err != nil {
		return nil, fmt.Errorf("hydrate client: %w", err)
	}
	r := &fosite.Request{
		ID:                p.ID,
		RequestedAt:       p.RequestedAt,
		Client:            client,
		RequestedScope:    fosite.Arguments(p.RequestedScope),
		GrantedScope:      fosite.Arguments(p.GrantedScope),
		Form:              p.Form,
		RequestedAudience: fosite.Arguments(p.RequestedAudience),
		GrantedAudience:   fosite.Arguments(p.GrantedAudience),
		Session:           session,
	}
	return r, nil
}

func (s *Storage) CreateAuthorizeCodeSession(ctx context.Context, code string, request fosite.Requester) error {
	raw, err := marshalRequest(request)
	if err != nil {
		return err
	}
	sess, _ := request.GetSession().(*Session)
	userID := ""
	if sess != nil {
		userID = sess.UserID
	}
	// Honor fosite's session-supplied expiry — it reflects the
	// AuthorizeCodeLifespan from the compose config. Falling back to a
	// hardcoded value here would let the retention reaper truncate
	// codes fosite still considers valid (different lifetimes on each
	// side of the HMAC). Default is fosite's 15min if the caller
	// somehow didn't set one.
	expiresAt := request.GetRequestedAt().Add(15 * time.Minute)
	if sess != nil {
		if t := sess.GetExpiresAt(fosite.AuthorizeCode); !t.IsZero() {
			expiresAt = t
		}
	}
	_, err = s.db(ctx).Exec(ctx, `
		INSERT INTO oauth_auth_codes
		    (signature, request_id, client_id, user_id, request,
		     requested_at, expires_at, active)
		VALUES ($1, $2, $3, $4, $5, $6, $7, TRUE)
	`, code, request.GetID(), request.GetClient().GetID(), userID,
		raw, request.GetRequestedAt(), expiresAt,
	)
	return err
}

func (s *Storage) GetAuthorizeCodeSession(ctx context.Context, code string, session fosite.Session) (fosite.Requester, error) {
	var raw []byte
	var active bool
	err := s.db(ctx).QueryRow(ctx, `
		SELECT request, active FROM oauth_auth_codes WHERE signature = $1
	`, code).Scan(&raw, &active)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fosite.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	req, err := s.hydrate(ctx, raw, session)
	if err != nil {
		return nil, err
	}
	if !active {
		// Reuse detection: the row exists but was invalidated. fosite's
		// contract is to return BOTH the request and ErrInvalidatedAuthorizeCode
		// so the caller can revoke downstream tokens (RFC 6749 §10.5).
		return req, fosite.ErrInvalidatedAuthorizeCode
	}
	return req, nil
}

// InvalidateAuthorizeCodeSession marks the code consumed via a CAS
// `WHERE signature = $1 AND active = TRUE`. The CAS is load-bearing:
// without it, two concurrent token exchanges at READ COMMITTED can
// each pass GetAuthorizeCodeSession (both see active=TRUE before
// either UPDATE lands) and both InvalidateAuthorizeCodeSession calls
// succeed against the same row — letting fosite issue two token pairs
// for one consent (RFC 6749 §10.5 violation). Adding active=TRUE to
// the WHERE makes the second UPDATE re-evaluate the predicate after
// the lock releases and return zero rows. Returning
// ErrInvalidatedAuthorizeCode triggers fosite's deferred rollback in
// flow_authorize_code_token.go.
func (s *Storage) InvalidateAuthorizeCodeSession(ctx context.Context, code string) error {
	tag, err := s.db(ctx).Exec(ctx,
		`UPDATE oauth_auth_codes SET active = FALSE WHERE signature = $1 AND active = TRUE`, code)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fosite.ErrInvalidatedAuthorizeCode
	}
	return nil
}

// ───────────────────────── AccessTokenStorage ─────────────────────────

func (s *Storage) CreateAccessTokenSession(ctx context.Context, signature string, request fosite.Requester) error {
	raw, err := marshalRequest(request)
	if err != nil {
		return err
	}
	sess, _ := request.GetSession().(*Session)
	userID := ""
	expiresAt := request.GetRequestedAt().Add(time.Hour) // fosite default
	if sess != nil {
		userID = sess.UserID
		if t := sess.GetExpiresAt(fosite.AccessToken); !t.IsZero() {
			expiresAt = t
		}
	}
	_, err = s.db(ctx).Exec(ctx, `
		INSERT INTO oauth_access_tokens
		    (signature, request_id, client_id, user_id, request,
		     requested_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, signature, request.GetID(), request.GetClient().GetID(), userID,
		raw, request.GetRequestedAt(), expiresAt,
	)
	return err
}

func (s *Storage) GetAccessTokenSession(ctx context.Context, signature string, session fosite.Session) (fosite.Requester, error) {
	var raw []byte
	var revokedAt *time.Time
	err := s.db(ctx).QueryRow(ctx, `
		SELECT request, revoked_at FROM oauth_access_tokens WHERE signature = $1
	`, signature).Scan(&raw, &revokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fosite.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	req, err := s.hydrate(ctx, raw, session)
	if err != nil {
		return nil, err
	}
	if revokedAt != nil {
		return req, fosite.ErrInactiveToken
	}
	return req, nil
}

func (s *Storage) DeleteAccessTokenSession(ctx context.Context, signature string) error {
	_, err := s.db(ctx).Exec(ctx,
		`DELETE FROM oauth_access_tokens WHERE signature = $1`, signature)
	return err
}

// ───────────────────────── RefreshTokenStorage ─────────────────────────

func (s *Storage) CreateRefreshTokenSession(ctx context.Context, signature, accessSignature string, request fosite.Requester) error {
	// accessSignature is intentionally ignored — RotateRefreshToken
	// cascades via request_id (the fosite-managed grouping ID) which
	// is also what RevokeAccessToken keys on. Storing the paired
	// signature would just duplicate that linkage with no benefit.
	_ = accessSignature

	raw, err := marshalRequest(request)
	if err != nil {
		return err
	}
	sess, _ := request.GetSession().(*Session)
	userID := ""
	if sess != nil {
		userID = sess.UserID
	}
	// fosite signals "no expiry" by leaving the session ExpiresAt at
	// zero (the RefreshTokenLifespan=-1 config path). Persist NULL in
	// that case so the retention reaper — which must skip NULLs — keeps
	// the row forever as the operator asked. Otherwise use the session
	// value, which already reflects the configured lifetime.
	//
	// On configured-lifetime deployments, fosite's refresh strategy
	// always sets a non-zero session expiry before this method is
	// called; a zero value here is unambiguous opt-in.
	var expiresAt *time.Time
	if sess != nil {
		if t := sess.GetExpiresAt(fosite.RefreshToken); !t.IsZero() {
			expiresAt = &t
		}
	}
	_, err = s.db(ctx).Exec(ctx, `
		INSERT INTO oauth_refresh_tokens
		    (signature, request_id, client_id, user_id,
		     request, requested_at, expires_at, active)
		VALUES ($1, $2, $3, $4, $5, $6, $7, TRUE)
	`, signature, request.GetID(), request.GetClient().GetID(), userID,
		raw, request.GetRequestedAt(), expiresAt,
	)
	return err
}

func (s *Storage) GetRefreshTokenSession(ctx context.Context, signature string, session fosite.Session) (fosite.Requester, error) {
	var raw []byte
	var active bool
	var revokedAt *time.Time
	err := s.db(ctx).QueryRow(ctx, `
		SELECT request, active, revoked_at FROM oauth_refresh_tokens WHERE signature = $1
	`, signature).Scan(&raw, &active, &revokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fosite.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	req, err := s.hydrate(ctx, raw, session)
	if err != nil {
		return nil, err
	}
	if !active || revokedAt != nil {
		// fosite contract: return both the Requester (so caller can
		// extract the request_id for chain revoke) and ErrInactiveToken.
		return req, fosite.ErrInactiveToken
	}
	return req, nil
}

func (s *Storage) DeleteRefreshTokenSession(ctx context.Context, signature string) error {
	_, err := s.db(ctx).Exec(ctx,
		`DELETE FROM oauth_refresh_tokens WHERE signature = $1`, signature)
	return err
}

// RotateRefreshToken marks the refresh row inactive (via CAS) and
// ALSO revokes every access token for the same request_id. Without
// the cascade, an attacker who captured the pre-refresh access token
// could continue using it for up to its remaining lifetime (~1h)
// after the legitimate client rotated. The convention is set by
// fosite's reference in-memory store; we match it.
//
// The CAS on the refresh row (`AND active = TRUE`) defends against
// concurrent refresh exchanges: same shape as
// InvalidateAuthorizeCodeSession. Returning ErrInactiveToken when
// the predicate doesn't match drives fosite into the
// handleRefreshTokenReuse path (flow_refresh.go:178), which rolls the
// in-progress exchange back.
//
// Runs through db(ctx) so when fosite wraps this call in
// MaybeBeginTx the two UPDATEs land in the same transaction.
func (s *Storage) RotateRefreshToken(ctx context.Context, requestID, refreshTokenSignature string) error {
	db := s.db(ctx)
	tag, err := db.Exec(ctx,
		`UPDATE oauth_refresh_tokens SET active = FALSE WHERE signature = $1 AND active = TRUE`,
		refreshTokenSignature,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fosite.ErrInactiveToken
	}
	_, err = db.Exec(ctx,
		`UPDATE oauth_access_tokens SET revoked_at = NOW() WHERE request_id = $1 AND revoked_at IS NULL`,
		requestID,
	)
	return err
}

// ───────────────────────── TokenRevocationStorage ─────────────────────────

func (s *Storage) RevokeRefreshToken(ctx context.Context, requestID string) error {
	_, err := s.db(ctx).Exec(ctx, `
		UPDATE oauth_refresh_tokens
		SET revoked_at = NOW(), active = FALSE
		WHERE request_id = $1 AND revoked_at IS NULL
	`, requestID)
	return err
}

func (s *Storage) RevokeAccessToken(ctx context.Context, requestID string) error {
	_, err := s.db(ctx).Exec(ctx, `
		UPDATE oauth_access_tokens
		SET revoked_at = NOW()
		WHERE request_id = $1 AND revoked_at IS NULL
	`, requestID)
	return err
}

// ───────────────────────── PKCERequestStorage ─────────────────────────

func (s *Storage) CreatePKCERequestSession(ctx context.Context, signature string, request fosite.Requester) error {
	raw, err := marshalRequest(request)
	if err != nil {
		return err
	}
	// Mirror the auth-code expiry source. PKCE sessions are paired
	// with codes (consumed at the same /token call) so using the same
	// expiry keeps them in lockstep — the reaper won't drop the PKCE
	// row out from under a still-valid code.
	sess, _ := request.GetSession().(*Session)
	expiresAt := request.GetRequestedAt().Add(15 * time.Minute)
	if sess != nil {
		if t := sess.GetExpiresAt(fosite.AuthorizeCode); !t.IsZero() {
			expiresAt = t
		}
	}
	_, err = s.db(ctx).Exec(ctx, `
		INSERT INTO oauth_pkce_requests (signature, request_id, client_id, request, expires_at)
		VALUES ($1, $2, $3, $4, $5)
	`, signature, request.GetID(), request.GetClient().GetID(), raw, expiresAt)
	return err
}

func (s *Storage) GetPKCERequestSession(ctx context.Context, signature string, session fosite.Session) (fosite.Requester, error) {
	var raw []byte
	err := s.db(ctx).QueryRow(ctx,
		`SELECT request FROM oauth_pkce_requests WHERE signature = $1`,
		signature).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fosite.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return s.hydrate(ctx, raw, session)
}

func (s *Storage) DeletePKCERequestSession(ctx context.Context, signature string) error {
	_, err := s.db(ctx).Exec(ctx,
		`DELETE FROM oauth_pkce_requests WHERE signature = $1`, signature)
	return err
}
