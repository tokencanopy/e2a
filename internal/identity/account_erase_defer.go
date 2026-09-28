package identity

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Deferred erase for recent external senders
// (docs/design/account-soft-deletion.md, "Deferred erase for recent senders").
//
// Provider feedback — above all complaints — arrives hours to days after a
// send. An account that sends a burst and erases itself at once would take its
// sending control row and its bounce/complaint aggregates with it before that
// feedback lands, so the feedback would count against nothing. A permanent
// erase of an account that sent to an external recipient within the window is
// therefore held in the ordinary account trash instead: the account is
// deleted from the owner's point of view (inert, restorable until purge_after),
// and the janitor purges it at the normal end of the trash window while late
// feedback keeps updating its aggregates.

// RecentSenderEraseDefer is the look-back window: a permanent erase of an
// account, a permanent agent delete, or a permanent message delete whose
// most recent send to an external recipient is younger than this is
// deferred to the matching trash (account trash, agent trash, message
// trash). Deferring the agent and message purges is what keeps the account
// check sound: the evidence it reads cannot be purged on demand inside the
// window. cmd/e2a assigns it at startup from
// trash.recent_sender_erase_defer_days (default 14). Zero disables the
// deferral everywhere. With account trash disabled the account-level
// deferral does not apply (there is no account trash window); agent and
// message deferral still do (their trash window is always at least a day).
var RecentSenderEraseDefer = 14 * 24 * time.Hour

// EraseDeferredMessage is the human explanation carried on a deferred-erase
// receipt.
const EraseDeferredMessage = "This account emailed external recipients recently, so it is kept in the trash " +
	"until purge_after before it is permanently erased (late delivery feedback such as spam complaints must " +
	"still reach it). The account is already unusable; the owner can restore it by signing in before purge_after."

// sentExternallySinceSQL reports whether the account ($1) had an outbound
// message settled as sent to an external recipient at or after $2, within
// the given scope (the whole account, one agent, or one message).
//
// Source: message_recipients. A row there is written exactly when the
// provider (or relay) accepted the message — one per envelope recipient
// (to/cc/bcc), normalized — so it records real sends, never drafts, holds,
// refusals or queued mail. It is reached through the account's agents and
// idx_messages_agent_created (or the messages primary key for one message),
// and the EXISTS stops at the first hit.
//
// The send instant is the latest of created_at, provider_accepted_at,
// reviewed_at and scheduled_at (GREATEST ignores NULLs), so a scheduled or
// review-held message that was submitted recently counts even when it was
// created long ago.
//
// "External" is the external-sending-access notion: anything other than
//   - an agent of the same account (any state — by account erase time the
//     account's agents are already trashed by the account trash),
//   - the account's verified owner mailbox (valid proof for its CURRENT
//     email, as sendingpolicy.ownerRecipientVerified), or
//   - an address on one of the deployment's shared agent domains (the
//     verified domains rows with no owning account).
func sentExternallySinceSQL(scope string) string {
	return `
SELECT EXISTS (
    SELECT 1
      FROM agent_identities AS a
      JOIN messages AS m ON m.agent_id = a.id
      JOIN message_recipients AS r ON r.message_id = m.id
      JOIN users AS u ON u.id = a.user_id
     WHERE a.user_id = $1` + scope + `
       AND m.direction = 'outbound'
       AND GREATEST(m.created_at, m.provider_accepted_at, m.reviewed_at, m.scheduled_at) >= $2
       AND NOT EXISTS (
           SELECT 1 FROM agent_identities AS own
            WHERE own.user_id = $1 AND lower(own.id) = r.address)
       AND NOT (u.owner_email_verified_at IS NOT NULL
                AND u.owner_email_verified_address IS NOT NULL
                AND u.owner_email_verified_address = lower(btrim(u.email))
                AND r.address = u.owner_email_verified_address)
       AND NOT EXISTS (
           SELECT 1 FROM domains AS d
            WHERE d.user_id IS NULL AND d.verified
              AND d.domain = split_part(r.address, '@', 2)))`
}

var (
	accountSentExternallySinceSQL = sentExternallySinceSQL("")
	agentSentExternallySinceSQL   = sentExternallySinceSQL("\n       AND a.id = $3")
	messageSentExternallySinceSQL = sentExternallySinceSQL("\n       AND m.id = $3")
)

func sentExternallySince(ctx context.Context, q rowQuerier, query string, args ...any) (bool, error) {
	var sent bool
	if err := q.QueryRow(ctx, query, args...).Scan(&sent); err != nil {
		return false, fmt.Errorf("erase: recent external send: %w", err)
	}
	return sent, nil
}

// AccountSentExternallySince reports whether the account sent to at least one
// external recipient at or after since (see sentExternallySinceSQL).
func (s *Store) AccountSentExternallySince(ctx context.Context, userID string, since time.Time) (bool, error) {
	return sentExternallySince(ctx, s.pool, accountSentExternallySinceSQL, userID, since)
}

// eraseDeferralCutoff returns the start of the look-back window and whether
// the deferral is enabled at all.
func eraseDeferralCutoff() (time.Time, bool) {
	if RecentSenderEraseDefer <= 0 {
		return time.Time{}, false
	}
	return time.Now().Add(-RecentSenderEraseDefer), true
}

// agentEraseDeferredTx reports whether a permanent delete of the agent must
// be deferred to the agent trash: the agent sent to an external recipient
// inside the window. Deferring here is what keeps the account-level check
// sound: the evidence it reads cannot be purged on demand inside the window.
func agentEraseDeferredTx(ctx context.Context, q rowQuerier, userID, agentID string) (bool, error) {
	since, ok := eraseDeferralCutoff()
	if !ok {
		return false, nil
	}
	return sentExternallySince(ctx, q, agentSentExternallySinceSQL, userID, since, agentID)
}

// messageEraseDeferredTx is agentEraseDeferredTx for one message.
func messageEraseDeferredTx(ctx context.Context, q rowQuerier, userID, messageID string) (bool, error) {
	since, ok := eraseDeferralCutoff()
	if !ok {
		return false, nil
	}
	return sentExternallySince(ctx, q, messageSentExternallySinceSQL, userID, since, messageID)
}

// Human explanations carried on deferred agent and message delete receipts.
const (
	AgentEraseDeferredMessage = "This agent emailed external recipients recently, so it was moved to the trash instead " +
		"of being deleted permanently now (late delivery feedback such as spam complaints must still reach it). It is " +
		"purged at purge_after and can be restored until then."
	MessageEraseDeferredMessage = "This message was sent to external recipients recently, so it stays in the trash " +
		"instead of being deleted permanently now (late delivery feedback such as spam complaints must still reach it). " +
		"It is purged at purge_after and can be restored until then."
)

// ErrPurgeDeferred is returned by the int-returning purge wrappers
// (DeleteAgent, DeleteAgentIncarnation, PurgeMessage) when the purge was
// deferred to the trash instead: nothing was deleted. Callers that need the
// deferred receipt use PermanentDeleteAgentIncarnation / PurgeMessageOrDefer.
var ErrPurgeDeferred = errors.New("identity: permanent deletion deferred to the trash (recent external send)")

// eraseDeferralApplies reports whether a permanent erase of the account must
// be deferred to the trash: the deferral is enabled, the deployment has
// account trash, and the account sent externally inside the window.
func (s *Store) eraseDeferralApplies(ctx context.Context, userID string) (bool, error) {
	since, ok := eraseDeferralCutoff()
	if !ok || !AccountTrashEnabled() {
		return false, nil
	}
	return s.AccountSentExternallySince(ctx, userID, since)
}
