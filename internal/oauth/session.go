package oauth

import (
	"time"

	"github.com/ory/fosite"
)

// Session is the e2a-specific data we attach to a fosite Requester
// for the lifetime of an authorization. fosite serializes this along
// with the rest of the Requester onto each oauth_* row; on lookup we
// hydrate it back via the caller-provided pointer.
//
// The two fields that matter beyond fosite's defaults:
//
//   - UserID: the e2a user_id this grant belongs to. fosite would
//     work with just the Subject string, but we want a typed handle
//     for the per-user revocation paths.
//   - AgentEmail: the inbox the OAuth client is connected to.
//     Tool calls can override per-call; this is the default. fosite
//     doesn't know about agents at all — this rides as session
//     extension data.
type Session struct {
	UserID       string                         `json:"user_id"`
	AgentEmail   string                         `json:"agent_email,omitempty"`
	Subject      string                         `json:"subject"`
	Username     string                         `json:"username,omitempty"`
	ExpiresAtMap map[fosite.TokenType]time.Time `json:"expires_at,omitempty"`
}

// SetExpiresAt records the expiry for a given token type. fosite calls
// this internally before persisting the session; we expose the map
// directly via the methods below so it round-trips through JSON.
func (s *Session) SetExpiresAt(key fosite.TokenType, exp time.Time) {
	if s.ExpiresAtMap == nil {
		s.ExpiresAtMap = make(map[fosite.TokenType]time.Time)
	}
	s.ExpiresAtMap[key] = exp
}

func (s *Session) GetExpiresAt(key fosite.TokenType) time.Time {
	if s.ExpiresAtMap == nil {
		return time.Time{}
	}
	return s.ExpiresAtMap[key]
}

func (s *Session) GetUsername() string { return s.Username }
func (s *Session) GetSubject() string  { return s.Subject }

// Clone returns a deep-enough copy that fosite handlers can mutate the
// returned session without affecting the original. Required by the
// fosite.Session interface contract — fosite calls Clone() before
// handing a session to a handler that may extend it.
func (s *Session) Clone() fosite.Session {
	if s == nil {
		return nil
	}
	cp := *s
	if s.ExpiresAtMap != nil {
		cp.ExpiresAtMap = make(map[fosite.TokenType]time.Time, len(s.ExpiresAtMap))
		for k, v := range s.ExpiresAtMap {
			cp.ExpiresAtMap[k] = v
		}
	}
	return &cp
}

// Compile-time check.
var _ fosite.Session = (*Session)(nil)
