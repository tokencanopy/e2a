package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tokencanopy/e2a/internal/identity"
)

// The account-restore interstitial (docs/design/account-soft-deletion.md
// §4.4). A sign-in that resolves to a trashed account receives a RESTRICTED
// session; these three dashboard-only routes are the only things it can do:
// read the deletion state, restore the account, or erase it now. They are
// deliberately not /v1 operations — restore is interactive only, never an API
// or a replayable machine call.

// AccountDeletionView is the interstitial's read model.
type AccountDeletionView struct {
	Email           string     `json:"email"`
	DeletedAt       *time.Time `json:"deleted_at"`
	PurgeAfter      *time.Time `json:"purge_after"`
	PurgeInProgress bool       `json:"purge_in_progress"`
}

// AccountRestoreView is the restore response.
type AccountRestoreView struct {
	Restored   bool       `json:"restored"`
	RestoredAt *time.Time `json:"restored_at"`
}

func (s *Server) registerAccountRestoreRoutes() {
	d := s.deps
	if d.RestrictedSession == nil || d.RestoreAccount == nil || d.DeleteUserData == nil {
		return
	}
	s.Router.Get("/api/account/deletion", s.handleAccountDeletionState)
	s.Router.Post("/api/account/restore", s.handleAccountRestore)
	s.Router.Post("/api/account/erase", s.handleAccountErase)
}

func (s *Server) restrictedUser(w http.ResponseWriter, r *http.Request) (*identity.User, string, bool) {
	u, token, err := s.deps.RestrictedSession(r)
	if err != nil || u == nil {
		if err != nil && !errors.Is(err, pgx.ErrNoRows) && !errors.Is(err, http.ErrNoCookie) {
			WriteError(w, r, http.StatusServiceUnavailable, "auth_unavailable", "session lookup failed; retry")
			return nil, "", false
		}
		WriteError(w, r, http.StatusUnauthorized, "unauthorized", "no account awaiting restore on this session; sign in again")
		return nil, "", false
	}
	return u, token, true
}

func writeAccountJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) handleAccountDeletionState(w http.ResponseWriter, r *http.Request) {
	u, _, ok := s.restrictedUser(w, r)
	if !ok {
		return
	}
	writeAccountJSON(w, http.StatusOK, AccountDeletionView{
		Email:           u.Email,
		DeletedAt:       u.DeletedAt,
		PurgeAfter:      u.PurgeAfter(),
		PurgeInProgress: u.PurgeClaimed,
	})
}

func (s *Server) handleAccountRestore(w http.ResponseWriter, r *http.Request) {
	u, token, ok := s.restrictedUser(w, r)
	if !ok {
		return
	}
	restored, err := s.deps.RestoreAccount(r.Context(), u.ID, token)
	switch {
	case err == nil:
		// The restricted cookie was short-lived; the upgraded session now
		// carries the ordinary lifetime, so re-issue the cookie to match.
		if s.deps.WriteSessionCookie != nil {
			s.deps.WriteSessionCookie(w, token, identity.SessionTTL)
		}
		writeAccountJSON(w, http.StatusOK, AccountRestoreView{Restored: true, RestoredAt: restored.RestoredAt})
	case errors.Is(err, identity.ErrNotInTrash):
		WriteError(w, r, http.StatusConflict, "not_in_trash", "the account is not in the trash")
	case errors.Is(err, identity.ErrPurgeInProgress):
		WriteError(w, r, http.StatusConflict, "purge_in_progress", "permanent erasure of this account has already begun and cannot be undone")
	case errors.Is(err, identity.ErrRegistrationRefused):
		WriteError(w, r, http.StatusForbidden, "registration_refused", "this account's sign-in identity is closed and the account cannot be restored")
	case errors.Is(err, identity.ErrTombstoneKeyUnavailable):
		WriteError(w, r, http.StatusServiceUnavailable, "internal_error", "restore is temporarily unavailable; retry later")
	case errors.Is(err, pgx.ErrNoRows):
		WriteError(w, r, http.StatusUnauthorized, "unauthorized", "the account no longer exists")
	default:
		WriteError(w, r, http.StatusInternalServerError, "internal_error", "failed to restore the account")
	}
}

func (s *Server) handleAccountErase(w http.ResponseWriter, r *http.Request) {
	u, _, ok := s.restrictedUser(w, r)
	if !ok {
		return
	}
	res, err := s.deps.DeleteUserData(r.Context(), u, true)
	if err != nil {
		var env *ErrorEnvelope
		if errors.As(accountDeleteError(err), &env) {
			writeRawEnvelope(w, r, env)
			return
		}
		WriteError(w, r, http.StatusInternalServerError, "internal_error", "failed to erase the account")
		return
	}
	res.Deleted = true
	if s.deps.WriteSessionCookie != nil {
		s.deps.WriteSessionCookie(w, "", -1)
	}
	writeAccountJSON(w, http.StatusOK, res)
}
