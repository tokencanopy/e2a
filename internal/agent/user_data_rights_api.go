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

// handleDeleteUserData wipes the authenticated user and every record
// tied to them, in a single Postgres transaction. Used for the
// right-of-deletion flow (GDPR Art. 17, CCPA "Do Not Sell or Share").
//
// Requires `?confirm=DELETE` in the query string as a guardrail
// against accidental clicks. The HTTP method (DELETE) plus this
// confirmation matches the pattern other destructive APIs use
// (Stripe's account close, GitHub's repo delete).
//
// DeleteUserDataCore counts OAuth rows for the audit line, runs the cascading
// delete, then best-effort notifies the external billing hook and merges the
// counts. HTTP-free; serves DELETE /v1/account?confirm=DELETE.
func (a *API) DeleteUserDataCore(ctx context.Context, user *identity.User) (*identity.DeleteUserDataResult, error) {
	// Count OAuth token rows BEFORE the DELETE so the audit report is correct
	// (small benign race vs CASCADE accepted, per the original handler).
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
	res, err := a.store.DeleteUserDataTx(ctx, user.ID, a.domainTeardownHook)
	if err != nil {
		return nil, err
	}
	// Notify only after the database deletion commits. In particular, a
	// send_in_progress conflict must not cancel billing for an account that
	// still exists. Hook failure never blocks the completed right-of-deletion;
	// a reconciler catches any orphan.
	if a.billingHookURL != "" {
		if err := a.notifyBillingUserDeleted(ctx, user.ID); err != nil {
			log.Printf("[api] billing-hook user-delete failed (continuing): user=%s err=%v", user.ID, err)
		}
	}
	res.OAuthAuthCodesDeleted = oauthCounts.AuthCodes
	res.OAuthAccessTokensDeleted = oauthCounts.AccessTokens
	res.OAuthRefreshTokensDeleted = oauthCounts.RefreshTokens
	log.Printf("[api] user deleted: id=%s removed=%+v", user.ID, res)
	return res, nil
}

// notifyBillingUserDeleted HMAC-POSTs to the external billing hook so
// the user's Stripe subscription gets canceled after the OSS cascade
// commits. Caller already checked
// a.billingHookURL is non-empty.
//
// Returns an error on transport / non-204 status, but the caller logs
// + continues — we never block a user's account deletion on a
// billing-side outage. A reconciler picks up any orphaned customer
// records later.
func (a *API) notifyBillingUserDeleted(ctx context.Context, userID string) error {
	body, err := json.Marshal(map[string]string{"user_id": userID})
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
