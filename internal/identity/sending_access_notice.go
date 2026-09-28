package identity

import (
	"context"
	"fmt"
)

// SendingAccessNoticeCounts are the account facts the external-sending-access
// operator notice needs beyond what GetUserByID already returns: the
// account's current plan, its sending-control state, and its live-resource
// counts. Read in one statement (plus two small bounded subqueries) only
// when the notice is being composed — never on a request's hot path.
type SendingAccessNoticeCounts struct {
	// PlanCode is "" when the account has no account_limits row (a
	// self-host default, or a brand-new account before a provisioner runs);
	// the caller displays "free" in that case.
	PlanCode string
	// SendingState is account_sending_controls.state, or "active" when the
	// account has no control row at all (the same fallback the control's
	// own default represents).
	SendingState string
	// PauseClass is account_sending_controls.pause_class. It carries
	// whatever value the row holds even when SendingState is not "paused"
	// (a class set by an earlier pause is not cleared on resume); callers
	// should only display it when SendingState == "paused".
	PauseClass string
	// LiveAgents is the count of the account's non-trashed agents.
	LiveAgents int
	// VerifiedDomains is the count of the account's domains with
	// domains.verified = true.
	VerifiedDomains int
}

// SendingAccessNoticeCounts reads the plan, sending-control state and
// live-resource counts for one account.
func (s *Store) SendingAccessNoticeCounts(ctx context.Context, userID string) (SendingAccessNoticeCounts, error) {
	var c SendingAccessNoticeCounts
	var planCode, pauseClass *string
	err := s.pool.QueryRow(ctx, `
		SELECT l.plan_code, COALESCE(c.state, 'active'), c.pause_class,
		       (SELECT count(*) FROM agent_identities WHERE user_id = u.id AND deleted_at IS NULL),
		       (SELECT count(*) FROM domains WHERE user_id = u.id AND verified)
		  FROM users AS u
		  LEFT JOIN account_limits AS l ON l.user_id = u.id
		  LEFT JOIN account_sending_controls AS c ON c.user_id = u.id
		 WHERE u.id = $1`, userID,
	).Scan(&planCode, &c.SendingState, &pauseClass, &c.LiveAgents, &c.VerifiedDomains)
	if err != nil {
		return SendingAccessNoticeCounts{}, fmt.Errorf("identity: read sending access notice counts: %w", err)
	}
	if planCode != nil {
		c.PlanCode = *planCode
	}
	if pauseClass != nil {
		c.PauseClass = *pauseClass
	}
	return c, nil
}
