package identity

import (
	"context"
	"errors"
	"fmt"
	"strings"
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

// EraseDeferExemptDomains are recipient domains that never count as
// external: the deployment's shared agent domain(s) by name — matched even
// when a shared domain's domains row has been adopted by an account (e.g. the
// probe account) — and provider test domains such as the SES mailbox
// simulator, which the standing prober and the e2e harness send to on every
// run. cmd/e2a assigns it at startup from shared_domain plus
// trash.erase_defer_exempt_domains (default [simulator.amazonses.com]).
// Entries are lower-case domain names.
var EraseDeferExemptDomains = []string{"simulator.amazonses.com"}

// eraseDeferExemptClasses are the account classes never deferred: synthetic
// probe traffic and internal dogfooding (the same classes sendingpolicy
// exempts from sending budgets).
const eraseDeferExemptClassesSQL = `('system', 'internal')`

// EraseDeferRetryLag bounds how long after its anchor (created_at, or the
// scheduled/approval instant for a scheduled or review-held message) a
// message can still be submitted to the provider. The send worker's longest
// finite hold is outboundsend.PolicyBudgetHoldHorizon (7 days, measured from
// that anchor; every hold class promotes to it and nothing moves it later),
// after which the message fails terminally. The worker derives that deadline
// from the constant, never from the runtime policy's budget_hold_max_days, so
// the constant is the true bound. This lag is that bound plus a week of
// margin for in-flight retries; TestEraseDeferRetryLagCoversTheLongestHold
// (internal/outboundsend) fails if the hold horizon ever outgrows it. It is
// the lower bound that lets the recent-send lookup range-scan its indexes
// instead of every outbound message of the agent.
const EraseDeferRetryLag = 14 * 24 * time.Hour

// sentExternallySinceSQL reports whether the account ($1) sent to an external
// recipient at or after $2 within the given scope (the whole account, one
// agent: agentScope, or one message: messageScope). $3 is the exempt domain
// list, $4 is $2 minus EraseDeferRetryLag, and $5 the scope id.
//
// Sends considered (per agent of the account, outbound only):
//   - arm A: created at or after $4 (a bounded range on
//     idx_messages_agent_created), whose send instant — the latest of
//     created_at, provider_accepted_at, reviewed_at, scheduled_at and
//     send_claimed_at — is at or after $2;
//   - arm B: an older scheduled or review-held message, found through the
//     partial expression index idx_messages_agent_delayed_outbound on
//     (agent_id, GREATEST(scheduled_at, reviewed_at)) bounded below by $4 (a
//     hold's TTL and a schedule's horizon are long, so created_at cannot bound
//     it; the fire/approval instant can, give or take the same retry lag),
//     whose send instant is at or after $2.
//
// Recipients: message_recipients rows (written when the provider or relay
// accepted the message, one per normalized envelope recipient) — or, for a
// message the provider accepted that has not settled yet (a provider message
// id with no recipient rows, or a send claimed inside the window), the
// message's own to/cc/bcc lists.
//
// "External" is the external-sending-access notion: anything other than
//   - an agent of the same account (any state — by account erase time the
//     account's agents are already trashed by the account trash),
//   - the account's verified owner mailbox (valid proof for its CURRENT
//     email, as sendingpolicy.ownerRecipientVerified), or
//   - an address whose domain — the part after the LAST '@', so a quoted
//     local part containing '@' cannot pose as an internal domain — is an
//     exempt domain ($3) or a verified domains row with no owning account.
//
// System and internal account classes are never deferred.
func sentExternallySinceSQL(agentScope, messageScope string) string {
	return `
SELECT EXISTS (
    SELECT 1
      FROM users AS u
      JOIN agent_identities AS a ON a.user_id = u.id
      CROSS JOIN LATERAL (
          SELECT m.id, m.to_recipients, m.cc, m.bcc, m.recipient,
                 m.provider_message_id, m.delivery_status, m.send_claimed_at
            FROM messages AS m
           WHERE m.agent_id = a.id AND m.direction = 'outbound'
             AND m.created_at >= $4` + messageScope + `
             AND GREATEST(m.created_at, m.provider_accepted_at, m.reviewed_at,
                          m.scheduled_at, m.send_claimed_at) >= $2
          UNION ALL
          SELECT m.id, m.to_recipients, m.cc, m.bcc, m.recipient,
                 m.provider_message_id, m.delivery_status, m.send_claimed_at
            FROM messages AS m
           WHERE m.agent_id = a.id AND m.direction = 'outbound'
             AND (m.scheduled_at IS NOT NULL OR m.reviewed_at IS NOT NULL)
             AND GREATEST(m.scheduled_at, m.reviewed_at) >= $4
             AND m.created_at < $4` + messageScope + `
             AND GREATEST(m.created_at, m.provider_accepted_at, m.reviewed_at,
                          m.scheduled_at, m.send_claimed_at) >= $2
      ) AS m
      CROSS JOIN LATERAL (
          SELECT r.address FROM message_recipients AS r WHERE r.message_id = m.id
          UNION ALL
          SELECT lower(btrim(x))
            FROM unnest(COALESCE(m.to_recipients, '{}') || COALESCE(m.cc, '{}') ||
                        COALESCE(m.bcc, '{}') ||
                        CASE WHEN m.to_recipients IS NULL THEN ARRAY[m.recipient] ELSE '{}' END) AS x
           WHERE NOT EXISTS (SELECT 1 FROM message_recipients AS r2 WHERE r2.message_id = m.id)
             AND (COALESCE(m.provider_message_id, '') <> ''
                  OR (m.delivery_status = 'sending' AND m.send_claimed_at >= $2))
      ) AS rcpt
     WHERE u.id = $1` + agentScope + `
       AND u.account_class NOT IN ` + eraseDeferExemptClassesSQL + `
       AND rcpt.address <> ''
       AND NOT EXISTS (
           SELECT 1 FROM agent_identities AS own
            WHERE own.user_id = $1 AND lower(own.id) = rcpt.address)
       AND NOT (u.owner_email_verified_at IS NOT NULL
                AND u.owner_email_verified_address IS NOT NULL
                AND u.owner_email_verified_address = lower(btrim(u.email))
                AND rcpt.address = u.owner_email_verified_address)
       AND NOT (lower(COALESCE(substring(rcpt.address from '@([^@]+)$'), '')) = ANY($3::text[]))
       AND NOT EXISTS (
           SELECT 1 FROM domains AS d
            WHERE d.user_id IS NULL AND d.verified
              AND d.domain = lower(substring(rcpt.address from '@([^@]+)$'))))`
}

var (
	accountSentExternallySinceSQL = sentExternallySinceSQL("", "")
	agentSentExternallySinceSQL   = sentExternallySinceSQL("\n       AND a.id = $5", "")
	messageSentExternallySinceSQL = sentExternallySinceSQL("", " AND m.id = $5")
)

func sentExternallySince(ctx context.Context, q rowQuerier, query, userID string, since time.Time, scope ...any) (bool, error) {
	args := append([]any{userID, since, exemptDomains(), since.Add(-EraseDeferRetryLag)}, scope...)
	var sent bool
	if err := q.QueryRow(ctx, query, args...).Scan(&sent); err != nil {
		return false, fmt.Errorf("erase: recent external send: %w", err)
	}
	return sent, nil
}

// exemptDomains is EraseDeferExemptDomains normalized, never nil (a nil
// slice would bind as SQL NULL and make "= ANY" unknown).
func exemptDomains() []string {
	out := make([]string, 0, len(EraseDeferExemptDomains))
	for _, d := range EraseDeferExemptDomains {
		if d = strings.ToLower(strings.TrimSpace(d)); d != "" {
			out = append(out, d)
		}
	}
	return out
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

// accountEraseDeferredTx reports whether a permanent erase of the account
// must be deferred to the account trash: the deferral is enabled, the
// deployment has account trash, and the account sent externally inside the
// window. purgeAccount runs it inside its claim transaction, under the user
// row lock.
func accountEraseDeferredTx(ctx context.Context, q rowQuerier, userID string) (bool, error) {
	since, ok := eraseDeferralCutoff()
	if !ok || !AccountTrashEnabled() {
		return false, nil
	}
	return sentExternallySince(ctx, q, accountSentExternallySinceSQL, userID, since)
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

// errAccountEraseDeferred is purgeAccount's in-transaction signal that an
// on-demand erase must stay in the account trash. It never leaves the store.
var errAccountEraseDeferred = errors.New("identity: account erase deferred")

// ErrPurgeDeferred is returned by the int-returning purge wrappers
// (DeleteAgent, DeleteAgentIncarnation, PurgeMessage) when the purge was
// deferred to the trash instead: nothing was deleted. Callers that need the
// deferred receipt use PermanentDeleteAgentIncarnation / PurgeMessageOrDefer.
var ErrPurgeDeferred = errors.New("identity: permanent deletion deferred to the trash (recent external send)")
