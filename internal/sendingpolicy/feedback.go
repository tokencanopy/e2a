package sendingpolicy

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/net/idna"

	"github.com/tokencanopy/e2a/internal/delivery"
	"github.com/tokencanopy/e2a/internal/suppressionsync"
)

// Deletion-resistant feedback provenance (slice B8).
//
// Provider feedback arrives long after the message that caused it, and the
// message, its agent, or the whole account may be gone by then. The detector
// must still see it: an abuser cannot be allowed to erase a complaint by
// deleting the message it belongs to. So this path never reads messages,
// agent_identities, or users. It correlates against the retained
// sending_feedback_correlations row (by provider message id, then by the
// random attempt marker SES echoes back), matches each recipient against the
// keyed HMACs recorded at authorization, and moves per-recipient detector
// buckets with monotonic evidence into the account's daily aggregates.
//
// Suppression repair is the one customer-visible effect: while the account
// still exists, a hard bounce, a genuine complaint, or a suppression-list
// subtype recreates the account-wide suppression from the signed event's
// plaintext recipient. The retained rows themselves never store an address.

// Bucket is the detector classification of one recipient's best evidence.
type Bucket string

const (
	BucketNone          Bucket = "none"
	BucketDelivered     Bucket = "delivered"
	BucketTerminalOther Bucket = "terminal_other"
	BucketHardBounce    Bucket = "hard_bounce"
	BucketComplaint     Bucket = "complaint"
)

// Rank is the deterministic evidence order none < delivered < terminal_other
// < hard_bounce < complaint. Higher evidence replaces lower; a delayed
// lower-ranked callback never erases an observed hard bounce or complaint.
func (b Bucket) Rank() int {
	switch b {
	case BucketDelivered:
		return 1
	case BucketTerminalOther:
		return 2
	case BucketHardBounce:
		return 3
	case BucketComplaint:
		return 4
	}
	return 0
}

// column is the aggregate column a bucket adds to; empty for none.
func (b Bucket) column() string {
	switch b {
	case BucketDelivered:
		return "delivered_count"
	case BucketTerminalOther:
		return "terminal_other_count"
	case BucketHardBounce:
		return "hard_bounce_count"
	case BucketComplaint:
		return "complaint_count"
	}
	return ""
}

// Suppression-list subtypes SES emits when it did not attempt delivery.
// AWS documents these as not affecting sender reputation, so they are
// excluded from both numerator and denominator; the global-list subtype
// "Suppressed" is documented as affecting reputation and stays included.
const (
	subtypeOnAccountSuppressionList = "OnAccountSuppressionList"
	subtypeOnTenantSuppressionList  = "OnTenantSuppressionList"
)

// isSuppressionListSubtype matches the two subtypes case-insensitively, the
// same way the bounce type is compared: a case variant from the provider
// must not turn an excluded suppression-list bounce into a hard bounce and
// inflate the numerator that pauses accounts.
func isSuppressionListSubtype(sub string) bool {
	sub = strings.TrimSpace(sub)
	return strings.EqualFold(sub, subtypeOnAccountSuppressionList) ||
		strings.EqualFold(sub, subtypeOnTenantSuppressionList)
}

// Derivation is a bucket plus whether the event should repair the local
// suppression list and, if so, under which source.
type Derivation struct {
	Bucket Bucket
	// Repair is true when the event proves the address should be suppressed:
	// an eligible hard bounce, a genuine complaint, or a suppression-list
	// subtype (SES already refuses it; the local list must agree).
	Repair bool
	// Source is the suppressions.source the repair records.
	Source string
}

// DeriveBucket maps a full event kind and its retained subtypes to the
// detector bucket. Every terminal bucket contributes one denominator unit;
// only hard bounce and complaint contribute numerators.
func DeriveBucket(kind delivery.EventKind, bounceType, bounceSubType, complaintSubType string) Derivation {
	switch kind {
	case delivery.KindDelivery:
		return Derivation{Bucket: BucketDelivered}
	case delivery.KindBounce:
		if isSuppressionListSubtype(bounceSubType) {
			return Derivation{Bucket: BucketNone, Repair: true, Source: suppressionsync.SourceBounce}
		}
		if strings.EqualFold(strings.TrimSpace(bounceType), "permanent") {
			return Derivation{Bucket: BucketHardBounce, Repair: true, Source: suppressionsync.SourceBounce}
		}
		// SES emits a bounce only once it has given up: a transient or
		// undetermined bounce is terminal for this recipient, just not an
		// eligible hard bounce.
		return Derivation{Bucket: BucketTerminalOther}
	case delivery.KindComplaint:
		if isSuppressionListSubtype(complaintSubType) {
			return Derivation{Bucket: BucketNone, Repair: true, Source: suppressionsync.SourceComplaint}
		}
		return Derivation{Bucket: BucketComplaint, Repair: true, Source: suppressionsync.SourceComplaint}
	}
	// Send, DeliveryDelay, Reject, Other: acceptance, non-terminal, or a
	// submission verdict — none are delivery outcomes.
	return Derivation{Bucket: BucketNone}
}

// feedbackCorrelation is the retained row a notification is matched to.
type feedbackCorrelation struct {
	id            string
	sourceAccount *string
	purpose       Purpose
	shared        bool
	expiresAt     *time.Time
}

// feedbackRecipient is one retained recipient row, keyed by HMAC.
type feedbackRecipient struct {
	mac        []byte
	keyVersion int
	bucket     Bucket
	rank       int
	occurredAt *time.Time
	eventID    *string
	epoch      *int64
	day        *time.Time
}

// ProcessProviderFeedback implements delivery.FeedbackProcessor: it moves
// detector evidence for one signed notification and reports the account
// suppressions that notification proves. It deliberately writes NO
// suppression itself — the live message path owns that row when a message
// survives, and the consumer applies the repair through RepairSuppressionsTx
// only when none does.
func (m *Module) ProcessProviderFeedback(ctx context.Context, fb delivery.ProviderFeedback) (delivery.FeedbackResult, error) {
	if strings.TrimSpace(fb.ProviderEventID) == "" {
		return delivery.FeedbackResult{}, errors.New("sendingpolicy: provider event id is required")
	}
	if fb.OccurredAt.IsZero() {
		return delivery.FeedbackResult{}, errors.New("sendingpolicy: provider event time is required")
	}

	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return delivery.FeedbackResult{}, fmt.Errorf("sendingpolicy: begin feedback: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	derived := DeriveBucket(fb.Kind, fb.BounceType, fb.BounceSubType, fb.ComplaintSubType)

	corr, found, err := lookupCorrelation(ctx, tx, fb.ProviderMessageID, fb.AttemptCorrelationID)
	if err != nil {
		return delivery.FeedbackResult{}, err
	}
	if !found {
		expired := false
		if validAttemptMarker(fb.AttemptCorrelationID) {
			if expired, err = pastRetention(ctx, tx, fb.AttemptCorrelationID); err != nil {
				return delivery.FeedbackResult{}, err
			}
		}
		if validAttemptMarker(fb.AttemptCorrelationID) && !expired {
			// e2a stamped this mail, yet no retained correlation answers
			// it: a correlation GC'd early, a write that never landed, or a
			// forged marker. Spec: count and alert, never treat as healthy.
			// The marker value is deliberately not logged or labelled.
			observeFeedback(FeedbackOutcomeUncorrelatedWithMarker, derived.Bucket)
			log.Printf("[sendingpolicy:feedback] %s event %s carries the provider-attempt marker but matches no retained correlation",
				fb.Kind, fb.ProviderEventID)
		} else {
			observeFeedback(FeedbackOutcomeUncorrelated, derived.Bucket)
		}
		return delivery.FeedbackResult{}, nil
	}
	result := delivery.FeedbackResult{Correlated: true}
	// Metric samples are emitted only after commit: a rolled-back pass is
	// retried by SNS and must not be counted twice.
	var samples []feedbackSample

	// One row per provider event id. A redelivered notification does not
	// move evidence again — though the evidence rules are themselves
	// idempotent for a replay, since a replayed event can only ever tie its
	// own rank. Repairs are still reported below, so a retry whose live
	// half failed after this commit can still finish its work.
	tag, err := tx.Exec(ctx, `
		INSERT INTO sending_feedback_events (provider_event_id, correlation_id, provider_occurred_at, expires_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (provider_event_id) DO NOTHING`,
		fb.ProviderEventID, corr.id, fb.OccurredAt.UTC(), corr.expiresAt)
	if err != nil {
		return delivery.FeedbackResult{}, fmt.Errorf("sendingpolicy: record feedback event: %w", err)
	}
	firstTime := tag.RowsAffected() == 1
	result.Duplicate = !firstTime
	if !firstTime {
		samples = append(samples, feedbackSample{FeedbackOutcomeDuplicate, derived.Bucket})
	}

	// Lock the account control row when the account still exists: it holds
	// the epoch the new evidence is assigned to, and the pause transition
	// (B9) takes the same lock, so an epoch cannot move under this write.
	//
	// Lock order note: this transaction takes the control row and then the
	// account's aggregate row (a users foreign key). Account deletion takes
	// the users row first and cascades into the control row, so the two can
	// deadlock. Postgres aborts one; this side is an SNS retry, and a
	// delete that wins leaves the lookup below returning no rows, which is
	// the provenance-only path. Deletion is rare and operator-initiated, so
	// the exposure is one retried notification.
	accountExists := false
	var accountID string
	var epoch int64
	if corr.purpose.isCustomer() && corr.sourceAccount != nil {
		err := tx.QueryRow(ctx,
			`SELECT user_id, outcome_epoch FROM account_sending_controls WHERE user_id = $1 FOR UPDATE`,
			*corr.sourceAccount,
		).Scan(&accountID, &epoch)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			// Account deleted: provenance only, never recreate customer state.
		case err != nil:
			return delivery.FeedbackResult{}, fmt.Errorf("sendingpolicy: lock account control: %w", err)
		default:
			accountExists = true
			result.AccountRef = accountID
		}
	}

	rows, err := loadFeedbackRecipients(ctx, tx, corr.id)
	if err != nil {
		return delivery.FeedbackResult{}, err
	}
	// The ingestion day is "now", not the provider time: the spec assigns
	// new evidence to the current epoch and UTC day so a delayed callback
	// counts where the detector is looking.
	now := m.now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)

	unmatched := 0
	for _, addr := range fb.Recipients {
		addr = strings.ToLower(strings.TrimSpace(addr))
		if addr == "" {
			continue
		}
		row, matched := m.matchRecipient(rows, addr)
		if !matched {
			// Not in the authorized envelope: no accounting, no repair. A
			// mismatched recipient on a signed event is either a provider
			// quirk or forged input; either way it proves nothing about an
			// address this account sent to. Counted, never logged with the
			// address itself.
			unmatched++
			if firstTime {
				samples = append(samples, feedbackSample{FeedbackOutcomeUnmatchedRecipient, derived.Bucket})
			}
			continue
		}
		if firstTime {
			bucket := derived.Bucket
			if bucket.denominatorOnly() {
				excluded, err := m.excludedFromDenominator(ctx, tx, corr, accountExists, addr)
				if err != nil {
					return delivery.FeedbackResult{}, err
				}
				if excluded {
					bucket = BucketNone
				}
			}
			if err := m.applyEvidence(ctx, tx, corr, row, bucket, fb, accountExists, accountID, epoch, today); err != nil {
				return delivery.FeedbackResult{}, err
			}
			outcome := FeedbackOutcomeCorrelated
			if corr.purpose.isCustomer() && corr.sourceAccount != nil && !accountExists {
				outcome = FeedbackOutcomeDeadAccount
			}
			samples = append(samples, feedbackSample{outcome, bucket})
		}
		// Repair is reported for CUSTOMER MESSAGES only. Platform mail this
		// account merely triggered — an approval notice, a webhook health
		// warning — is addressed to the account owner, and a bounce on it
		// suppressing that address account-wide would block the customer's
		// own sends to it: a new customer-visible effect the pre-B8 path
		// never had. Those purposes still feed the detector above, which is
		// what the spec requires of them.
		if derived.Repair && accountExists && corr.purpose == PurposeCustomerMessage {
			result.RepairNeeded = append(result.RepairNeeded, delivery.FeedbackRepair{
				Address: addr, Source: derived.Source, Reason: repairReason(fb, derived),
			})
		}
	}
	if unmatched > 0 && firstTime {
		// A systematic mismatch (a normalization divergence, a rotated key)
		// would otherwise be invisible: the detector would simply see
		// nothing and read as healthy.
		log.Printf("[sendingpolicy:feedback] %d recipient(s) on event %s did not match correlation %s's authorized envelope",
			unmatched, fb.ProviderEventID, corr.id)
	}

	if err := tx.Commit(ctx); err != nil {
		return delivery.FeedbackResult{}, fmt.Errorf("sendingpolicy: commit feedback: %w", err)
	}
	for _, sm := range samples {
		observeFeedback(sm.outcome, sm.bucket)
	}
	return result, nil
}

// Feedback ingestion outcomes, the bounded `outcome` label of
// e2a_sending_feedback_ingested_total. One sample per matched or unmatched
// recipient of a first-seen event, one per uncorrelated or duplicate event.
const (
	// FeedbackOutcomeCorrelated: a recipient matched its retained HMAC and
	// its evidence was applied (to a live account, or to a non-customer
	// purpose that has no account to aggregate into).
	FeedbackOutcomeCorrelated = "correlated"
	// FeedbackOutcomeDeadAccount: correlated, but the customer account is
	// gone — provenance-only, no aggregate, no customer state.
	FeedbackOutcomeDeadAccount = "dead_account"
	// FeedbackOutcomeUnmatchedRecipient: the event correlated but this
	// recipient is not in the authorized envelope (or its key is not held).
	FeedbackOutcomeUnmatchedRecipient = "unmatched_recipient"
	// FeedbackOutcomeUncorrelatedWithMarker: e2a's attempt marker is present
	// but no retained correlation answers it. The alerting outcome.
	FeedbackOutcomeUncorrelatedWithMarker = "uncorrelated_with_marker"
	// FeedbackOutcomeUncorrelated: no marker and no provider-id match —
	// mail from before B8, from another deployment on the topic, or past
	// its retention.
	FeedbackOutcomeUncorrelated = "uncorrelated"
	// FeedbackOutcomeDuplicate: the provider event id was already recorded.
	FeedbackOutcomeDuplicate = "duplicate"
)

// FeedbackObserver receives one bounded (outcome, bucket) sample per
// ingestion result. Set once at startup by the composition root
// (telemetry.Metrics.SendingFeedbackIngested); nil = no-op. Never carries an
// address, account, correlation, or provider id.
type FeedbackObserver func(outcome, bucket string)

var feedbackObserver atomic.Value // FeedbackObserver

// SetFeedbackObserver installs the process-wide ingestion observer.
func SetFeedbackObserver(o FeedbackObserver) { feedbackObserver.Store(o) }

func observeFeedback(outcome string, bucket Bucket) {
	if o, ok := feedbackObserver.Load().(FeedbackObserver); ok && o != nil {
		o(outcome, string(bucket))
	}
}

type feedbackSample struct {
	outcome string
	bucket  Bucket
}

// validAttemptMarker mirrors the consumer's shape check: the parser already
// drops a malformed header, so any non-empty value here is e2a-shaped.
func validAttemptMarker(v string) bool { return strings.HasPrefix(strings.TrimSpace(v), "cor_") }

// sesMailboxSimulatorDomain is SES's mailbox simulator. Mail to it never
// reaches a real inbox, so its deliveries prove nothing about a sender.
const sesMailboxSimulatorDomain = "simulator.amazonses.com"

// WithFeedbackExcludedDomains configures the deployment's shared agent
// domains (config shared_domain): a delivery to an agent this deployment
// hosts is free for any sender to manufacture and must not dilute the
// detector's denominator. The SES mailbox simulator is always excluded.
// Domains are canonicalized (IDNA ASCII, lower case).
func (m *Module) WithFeedbackExcludedDomains(domains ...string) *Module {
	set := map[string]struct{}{}
	for _, d := range domains {
		if d = canonicalDomain(d); d != "" {
			set[d] = struct{}{}
		}
	}
	m.feedbackExcludedDomains = set
	return m
}

// ExcludesFeedbackDomain reports whether deliveries to domain are excluded
// from the detector denominator by configuration (the simulator or a
// configured shared agent domain). The composition-root test reads it.
func (m *Module) ExcludesFeedbackDomain(domain string) bool {
	d := canonicalDomain(domain)
	if d == sesMailboxSimulatorDomain {
		return true
	}
	_, ok := m.feedbackExcludedDomains[d]
	return ok
}

// denominatorOnly reports whether a bucket adds to the detector's
// denominator without adding to any numerator.
func (b Bucket) denominatorOnly() bool {
	return b == BucketDelivered || b == BucketTerminalOther
}

// excludedFromDenominator reports whether a denominator-only outcome for
// addr must not count: the SES mailbox simulator, the deployment's shared
// agent domains (configured, plus the platform-owned verified domains rows
// seeded for them), and the sending account's own verified domains. Each is
// mail a sender can generate at will to itself, so counting it would let an
// abuser dilute a bounce or complaint rate below the pause threshold with
// self-addressed traffic. Only denominator-only outcomes are excluded: a
// hard bounce or complaint from any of these recipients is still evidence
// against the sender and is never discarded. Decided from configuration and
// the domains table, never DNS, because it runs on the ingestion path.
func (m *Module) excludedFromDenominator(ctx context.Context, tx pgx.Tx, corr feedbackCorrelation, accountExists bool, addr string) (bool, error) {
	at := strings.LastIndexByte(addr, '@')
	if at < 0 || at == len(addr)-1 {
		return false, nil
	}
	raw := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(addr[at+1:])), ".")
	domain := canonicalDomain(raw)
	if m.ExcludesFeedbackDomain(domain) {
		return true, nil
	}
	var account *string
	if accountExists && corr.sourceAccount != nil {
		account = corr.sourceAccount
	}
	var excluded bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
		    SELECT 1 FROM domains
		     WHERE domain = ANY($1::text[]) AND verified
		       AND (user_id IS NULL OR user_id = $2))`,
		[]string{domain, raw}, account,
	).Scan(&excluded); err != nil {
		return false, fmt.Errorf("sendingpolicy: classify feedback recipient domain: %w", err)
	}
	return excluded, nil
}

// RepairSuppressionsTx writes the account-wide suppressions a signed event
// proved, for the case where no live message row can own them, inside the
// caller's transaction. Upserts go through suppressionsync, so a row
// re-proven here advances its generation and clears a pending removal. It
// returns only the repairs that inserted a new row: an address already
// suppressed — manually, by an earlier bounce — is refreshed, never
// reported, so the consumer announces nothing for it.
func (m *Module) RepairSuppressionsTx(ctx context.Context, tx pgx.Tx, accountRef string, repairs []delivery.FeedbackRepair) ([]delivery.FeedbackRepair, error) {
	if strings.TrimSpace(accountRef) == "" || len(repairs) == 0 {
		return nil, nil
	}
	// The account must still exist: a suppression is customer state, and
	// the row carries a foreign key to the user.
	// FOR KEY SHARE, not a bare EXISTS: the suppression rows carry a foreign
	// key to this user, so a deletion committing between the check and the
	// upsert would turn a no-op into a constraint violation. The key share
	// blocks that delete for the length of this transaction and is exactly
	// what the insert would take anyway.
	var live string
	err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id = $1 FOR KEY SHARE`, accountRef).Scan(&live)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sendingpolicy: check account for repair: %w", err)
	}
	var inserted []delivery.FeedbackRepair
	for _, r := range repairs {
		up, err := suppressionsync.UpsertTx(ctx, tx, "supp_"+randomSuffix(), accountRef,
			strings.ToLower(strings.TrimSpace(r.Address)), r.Reason, r.Source, "")
		if err != nil {
			return nil, err
		}
		if up.Inserted {
			inserted = append(inserted, r)
		}
	}
	return inserted, nil
}

// RepairSuppressions is RepairSuppressionsTx in a transaction of its own,
// for operator tooling and tests that have no enclosing transaction.
func (m *Module) RepairSuppressions(ctx context.Context, accountRef string, repairs []delivery.FeedbackRepair) ([]delivery.FeedbackRepair, error) {
	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("sendingpolicy: begin suppression repair: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	inserted, err := m.RepairSuppressionsTx(ctx, tx, accountRef, repairs)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("sendingpolicy: commit suppression repair: %w", err)
	}
	return inserted, nil
}

// lookupCorrelation resolves the retained row by provider message id first
// (normalized to SES's bare form, the same function that bound it), then by
// the echoed attempt marker.
//
// Both reads take FOR SHARE. The account purge's seal stamps expires_at on
// this row and then on its events; without the share lock, a notification
// could read the pre-seal NULL expiry, the seal could stamp and commit, and
// this transaction would then insert an event row with a NULL expiry that
// nothing ever stamps again. With it, the seal's UPDATE waits for this
// transaction (whose event row its follow-up events UPDATE then sees), or
// this read waits for the seal and re-reads the stamped expiry. Lock order
// is correlation (share) → control row (update) → users (key share, via the
// aggregate FK); the seal holds users FOR NO KEY UPDATE, which does not
// conflict with key share, so the two cannot deadlock. The retention
// janitor's DELETE of an expired correlation likewise waits for, or is seen
// by, this lock, so no event can be inserted for a correlation the janitor
// just removed.
func lookupCorrelation(ctx context.Context, tx pgx.Tx, providerMessageID, attemptID string) (feedbackCorrelation, bool, error) {
	const cols = `SELECT correlation_id, source_account_ref, purpose, shared_reputation, expires_at
	                FROM sending_feedback_correlations `
	scan := func(row pgx.Row) (feedbackCorrelation, bool, error) {
		var c feedbackCorrelation
		var purpose string
		err := row.Scan(&c.id, &c.sourceAccount, &purpose, &c.shared, &c.expiresAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return feedbackCorrelation{}, false, nil
		}
		if err != nil {
			return feedbackCorrelation{}, false, fmt.Errorf("sendingpolicy: lookup correlation: %w", err)
		}
		c.purpose = Purpose(purpose)
		return c, true, nil
	}
	if id := NormalizeProviderMessageID(providerMessageID); id != "" {
		// provider_message_id is not unique (a re-driven send can bind a new
		// id to a new attempt), so order deterministically. Rows past their
		// horizon are excluded outright rather than merely ordered last: the
		// janitor deletes exactly those rows, and a LIMIT 1 FOR SHARE whose
		// chosen row is deleted concurrently returns NO row instead of the
		// next one — a spurious uncorrelated result. An unexpired row is
		// never a GC target, so the lock cannot lose it.
		c, ok, err := scan(tx.QueryRow(ctx, cols+`WHERE provider_message_id = $1
			  AND (expires_at IS NULL OR expires_at > now())
			ORDER BY created_at DESC, correlation_id
			LIMIT 1
			  FOR SHARE`, id))
		if err != nil || ok {
			return c, ok, err
		}
	}
	if attemptID = strings.TrimSpace(attemptID); attemptID != "" {
		return scan(tx.QueryRow(ctx, cols+`WHERE correlation_id = $1
			  AND (expires_at IS NULL OR expires_at > now())
			  FOR SHARE`, attemptID))
	}
	return feedbackCorrelation{}, false, nil
}

// pastRetention reports whether the marker names a correlation that still
// exists but is past its horizon (or is being removed by the janitor right
// now — this unlocked read sees the pre-delete snapshot). Feedback for it is
// retention working as designed, not the lost-correlation alert.
func pastRetention(ctx context.Context, tx pgx.Tx, attemptID string) (bool, error) {
	var exists bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM sending_feedback_correlations WHERE correlation_id = $1)`,
		strings.TrimSpace(attemptID)).Scan(&exists); err != nil {
		return false, fmt.Errorf("sendingpolicy: check expired correlation: %w", err)
	}
	return exists, nil
}

func loadFeedbackRecipients(ctx context.Context, tx pgx.Tx, correlationID string) ([]*feedbackRecipient, error) {
	rows, err := tx.Query(ctx, `
		SELECT recipient_hmac, hmac_key_version, detector_bucket, evidence_rank,
		       provider_occurred_at, evidence_event_id, bucket_epoch, bucket_day
		  FROM sending_feedback_recipients
		 WHERE correlation_id = $1
		 ORDER BY recipient_hmac
		   FOR UPDATE`, correlationID)
	if err != nil {
		return nil, fmt.Errorf("sendingpolicy: load feedback recipients: %w", err)
	}
	defer rows.Close()
	var out []*feedbackRecipient
	for rows.Next() {
		r := &feedbackRecipient{}
		var bucket string
		if err := rows.Scan(&r.mac, &r.keyVersion, &bucket, &r.rank, &r.occurredAt, &r.eventID, &r.epoch, &r.day); err != nil {
			return nil, fmt.Errorf("sendingpolicy: scan feedback recipient: %w", err)
		}
		r.bucket = Bucket(bucket)
		out = append(out, r)
	}
	return out, rows.Err()
}

// matchRecipient finds the retained row whose keyed HMAC verifies for addr.
// Without a keyring nothing can match, which is the fail-closed answer: a
// process that does not hold the key cannot vouch for a recipient.
//
// The HMAC subject is canonicalRecipient(addr), the same function the gate
// signed with. A row signed before canonicalization existed (a Unicode
// domain signed as typed) is still matched through the raw form.
func (m *Module) matchRecipient(rows []*feedbackRecipient, addr string) (*feedbackRecipient, bool) {
	if m.secrets.Keyring == nil {
		return nil, false
	}
	subjects := [][]byte{[]byte(canonicalRecipient(addr))}
	if raw := strings.ToLower(strings.TrimSpace(addr)); raw != string(subjects[0]) {
		subjects = append(subjects, []byte(raw))
	}
	for _, r := range rows {
		for _, subject := range subjects {
			if m.secrets.Keyring.Verify(r.keyVersion, subject, r.mac) {
				return r, true
			}
		}
	}
	return nil, false
}

// canonicalRecipient is the ONE form a recipient's keyed HMAC is computed
// over, on both sides of the provider: the gate signs it at authorization,
// and feedback verifies against it. The local part is lower-cased as typed;
// the domain goes through IDNA ToASCII (lookup profile), so an
// internationalized domain authorized as Unicode ("bücher.test") and
// reported back by the provider in its A-label form ("xn--bcher-kva.test")
// produce the same HMAC. A domain the lookup profile refuses keeps its
// lower-cased raw form — deterministic, so both sides still agree.
func canonicalRecipient(addr string) string {
	addr = strings.ToLower(strings.TrimSpace(addr))
	at := strings.LastIndexByte(addr, '@')
	if at <= 0 || at == len(addr)-1 {
		return addr
	}
	return addr[:at+1] + canonicalDomain(addr[at+1:])
}

// canonicalDomain is canonicalRecipient's domain half.
func canonicalDomain(domain string) string {
	domain = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
	if ascii, err := idna.Lookup.ToASCII(domain); err == nil && ascii != "" {
		return strings.ToLower(ascii)
	}
	return domain
}

// applyEvidence moves one recipient's bucket under the monotonic evidence
// order and keeps the daily aggregates in step: subtract the prior bucket
// from the epoch/day it was counted in, add the new one to the current
// epoch and today, all in the caller's transaction.
func (m *Module) applyEvidence(ctx context.Context, tx pgx.Tx, corr feedbackCorrelation, row *feedbackRecipient, bucket Bucket, fb delivery.ProviderFeedback, accountExists bool, accountID string, epoch int64, today time.Time) error {
	newRank := bucket.Rank()
	occurred := fb.OccurredAt.UTC()
	switch {
	case newRank > row.rank:
		// replace below
	case newRank == row.rank:
		// Equal evidence: zero delta. Provider time, then event id, decide
		// which event the row remembers as its provenance.
		if row.occurredAt != nil && (occurred.Before(*row.occurredAt) ||
			(occurred.Equal(*row.occurredAt) && row.eventID != nil && fb.ProviderEventID <= *row.eventID)) {
			return nil
		}
		if _, err := tx.Exec(ctx, `
			UPDATE sending_feedback_recipients
			   SET provider_occurred_at = $3, evidence_event_id = $4, updated_at = now()
			 WHERE correlation_id = $1 AND recipient_hmac = $2`,
			corr.id, row.mac, occurred, fb.ProviderEventID); err != nil {
			return fmt.Errorf("sendingpolicy: update feedback provenance: %w", err)
		}
		return nil
	default:
		// Lower evidence than already observed: never regress.
		return nil
	}

	// Subtract the prior bucket where it was counted. A missing aggregate
	// row (expired, or the account is gone and the FK cascaded) is a no-op.
	if col := row.bucket.column(); col != "" && row.epoch != nil && row.day != nil && corr.sourceAccount != nil {
		if _, err := tx.Exec(ctx, fmt.Sprintf(`
			UPDATE account_sending_outcomes_daily
			   SET %[1]s = GREATEST(%[1]s - 1, 0)
			 WHERE user_id = $1 AND outcome_epoch = $2 AND day = $3 AND shared_reputation = $4`, col),
			*corr.sourceAccount, *row.epoch, *row.day, corr.shared); err != nil {
			return fmt.Errorf("sendingpolicy: subtract prior outcome: %w", err)
		}
	}

	var newEpoch *int64
	var newDay *time.Time
	if col := bucket.column(); col != "" && accountExists {
		if _, err := tx.Exec(ctx, fmt.Sprintf(`
			INSERT INTO account_sending_outcomes_daily (user_id, outcome_epoch, day, shared_reputation, %[1]s)
			VALUES ($1, $2, $3, $4, 1)
			ON CONFLICT (user_id, outcome_epoch, day, shared_reputation) DO UPDATE
			    SET %[1]s = account_sending_outcomes_daily.%[1]s + 1`, col),
			accountID, epoch, today, corr.shared); err != nil {
			return fmt.Errorf("sendingpolicy: add outcome: %w", err)
		}
		newEpoch, newDay = &epoch, &today
	}

	if _, err := tx.Exec(ctx, `
		UPDATE sending_feedback_recipients
		   SET detector_bucket = $3, evidence_rank = $4, provider_occurred_at = $5,
		       evidence_event_id = $6, bucket_epoch = $7, bucket_day = $8, updated_at = now()
		 WHERE correlation_id = $1 AND recipient_hmac = $2`,
		corr.id, row.mac, string(bucket), newRank, occurred, fb.ProviderEventID, newEpoch, newDay); err != nil {
		return fmt.Errorf("sendingpolicy: update feedback recipient: %w", err)
	}
	row.bucket, row.rank, row.epoch, row.day = bucket, newRank, newEpoch, newDay
	row.occurredAt, row.eventID = &occurred, &fb.ProviderEventID
	return nil
}

// repairReason is the suppression reason recorded: the retained subtype, so
// the customer sees why (never the diagnostic text, which can quote the
// recipient's server).
func repairReason(fb delivery.ProviderFeedback, d Derivation) string {
	switch fb.Kind {
	case delivery.KindBounce:
		if s := strings.TrimSpace(fb.BounceSubType); s != "" {
			return "bounce:" + s
		}
		return "bounce:" + fb.BounceType
	case delivery.KindComplaint:
		if s := strings.TrimSpace(fb.ComplaintSubType); s != "" {
			return "complaint:" + s
		}
		return "complaint"
	}
	return string(d.Bucket)
}

func randomSuffix() string { return strings.TrimPrefix(randomID("x_"), "x_") }

// VerifyKeyringCoverage fails closed when a retained, unexpired recipient
// row was signed under a key version this process does not hold. Feedback
// for those rows could never be matched, which would leave the detector
// silently blind; refusing to start is the alert.
//
// A deployment with NO keyring configured is not checked: it signs nothing
// and matches nothing by design (self-host with every control disabled), and
// bricking such a server over rows an earlier configuration wrote would turn
// a disabled feature into an outage. Removing a key version while it is
// still in use is the case this guards.
func (m *Module) VerifyKeyringCoverage(ctx context.Context) error {
	if m.secrets.Keyring == nil {
		return nil
	}
	held := m.secrets.Keyring.Versions()
	// One question, first answer wins. A violation stops at the first row;
	// proving the healthy case is inherently a full pass over the retained
	// rows, which no index can serve for a "not in this set" predicate.
	// That cost is boot-only, and the alternative — starting blind — is
	// the failure this gate exists to prevent.
	var missing *int
	err := m.pool.QueryRow(ctx, `
		SELECT r.hmac_key_version
		  FROM sending_feedback_recipients r
		  JOIN sending_feedback_correlations c ON c.correlation_id = r.correlation_id
		 WHERE NOT (r.hmac_key_version = ANY($1::int[]))
		   AND (c.expires_at IS NULL OR c.expires_at > now())
		 LIMIT 1`, held).Scan(&missing)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("sendingpolicy: read retained key versions: %w", err)
	}
	return fmt.Errorf("sendingpolicy: retained feedback rows are signed under HMAC key version %d, which this keyring (versions %v) does not hold; keep the old key until those rows expire, then remove it", *missing, held)
}

// FeedbackGCStats reports what one retention pass removed.
type FeedbackGCStats struct {
	Events       int64
	Recipients   int64
	Correlations int64
	Outcomes     int64
}

// GCFeedback removes feedback provenance past its horizon and daily outcome
// rows outside the detector window plus one UTC day of safety. Customer
// correlations carry no expiry while the account exists; the account purge's
// seal transaction (identity.Store.purgeAccount) stamps one when the account
// becomes irrecoverable, so this pass is what makes the post-purge retention
// real. A trashed-but-restorable account is not stamped.
//
// Events and recipients are removed BY THEIR CORRELATION, in the statement
// that removes it, rather than by their own expiry column: an event whose
// expiry was never stamped (a pre-fix race with the purge seal left some
// with NULL) must still go when its correlation does. Events are also
// removed by their own stamped expiry, which is always the correlation's.
func (m *Module) GCFeedback(ctx context.Context, now time.Time, windowDays int) (FeedbackGCStats, error) {
	var st FeedbackGCStats
	now = now.UTC()
	// The outer SELECT reads only the CTEs' RETURNING output, never a table
	// a CTE modified, so the statement-snapshot hazard does not apply.
	if err := m.pool.QueryRow(ctx, `
		WITH gone AS (
		    DELETE FROM sending_feedback_correlations
		     WHERE expires_at IS NOT NULL AND expires_at <= $1
		    RETURNING correlation_id
		), recipients AS (
		    DELETE FROM sending_feedback_recipients r
		     USING gone WHERE r.correlation_id = gone.correlation_id
		    RETURNING 1
		), events AS (
		    DELETE FROM sending_feedback_events e
		     USING gone WHERE e.correlation_id = gone.correlation_id
		    RETURNING 1
		)
		SELECT (SELECT count(*) FROM gone), (SELECT count(*) FROM recipients), (SELECT count(*) FROM events)`,
		now).Scan(&st.Correlations, &st.Recipients, &st.Events); err != nil {
		return st, fmt.Errorf("sendingpolicy: gc feedback provenance: %w", err)
	}
	tag, err := m.pool.Exec(ctx, `DELETE FROM sending_feedback_events WHERE expires_at IS NOT NULL AND expires_at <= $1`, now)
	if err != nil {
		return st, fmt.Errorf("sendingpolicy: gc feedback events: %w", err)
	}
	st.Events += tag.RowsAffected()
	if windowDays < 1 {
		windowDays = 7
	}
	cutoff := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -(windowDays + 1))
	tag, err = m.pool.Exec(ctx, `DELETE FROM account_sending_outcomes_daily WHERE day < $1`, cutoff)
	if err != nil {
		return st, fmt.Errorf("sendingpolicy: gc daily outcomes: %w", err)
	}
	st.Outcomes = tag.RowsAffected()
	return st, nil
}

// FeedbackReconcileStats reports what one retention reconciliation changed.
type FeedbackReconcileStats struct {
	// StampedCorrelations are customer correlations whose account no longer
	// exists but which had no expiry yet.
	StampedCorrelations int64
	// StampedEvents are events given their correlation's expiry.
	StampedEvents int64
	// OrphanEvents are events whose correlation was already gone.
	OrphanEvents int64
}

// ReconcileFeedbackRetention closes the gaps the purge seal cannot: it
// stamps expires_at = now + retention on every customer correlation whose
// source account no longer has a users row (erased before the seal stamped
// anything, or a correlation authorized in a race with the purge), gives
// every unstamped event its correlation's expiry, and deletes events whose
// correlation is already gone. Idempotent: only NULL expiries are written.
// Migration 124 runs the same predicate once for the backlog; this pass is
// the standing backstop.
//
// A trashed account still has its users row, so it is never stamped here —
// the same rule the seal follows.
func (m *Module) ReconcileFeedbackRetention(ctx context.Context, now time.Time, retention time.Duration) (FeedbackReconcileStats, error) {
	var st FeedbackReconcileStats
	if retention <= 0 {
		return st, errors.New("sendingpolicy: feedback retention must be positive")
	}
	expires := now.UTC().Add(retention)
	tag, err := m.pool.Exec(ctx, `
		UPDATE sending_feedback_correlations c
		   SET expires_at = $1
		 WHERE c.expires_at IS NULL
		   AND c.purpose IN ('customer_message', 'customer_notification')
		   AND c.source_account_ref IS NOT NULL
		   AND NOT EXISTS (SELECT 1 FROM users u WHERE u.id = c.source_account_ref)`, expires)
	if err != nil {
		return st, fmt.Errorf("sendingpolicy: stamp orphaned feedback correlations: %w", err)
	}
	st.StampedCorrelations = tag.RowsAffected()
	tag, err = m.pool.Exec(ctx, `
		UPDATE sending_feedback_events e
		   SET expires_at = c.expires_at
		  FROM sending_feedback_correlations c
		 WHERE c.correlation_id = e.correlation_id
		   AND e.expires_at IS NULL AND c.expires_at IS NOT NULL`)
	if err != nil {
		return st, fmt.Errorf("sendingpolicy: stamp feedback event retention: %w", err)
	}
	st.StampedEvents = tag.RowsAffected()
	tag, err = m.pool.Exec(ctx, `
		DELETE FROM sending_feedback_events e
		 WHERE NOT EXISTS (SELECT 1 FROM sending_feedback_correlations c WHERE c.correlation_id = e.correlation_id)`)
	if err != nil {
		return st, fmt.Errorf("sendingpolicy: sweep orphaned feedback events: %w", err)
	}
	st.OrphanEvents = tag.RowsAffected()
	return st, nil
}

// effectivePolicyNow reads the policy this deployment actually runs: the
// database singleton when the source is the database, the validated config
// otherwise.
func (m *Module) effectivePolicyNow(ctx context.Context) (RuntimePolicy, error) {
	if m.source != PolicySourceDatabase {
		return m.configPolicy, nil
	}
	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return RuntimePolicy{}, fmt.Errorf("sendingpolicy: begin policy read: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	policy, err := m.effectivePolicy(ctx, tx)
	if err != nil {
		return RuntimePolicy{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return RuntimePolicy{}, fmt.Errorf("sendingpolicy: commit policy read: %w", err)
	}
	return policy, nil
}

// EffectiveDetectorWindowDays reads the detector window from the effective
// policy.
func (m *Module) EffectiveDetectorWindowDays(ctx context.Context) (int, error) {
	policy, err := m.effectivePolicyNow(ctx)
	if err != nil {
		return 0, err
	}
	return policy.DetectorWindowDays, nil
}

// EffectiveFeedbackRetention is the post-account-deletion horizon for
// retained feedback provenance, from the effective policy — the same
// accessor the retention janitor reads, so the purge seal, the janitor's
// backstop stamp, and non-customer correlations all agree on a database-
// source deployment whose activated policy differs from the config file.
func (m *Module) EffectiveFeedbackRetention(ctx context.Context) (time.Duration, error) {
	policy, err := m.effectivePolicyNow(ctx)
	if err != nil {
		return 0, err
	}
	if policy.SendingFeedbackPostAcctRetention < 1 {
		return 0, fmt.Errorf("sendingpolicy: effective feedback retention is %d days", policy.SendingFeedbackPostAcctRetention)
	}
	return time.Duration(policy.SendingFeedbackPostAcctRetention) * 24 * time.Hour, nil
}
