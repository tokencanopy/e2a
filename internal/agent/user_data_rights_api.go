package agent

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/tokencanopy/e2a/internal/auth"
	"github.com/tokencanopy/e2a/internal/identity"
)

// handleExportUserData returns a complete dump of the authenticated
// user's data as JSON. Used for the right-of-access flow (GDPR Art. 15,
// CCPA equivalent). Output is deterministic-ordered (created_at) so a
// caller diffing exports across time gets stable results.
//
// ExportUserDataCore assembles the full user-data export (store export +
// OAuth connections). HTTP-free; serves GET /v1/account/export.
func (a *API) ExportUserDataCore(ctx context.Context, userID string) (*identity.UserExport, error) {
	dump, err := a.store.ExportUserData(ctx, userID)
	if err != nil {
		return nil, err
	}
	// Append OAuth connections, if OAuth is wired. Side-call so the identity
	// package needs no oauth dependency; failure is logged, not fatal.
	if a.oauthStorage != nil {
		conns, err := a.oauthStorage.ExportConnectionsForUser(ctx, userID)
		if err != nil {
			log.Printf("[api] export oauth connections failed: user=%s err=%v", userID, err)
		} else {
			dump.OAuthConnections = make([]identity.OAuthConnectionEntry, len(conns))
			for i, c := range conns {
				dump.OAuthConnections[i] = identity.OAuthConnectionEntry{
					ClientID:   c.ClientID,
					ClientName: c.ClientName,
					AgentEmail: c.AgentEmail,
					Scope:      c.Scope,
					IssuedAt:   c.IssuedAt,
					ExpiresAt:  c.ExpiresAt,
					RevokedAt:  c.RevokedAt,
				}
			}
		}
	}
	return dump, nil
}

// DeleteUserDataCore serves DELETE /v1/account?confirm=DELETE (GDPR Art. 17 /
// CCPA right of deletion). By default it moves the account to the trash
// (identity.TrashAccount): the account is inert at once and restorable by
// signing in until purge_after, after which the janitor purges it. With
// permanent=true — or on a deployment that disabled account trash
// (trash.account_retention_days: 0) — it erases immediately
// (identity.EraseAccount), tombstones first.
//
// The billing hook is notified only after the database change commits (a
// send_in_progress refusal must not touch billing for an account that still
// exists); hook failure never blocks the completed deletion.
func (a *API) DeleteUserDataCore(ctx context.Context, user *identity.User, permanent bool) (*identity.DeleteUserDataResult, error) {
	// Count OAuth token rows BEFORE the change so the audit report is correct
	// (small benign race accepted, per the original handler).
	var oauthCounts struct {
		AuthCodes, AccessTokens, RefreshTokens int64
	}
	if a.oauthStorage != nil {
		if c, err := a.oauthStorage.CountUserOAuthRows(ctx, user.ID); err == nil {
			oauthCounts.AuthCodes = c.AuthCodes
			oauthCounts.AccessTokens = c.AccessTokens
			oauthCounts.RefreshTokens = c.RefreshTokens
		}
	}
	var (
		res *identity.DeleteUserDataResult
		err error
	)
	if permanent || !identity.AccountTrashEnabled() {
		res, err = a.store.EraseAccount(ctx, user.ID, a.domainTeardownHook)
		if err != nil {
			return nil, err
		}
		res.OAuthAuthCodesDeleted = oauthCounts.AuthCodes
		res.OAuthAccessTokensDeleted = oauthCounts.AccessTokens
		res.OAuthRefreshTokensDeleted = oauthCounts.RefreshTokens
		a.notifyBilling(ctx, user.ID, billingModePurge)
	} else {
		res, err = a.store.TrashAccount(ctx, user.ID, a.domainTeardownHook)
		if err != nil {
			return nil, err
		}
		a.notifyBilling(ctx, user.ID, billingModeTrash)
	}
	log.Printf("[api] user deleted: id=%s mode=%s removed=%+v", user.ID, res.Mode, res)
	return res, nil
}

// RestoreAccountCore serves POST /api/account/restore: it restores a trashed
// account through the restricted session the sign-in issued (upgrading that
// session to an ordinary one) and reverts the billing cancel-at-period-end.
func (a *API) RestoreAccountCore(ctx context.Context, userID, sessionToken string) (*identity.User, error) {
	u, err := a.store.RestoreAccount(ctx, userID, sessionToken)
	if err != nil {
		return nil, err
	}
	a.notifyBilling(ctx, userID, billingModeRestore)
	log.Printf("[api] user restored: id=%s", userID)
	return u, nil
}

// PurgeDeletedAccounts is the janitor's account pass: it purges trashed
// accounts past retention (tombstones and the summary first) and notifies the
// billing hook with mode "purge" for each account fully purged.
func (a *API) PurgeDeletedAccounts(ctx context.Context) (int64, error) {
	purged, err := a.store.PurgeDeletedUsers(ctx, a.domainTeardownHook)
	for _, id := range purged {
		a.notifyBilling(ctx, id, billingModePurge)
		log.Printf("[api] user purged: id=%s", id)
	}
	return int64(len(purged)), err
}

// DeleteExpiredTombstones and DeleteExpiredDeletedAccountSummaries complete
// the janitor.AccountPurger surface.
func (a *API) DeleteExpiredTombstones(ctx context.Context) (int64, error) {
	return a.store.DeleteExpiredTombstones(ctx)
}

func (a *API) DeleteExpiredDeletedAccountSummaries(ctx context.Context) (int64, error) {
	return a.store.DeleteExpiredDeletedAccountSummaries(ctx)
}

// Billing hook modes. The payload is additive over the original
// {"user_id": …} call: a sidecar that predates the mode field treats every
// call as "cancel", which is exactly right for purge.
const (
	billingModeTrash   = "trash"   // set cancel_at_period_end
	billingModeRestore = "restore" // revert cancel_at_period_end
	billingModePurge   = "purge"   // cancel now (the original behaviour)
)

func (a *API) notifyBilling(ctx context.Context, userID, mode string) {
	if a.billingHookURL == "" {
		return
	}
	if err := a.notifyBillingUserDeleted(ctx, userID, mode); err != nil {
		log.Printf("[api] billing-hook user-%s failed (continuing): user=%s err=%v", mode, userID, err)
	}
}

func (a *API) notifyBillingUserDeleted(ctx context.Context, userID, mode string) error {
	body, err := json.Marshal(map[string]string{"user_id": userID, "mode": mode})
	if err != nil {
		return err
	}
	h := hmac.New(sha256.New, []byte(a.internalAPISecret))
	h.Write(body)
	sig := hex.EncodeToString(h.Sum(nil))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.billingHookURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-E2A-Internal-Signature", sig)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		// Status code only: the sidecar's response body is third-party text that
		// would flow into logs via callers' err=%v. The sidecar logs its own errors.
		return fmt.Errorf("billing hook returned status %d", resp.StatusCode)
	}
	return nil
}

// RestrictedSession resolves the request's session cookie to a RESTRICTED
// session (one issued to a sign-in that resolved to a trashed account) and
// its trashed user. It never resolves an ordinary session.
func (a *API) RestrictedSession(r *http.Request) (*identity.User, string, error) {
	c, err := r.Cookie(auth.SessionCookieName)
	if err != nil {
		return nil, "", err
	}
	u, err := a.store.GetRestrictedSession(r.Context(), c.Value)
	if err != nil {
		return nil, "", err
	}
	return u, c.Value, nil
}

// WriteSessionCookie sets (maxAge > 0) or expires (maxAge <= 0) the
// dashboard session cookie with the same attributes the login doors use.
func (a *API) WriteSessionCookie(w http.ResponseWriter, token string, maxAge time.Duration) {
	c := &http.Cookie{
		Name:     auth.SessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   a.production,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(maxAge.Seconds()),
	}
	if maxAge <= 0 {
		c.Value = ""
		c.MaxAge = -1
	}
	http.SetCookie(w, c)
}
