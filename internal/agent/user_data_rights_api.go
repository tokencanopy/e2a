package agent

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"time"

	"github.com/tokencanopy/e2a/internal/auth"
	"github.com/tokencanopy/e2a/internal/billingnotify"
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
	erase := permanent || !identity.AccountTrashEnabled()
	if erase {
		// A paused account is refused (ErrEraseHeld) on every erase path,
		// including a plain delete on a deployment with account trash
		// disabled: there is no trash window to hold it in.
		res, err = a.store.EraseAccount(ctx, user.ID, a.domainTeardownHook)
		if err != nil {
			return nil, err
		}
	}
	if erase {
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

// Billing notifications. Purge keeps the ORIGINAL call — {"user_id"} to
// billing_hook_url, which every billing service treats as "cancel". Trash and
// restore go to a separate account-state endpoint, because a billing service
// that predates account trash cancels (and deletes the customer) on ANY call
// to the hook URL; an old service answers 404 there, which is logged and
// never blocks the trash or restore.
const (
	billingModeTrash   = "trash"   // account-state: set cancel_at_period_end
	billingModeRestore = "restore" // account-state: revert cancel_at_period_end
	billingModePurge   = "purge"   // hook URL: cancel now (the original behaviour)
)

func (a *API) notifyBilling(ctx context.Context, userID, mode string) {
	// Trash/restore notices are durable River jobs when the store's
	// account-state hook is wired (billingnotify); only purge — and a
	// deployment without the job — posts directly here.
	if mode != billingModePurge && a.store.HasAccountStateHook() {
		return
	}
	var (
		target string
		body   map[string]string
	)
	if mode == billingModePurge {
		target, body = a.billingHookURL, map[string]string{"user_id": userID}
	} else {
		target, body = a.accountStateURL(), map[string]string{"user_id": userID, "mode": mode}
	}
	if target == "" {
		return
	}
	if err := a.postBilling(ctx, target, body); err != nil {
		log.Printf("[api] billing notify (%s) failed (continuing): user=%s err=%v", mode, userID, err)
	}
}

// accountStateURL is the configured account-state endpoint, or the sibling
// path "account-state" of the billing hook URL.
func (a *API) accountStateURL() string {
	if a.billingAccountStateURL != "" {
		return a.billingAccountStateURL
	}
	if a.billingHookURL == "" {
		return ""
	}
	base, err := url.Parse(a.billingHookURL)
	if err != nil {
		return ""
	}
	return base.ResolveReference(&url.URL{Path: "account-state"}).String()
}

// postBilling HMAC-POSTs a JSON body to a billing endpoint after the account
// change commits. Any non-204 (including an old service's 404 on the
// account-state path) is returned as an error the caller logs and ignores.
func (a *API) postBilling(ctx context.Context, target string, payload map[string]string) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	h := hmac.New(sha256.New, []byte(a.internalAPISecret))
	h.Write(body)
	sig := hex.EncodeToString(h.Sum(nil))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
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
		return billingStatusError(resp.StatusCode)
	}
	return nil
}

// RestrictedSession resolves the request's session cookie to a RESTRICTED
// session (one issued to a sign-in that resolved to a trashed account) and
// its trashed user. It never resolves an ordinary session.
func (a *API) RestrictedSession(r *http.Request) (*identity.User, string, error) {
	c, err := r.Cookie(auth.RestoreSessionCookieName)
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

// SameOriginDashboardRequest is the CSRF check for the cookie-authenticated
// restore/erase routes: the request must provably come from the deployment's
// public dashboard origin (the same Origin/Referer rule logout uses).
func (a *API) SameOriginDashboardRequest(r *http.Request) bool {
	return auth.IsSameOriginRequest(r, a.publicURL)
}

// ClearRestoreSessionCookie expires the restricted-session cookie (after a
// restore re-issued an ordinary e2a_session, or after an erase).
func (a *API) ClearRestoreSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     auth.RestoreSessionCookieName,
		Value:    "",
		Path:     "/api/account/",
		HttpOnly: true,
		Secure:   a.production,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// PostAccountState is the billingnotify.Poster: it sends one trash/restore
// notice to the account-state endpoint. A 404 (an older billing service)
// maps to billingnotify.ErrNotFound so the job completes instead of retrying.
func (a *API) PostAccountState(ctx context.Context, userID, mode string) error {
	target := a.accountStateURL()
	if target == "" {
		return nil
	}
	err := a.postBilling(ctx, target, map[string]string{"user_id": userID, "mode": mode})
	var se billingStatusError
	if errors.As(err, &se) && int(se) == http.StatusNotFound {
		return billingnotify.ErrNotFound
	}
	return err
}

// billingStatusError is a non-204 billing response status.
type billingStatusError int

func (e billingStatusError) Error() string {
	return fmt.Sprintf("billing hook returned status %d", int(e))
}
