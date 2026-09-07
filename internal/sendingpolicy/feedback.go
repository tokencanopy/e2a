package sendingpolicy

import (
	"context"
	"errors"
	"fmt"
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
		switch strings.TrimSpace(bounceSubType) {
		case subtypeOnAccountSuppressionList, subtypeOnTenantSuppressionList:
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
		switch strings.TrimSpace(complaintSubType) {
		case subtypeOnAccountSuppressionList, subtypeOnTenantSuppressionList:
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

// ProcessProviderFeedback implements delivery.FeedbackProcessor.
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

	// One row per provider event id: a redelivered notification is a zero
	// delta even after the customer message is gone.
	tag, err := tx.Exec(ctx, `
		INSERT INTO sending_feedback_events (provider_event_id, correlation_id, provider_occurred_at, expires_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (provider_event_id) DO NOTHING`,
		fb.ProviderEventID, corr.id, fb.OccurredAt.UTC(), corr.expiresAt)
	if err != nil {
		return delivery.FeedbackResult{}, fmt.Errorf("sendingpolicy: record feedback event: %w", err)
	}
	if tag.RowsAffected() == 0 {
		result.Duplicate = true
		if err := tx.Commit(ctx); err != nil {
			return delivery.FeedbackResult{}, fmt.Errorf("sendingpolicy: commit feedback: %w", err)
		}
		return result, nil
	}

	derived := DeriveBucket(fb.Kind, fb.BounceType, fb.BounceSubType, fb.ComplaintSubType)

	// Lock the account control row when the account still exists: it holds
	// the epoch the new evidence is assigned to, and the pause transition
	// (B9) takes the same lock, so an epoch cannot move under this write.
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

	for _, addr := range fb.Recipients {
		addr = strings.ToLower(strings.TrimSpace(addr))
		if addr == "" {
			continue
		}
		row, matched := m.matchRecipient(rows, addr)
		if !matched {
			// Not in the authorized envelope: no accounting, no suppression.
			// A mismatched recipient on a signed event is either a provider
			// quirk or forged input; either way it proves nothing about an
			// address this account sent to.
			continue
		}

		if err := m.applyEvidence(ctx, tx, corr, row, derived.Bucket, fb, accountExists, accountID, epoch, today); err != nil {
			return delivery.FeedbackResult{}, err
		}

		if derived.Repair && accountExists {
			up, err := suppressionsync.UpsertTx(ctx, tx, "supp_"+randomSuffix(), accountID, addr,
				repairReason(fb, derived), derived.Source, "")
			if err != nil {
				return delivery.FeedbackResult{}, err
			}
			result.Suppressions = append(result.Suppressions, delivery.FeedbackSuppression{
				Address: addr, ID: up.ID, Source: derived.Source, Reason: repairReason(fb, derived), Inserted: up.Inserted,
			})
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return delivery.FeedbackResult{}, fmt.Errorf("sendingpolicy: commit feedback: %w", err)
	}
	return result, nil
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
		c, ok, err := scan(tx.QueryRow(ctx, cols+`WHERE provider_message_id = $1 LIMIT 1`, id))
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
func (m *Module) VerifyKeyringCoverage(ctx context.Context) error {
	rows, err := m.pool.Query(ctx, `
		SELECT DISTINCT r.hmac_key_version
		  FROM sending_feedback_recipients r
		  JOIN sending_feedback_correlations c ON c.correlation_id = r.correlation_id
		 WHERE c.expires_at IS NULL OR c.expires_at > now()
		 ORDER BY 1`)
	if err != nil {
		return fmt.Errorf("sendingpolicy: read retained key versions: %w", err)
	}
	defer rows.Close()
	var missing []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return err
		}
		if m.secrets.Keyring == nil || !hasVersion(m.secrets.Keyring, v) {
			missing = append(missing, v)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(missing) > 0 {
		return fmt.Errorf("sendingpolicy: retained feedback rows are signed under HMAC key version(s) %v this keyring does not hold; the keyring must stay a superset until those rows expire", missing)
	}
	return nil
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
