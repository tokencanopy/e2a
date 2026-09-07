package sendingpolicy

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

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
// survives, and the consumer applies the repair through RepairSuppressions
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

	corr, found, err := lookupCorrelation(ctx, tx, fb.ProviderMessageID, fb.AttemptCorrelationID)
	if err != nil {
		return delivery.FeedbackResult{}, err
	}
	if !found {
		return delivery.FeedbackResult{}, nil
	}
	result := delivery.FeedbackResult{Correlated: true}

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

	derived := DeriveBucket(fb.Kind, fb.BounceType, fb.BounceSubType, fb.ComplaintSubType)

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
			continue
		}
		if firstTime {
			if err := m.applyEvidence(ctx, tx, corr, row, derived.Bucket, fb, accountExists, accountID, epoch, today); err != nil {
				return delivery.FeedbackResult{}, err
			}
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
	if unmatched > 0 {
		// A systematic mismatch (a normalization divergence, a rotated key)
		// would otherwise be invisible: the detector would simply see
		// nothing and read as healthy.
		log.Printf("[sendingpolicy:feedback] %d recipient(s) on event %s did not match correlation %s's authorized envelope",
			unmatched, fb.ProviderEventID, corr.id)
	}

	if err := tx.Commit(ctx); err != nil {
		return delivery.FeedbackResult{}, fmt.Errorf("sendingpolicy: commit feedback: %w", err)
	}
	return result, nil
}

// RepairSuppressions writes the account-wide suppressions a signed event
// proved, for the case where no live message row can own them. Upserts go
// through suppressionsync, so a row re-proven here advances its generation
// and clears a pending removal.
func (m *Module) RepairSuppressions(ctx context.Context, accountRef string, repairs []delivery.FeedbackRepair) error {
	if strings.TrimSpace(accountRef) == "" || len(repairs) == 0 {
		return nil
	}
	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("sendingpolicy: begin suppression repair: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The account must still exist: a suppression is customer state, and
	// the row carries a foreign key to the user.
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id = $1)`, accountRef).Scan(&exists); err != nil {
		return fmt.Errorf("sendingpolicy: check account for repair: %w", err)
	}
	if !exists {
		return nil
	}
	for _, r := range repairs {
		if _, err := suppressionsync.UpsertTx(ctx, tx, "supp_"+randomSuffix(), accountRef,
			strings.ToLower(strings.TrimSpace(r.Address)), r.Reason, r.Source, ""); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// lookupCorrelation resolves the retained row by provider message id first
// (normalized to SES's bare form, the same function that bound it), then by
// the echoed attempt marker.
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
		// id to a new attempt), so order deterministically and prefer a row
		// that is still retained over one already past its horizon.
		c, ok, err := scan(tx.QueryRow(ctx, cols+`WHERE provider_message_id = $1
			ORDER BY (expires_at IS NULL OR expires_at > now()) DESC, created_at DESC, correlation_id
			LIMIT 1`, id))
		if err != nil || ok {
			return c, ok, err
		}
	}
	if attemptID = strings.TrimSpace(attemptID); attemptID != "" {
		return scan(tx.QueryRow(ctx, cols+`WHERE correlation_id = $1`, attemptID))
	}
	return feedbackCorrelation{}, false, nil
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
func (m *Module) matchRecipient(rows []*feedbackRecipient, addr string) (*feedbackRecipient, bool) {
	if m.secrets.Keyring == nil {
		return nil, false
	}
	for _, r := range rows {
		if m.secrets.Keyring.Verify(r.keyVersion, []byte(addr), r.mac) {
			return r, true
		}
	}
	return nil, false
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
	// Bounded by construction: ask only whether a retained row exists under
	// a version outside the keyring, and stop at the first one. The scan
	// cannot be made to walk the whole table.
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

func hasVersion(k *Keyring, v int) bool {
	for _, have := range k.Versions() {
		if have == v {
			return true
		}
	}
	return false
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
// correlations carry no expiry while the account exists; account deletion
// stamps one (identity.Store.DeleteUserDataTx), so this pass is what makes
// the post-deletion retention real.
func (m *Module) GCFeedback(ctx context.Context, now time.Time, windowDays int) (FeedbackGCStats, error) {
	var st FeedbackGCStats
	now = now.UTC()
	tag, err := m.pool.Exec(ctx, `DELETE FROM sending_feedback_events WHERE expires_at IS NOT NULL AND expires_at <= $1`, now)
	if err != nil {
		return st, fmt.Errorf("sendingpolicy: gc feedback events: %w", err)
	}
	st.Events = tag.RowsAffected()
	tag, err = m.pool.Exec(ctx, `
		DELETE FROM sending_feedback_recipients r
		 USING sending_feedback_correlations c
		 WHERE c.correlation_id = r.correlation_id AND c.expires_at IS NOT NULL AND c.expires_at <= $1`, now)
	if err != nil {
		return st, fmt.Errorf("sendingpolicy: gc feedback recipients: %w", err)
	}
	st.Recipients = tag.RowsAffected()
	tag, err = m.pool.Exec(ctx, `DELETE FROM sending_feedback_correlations WHERE expires_at IS NOT NULL AND expires_at <= $1`, now)
	if err != nil {
		return st, fmt.Errorf("sendingpolicy: gc feedback correlations: %w", err)
	}
	st.Correlations = tag.RowsAffected()
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

// EffectiveDetectorWindowDays reads the detector window from the policy this
// deployment actually runs (the database singleton when the source is the
// database, the validated config otherwise).
func (m *Module) EffectiveDetectorWindowDays(ctx context.Context) (int, error) {
	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("sendingpolicy: begin policy read: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	policy, err := m.effectivePolicy(ctx, tx)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("sendingpolicy: commit policy read: %w", err)
	}
	return policy.DetectorWindowDays, nil
}
