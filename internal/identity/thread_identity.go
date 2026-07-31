package identity

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/mail"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tokencanopy/e2a/internal/emailauth"
	"github.com/tokencanopy/e2a/internal/rfcmessageid"
)

type messageThreadAssignment struct {
	threadID          string
	threadParentID    string
	rfcMessageIDKey   string
	resolutionSource  string
	diagnosticSources []string
}

// ThreadIdentityMetrics is deliberately narrower than telemetry.Metrics so
// identity does not depend on a concrete observability package.
type ThreadIdentityMetrics interface {
	ThreadResolution(source string, count int)
}

const threadAmbiguityLogInterval = time.Minute

var threadAmbiguityNextLogUnix atomic.Int64

// maybeLogThreadAmbiguity emits only bounded counts. The candidate RFC IDs,
// addresses, subjects, and content are intentionally unavailable to the log
// call. A process-wide one-minute gate keeps repeated hostile/duplicate
// anchors from flooding routine logs.
func maybeLogThreadAmbiguity(now time.Time, candidates, distinctThreads int) bool {
	nowUnix := now.Unix()
	for {
		next := threadAmbiguityNextLogUnix.Load()
		if nowUnix < next {
			return false
		}
		if threadAmbiguityNextLogUnix.CompareAndSwap(
			next, now.Add(threadAmbiguityLogInterval).Unix(),
		) {
			log.Printf(
				"[thread_identity] resolution=ambiguous_anchor candidates=%d distinct_threads=%d",
				candidates, distinctThreads,
			)
			return true
		}
	}
}

func (s *Store) recordThreadResolution(source string, count int) {
	if s.threadIdentityMetrics != nil && source != "" && count > 0 {
		s.threadIdentityMetrics.ThreadResolution(source, count)
	}
}

func (s *Store) recordThreadAssignment(assignment messageThreadAssignment) {
	s.recordThreadResolution(assignment.resolutionSource, 1)
	for _, source := range assignment.diagnosticSources {
		s.recordThreadResolution(source, 1)
	}
}

// RFCMessageIDCandidate preserves the exact wire token for legacy exact-match
// fallback while carrying its canonical lookup key for new rows.
type RFCMessageIDCandidate struct {
	Original  string
	Canonical string
}

// InboundThreadEvidence is prepared from authenticated inbound headers before
// the persistence transaction. Candidate ownership and consensus are always
// re-evaluated by the store inside that transaction.
type InboundThreadEvidence struct {
	InReplyTo            []RFCMessageIDCandidate
	References           []RFCMessageIDCandidate
	DeliveryTwinSourceID string
}

func freshMessageThread(rfcMessageIDKey string) messageThreadAssignment {
	return messageThreadAssignment{
		threadID:        NewThreadID(),
		rfcMessageIDKey: rfcMessageIDKey,
	}
}

func freshInboundMessageThread(messageID string) messageThreadAssignment {
	key := canonicalRFCMessageIDKey(messageID)
	return freshMessageThread(key)
}

func canonicalRFCMessageIDKey(messageID string) string {
	key, err := rfcmessageid.Canonicalize(messageID)
	if err != nil {
		return ""
	}
	return key
}

// EnsureThreadTx returns the immutable mailbox-local thread for messageID,
// lazily assigning one to an exact legacy anchor while holding its row lock.
// Hidden and soft-deleted rows remain eligible anchors; ownership never crosses
// agentID.
func (s *Store) EnsureThreadTx(ctx context.Context, tx pgx.Tx, agentID, messageID string) (string, error) {
	var direction, emailMessageID, providerMessageID, threadID, rfcKey string
	err := tx.QueryRow(ctx,
		`SELECT direction,
		        COALESCE(email_message_id, ''),
		        COALESCE(provider_message_id, ''),
		        COALESCE(thread_id, ''),
		        COALESCE(rfc_message_id_key, '')
		   FROM messages
		  WHERE id = $1 AND agent_id = $2
		  FOR UPDATE`,
		messageID, agentID,
	).Scan(&direction, &emailMessageID, &providerMessageID, &threadID, &rfcKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrMessageNotFound
	}
	if err != nil {
		return "", err
	}
	if threadID != "" && !IsValidThreadID(threadID) {
		return "", fmt.Errorf("message %s has invalid thread_id", messageID)
	}

	if rfcKey == "" {
		wireID := emailMessageID
		if direction == "outbound" {
			wireID = providerMessageID
		}
		if canonical, canonicalErr := rfcmessageid.Canonicalize(wireID); canonicalErr == nil {
			rfcKey = canonical
		}
	}
	lazilyAdopted := threadID == ""
	if lazilyAdopted {
		threadID = NewThreadID()
	}

	if _, err := tx.Exec(ctx,
		`UPDATE messages
		    SET thread_id = COALESCE(thread_id, $3),
		        rfc_message_id_key = CASE
		          WHEN rfc_message_id_key IS NULL AND $4 <> '' THEN $4
		          ELSE rfc_message_id_key
		        END
		  WHERE id = $1 AND agent_id = $2`,
		messageID, agentID, threadID, rfcKey,
	); err != nil {
		return "", err
	}
	if lazilyAdopted {
		s.recordThreadResolution("lazy_legacy_anchor", 1)
	}
	return threadID, nil
}

// CreateOutboundMessageThreadedTx persists an outbound message with its
// mailbox-local topology decision in the caller's transaction. Only a reply
// operation may inherit parentMessageID; sends and forwards always start a new
// thread even if a source resource or conversation_id is reused.
func (s *Store) CreateOutboundMessageThreadedTx(
	ctx context.Context,
	tx pgx.Tx,
	parentMessageID string,
	agentID string,
	toRecipients, cc, bcc []string,
	subject, msgType, method, providerMessageID, conversationID string,
	rawMessage []byte,
	deliveryStatus, envelopeFrom, sentAs string,
) (*Message, error) {
	assignment, err := s.outboundThreadAssignmentTx(ctx, tx, agentID, msgType, parentMessageID, providerMessageID)
	if err != nil {
		return nil, err
	}
	message, err := s.createOutboundMessageAssignedTx(
		ctx, tx, assignment, agentID, toRecipients, cc, bcc, subject, msgType,
		method, providerMessageID, conversationID, rawMessage, deliveryStatus,
		envelopeFrom, sentAs,
	)
	if err == nil {
		s.recordThreadAssignment(assignment)
	}
	return message, err
}

func (s *Store) outboundThreadAssignmentTx(ctx context.Context, tx pgx.Tx, agentID, msgType, parentMessageID, ownProviderMessageID string) (messageThreadAssignment, error) {
	assignment := freshMessageThread("")
	switch {
	case msgType == "reply" && parentMessageID != "":
		assignment.resolutionSource = "api_reply"
		parentThreadID, err := s.EnsureThreadTx(ctx, tx, agentID, parentMessageID)
		if err != nil {
			return messageThreadAssignment{}, err
		}
		assignment.threadID = parentThreadID
		assignment.threadParentID = parentMessageID
	case msgType == "forward":
		assignment.resolutionSource = "forward"
	case msgType == "reply":
		assignment.resolutionSource = "no_anchor"
	default:
		assignment.resolutionSource = "fresh_send"
	}
	if canonical, err := rfcmessageid.Canonicalize(ownProviderMessageID); err == nil {
		assignment.rfcMessageIDKey = canonical
	}
	return assignment, nil
}

type threadAnchorRow struct {
	id       string
	threadID string
}

// resolveInboundThreadTx applies immediate-parent then References precedence.
// It never merges established threads and only assigns a direct parent when a
// single exact row was selected.
func (s *Store) resolveInboundThreadTx(ctx context.Context, tx pgx.Tx, agentID, recipient, ownMessageID string, auth InboundAuth, evidence InboundThreadEvidence) (messageThreadAssignment, error) {
	ownKey, _ := rfcmessageid.Canonicalize(ownMessageID)
	diagnostics := make([]string, 0, 4)

	if evidence.DeliveryTwinSourceID != "" {
		assignment, matched, err := s.resolveAuthenticatedDeliveryTwinTx(
			ctx, tx, agentID, recipient, ownKey, auth, evidence.DeliveryTwinSourceID,
		)
		if err != nil {
			return messageThreadAssignment{}, err
		}
		if matched {
			assignment.resolutionSource = "authenticated_delivery_twin"
			return assignment, nil
		}
	}

	type sourcedCandidate struct {
		RFCMessageIDCandidate
		source string
	}
	candidates := make([]sourcedCandidate, 0, len(evidence.InReplyTo)+len(evidence.References))
	for i := len(evidence.InReplyTo) - 1; i >= 0; i-- {
		candidates = append(candidates, sourcedCandidate{
			RFCMessageIDCandidate: evidence.InReplyTo[i],
			source:                "rfc_in_reply_to",
		})
	}
	for i := len(evidence.References) - 1; i >= 0; i-- {
		candidates = append(candidates, sourcedCandidate{
			RFCMessageIDCandidate: evidence.References[i],
			source:                "rfc_references",
		})
	}

	for _, candidate := range candidates {
		canonical, err := rfcmessageid.Canonicalize(candidate.Canonical)
		if err != nil {
			continue
		}
		rows, err := s.findCanonicalThreadAnchorsTx(ctx, tx, agentID, canonical)
		if err != nil {
			return messageThreadAssignment{}, err
		}
		legacyRows, err := s.findLegacyThreadAnchorsTx(ctx, tx, agentID, candidate.Original, canonical)
		if err != nil {
			return messageThreadAssignment{}, err
		}
		rows = mergeThreadAnchorRows(rows, legacyRows)
		if len(rows) == 0 {
			diagnostics = append(diagnostics, "legacy_anchor_unmatched")
			continue
		}

		threads := make(map[string]struct{})
		foundThreadless := false
		for _, row := range rows {
			if row.threadID == "" {
				foundThreadless = true
				continue
			}
			if !IsValidThreadID(row.threadID) {
				return messageThreadAssignment{}, fmt.Errorf("message %s has invalid thread_id", row.id)
			}
			threads[row.threadID] = struct{}{}
		}
		if foundThreadless {
			diagnostics = append(diagnostics, "anchor_found_without_thread")
		}
		if len(threads) > 1 {
			diagnostics = append(diagnostics, "ambiguous_anchor")
			maybeLogThreadAmbiguity(time.Now(), len(rows), len(threads))
			continue
		}
		if len(threads) == 1 {
			var threadID string
			for id := range threads {
				threadID = id
			}
			parentID := ""
			if len(rows) == 1 {
				lockedThread, ensureErr := s.EnsureThreadTx(ctx, tx, agentID, rows[0].id)
				if errors.Is(ensureErr, ErrMessageNotFound) {
					continue
				}
				if ensureErr != nil {
					return messageThreadAssignment{}, ensureErr
				}
				threadID = lockedThread
				parentID = rows[0].id
			}
			return messageThreadAssignment{
				threadID:          threadID,
				threadParentID:    parentID,
				rfcMessageIDKey:   ownKey,
				resolutionSource:  candidate.source,
				diagnosticSources: diagnostics,
			}, nil
		}

		if len(rows) == 1 {
			threadID, ensureErr := s.EnsureThreadTx(ctx, tx, agentID, rows[0].id)
			if errors.Is(ensureErr, ErrMessageNotFound) {
				continue
			}
			if ensureErr != nil {
				return messageThreadAssignment{}, ensureErr
			}
			return messageThreadAssignment{
				threadID:          threadID,
				threadParentID:    rows[0].id,
				rfcMessageIDKey:   ownKey,
				resolutionSource:  candidate.source,
				diagnosticSources: diagnostics,
			}, nil
		}
		// Several all-null matches are ambiguous. Leave them untouched and try
		// an older References candidate.
		diagnostics = append(diagnostics, "ambiguous_anchor")
		maybeLogThreadAmbiguity(time.Now(), len(rows), 0)
	}
	assignment := freshMessageThread(ownKey)
	assignment.resolutionSource = "no_anchor"
	assignment.diagnosticSources = diagnostics
	return assignment, nil
}

func (s *Store) resolveAuthenticatedDeliveryTwinTx(ctx context.Context, tx pgx.Tx, agentID, recipient, ownKey string, auth InboundAuth, sourceMessageID string) (messageThreadAssignment, bool, error) {
	if NormalizeEmail(recipient) == "" ||
		ownKey == "" ||
		auth.Authentication == nil ||
		auth.Authentication.SPF.Status != emailauth.StatusPass ||
		auth.Authentication.SPF.Domain == nil {
		return messageThreadAssignment{}, false, nil
	}
	authenticatedDomain := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(*auth.Authentication.SPF.Domain), "."))
	if authenticatedDomain == "" {
		return messageThreadAssignment{}, false, nil
	}

	var msgType, envelopeFrom, providerMessageID, sourceKey string
	var toRecipients []string
	err := tx.QueryRow(ctx,
		`SELECT COALESCE(message_type, ''),
		        COALESCE(envelope_from, ''),
		        COALESCE(provider_message_id, ''),
		        COALESCE(rfc_message_id_key, ''),
		        to_recipients
		   FROM messages
		  WHERE id = $1
		    AND agent_id = $2
		    AND direction = 'outbound'
		  FOR UPDATE`,
		sourceMessageID, agentID,
	).Scan(&msgType, &envelopeFrom, &providerMessageID, &sourceKey, &toRecipients)
	if errors.Is(err, pgx.ErrNoRows) {
		return messageThreadAssignment{}, false, nil
	}
	if err != nil {
		return messageThreadAssignment{}, false, err
	}
	if msgType != "test" || !containsNormalizedAddress(toRecipients, recipient) {
		return messageThreadAssignment{}, false, nil
	}
	sourceDomain := addressDomain(envelopeFrom)
	if sourceDomain == "" || !sameOrSubdomain(authenticatedDomain, sourceDomain) {
		return messageThreadAssignment{}, false, nil
	}

	if sourceKey != "" {
		canonical, canonicalErr := rfcmessageid.Canonicalize(sourceKey)
		if canonicalErr != nil || canonical != ownKey {
			return messageThreadAssignment{}, false, nil
		}
	} else if providerMessageID != "" {
		canonical, canonicalErr := rfcmessageid.Canonicalize(providerMessageID)
		if canonicalErr != nil || canonical != ownKey {
			return messageThreadAssignment{}, false, nil
		}
	}

	threadID, err := s.EnsureThreadTx(ctx, tx, agentID, sourceMessageID)
	if err != nil {
		if errors.Is(err, ErrMessageNotFound) {
			return messageThreadAssignment{}, false, nil
		}
		return messageThreadAssignment{}, false, err
	}
	return messageThreadAssignment{
		threadID:        threadID,
		rfcMessageIDKey: ownKey,
	}, true, nil
}

func addressDomain(address string) string {
	if parsed, err := mail.ParseAddress(address); err == nil {
		address = parsed.Address
	}
	at := strings.LastIndexByte(address, '@')
	if at < 0 || at == len(address)-1 {
		return ""
	}
	return strings.ToLower(strings.TrimSuffix(address[at+1:], "."))
}

func sameOrSubdomain(candidate, parent string) bool {
	return candidate == parent || strings.HasSuffix(candidate, "."+parent)
}

func containsNormalizedAddress(addresses []string, target string) bool {
	target = NormalizeEmail(target)
	for _, address := range addresses {
		if NormalizeEmail(address) == target {
			return true
		}
	}
	return false
}

func (s *Store) findCanonicalThreadAnchorsTx(ctx context.Context, tx pgx.Tx, agentID, canonical string) ([]threadAnchorRow, error) {
	rows, err := tx.Query(ctx,
		`SELECT id, COALESCE(thread_id, '')
		   FROM messages
		  WHERE agent_id = $1 AND rfc_message_id_key = $2
		  ORDER BY created_at, id`,
		agentID, canonical,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanThreadAnchorRows(rows)
}

func (s *Store) findLegacyThreadAnchorsTx(ctx context.Context, tx pgx.Tx, agentID, original, canonical string) ([]threadAnchorRow, error) {
	keys := []string{original}
	if canonical != original {
		keys = append(keys, canonical)
	}
	found := make(map[string]threadAnchorRow)
	for _, query := range []string{
		`SELECT id, COALESCE(thread_id, '')
		   FROM messages
		  WHERE agent_id = $1
		    AND direction = 'inbound'
		    AND email_message_id <> ''
		    AND email_message_id = ANY($2)
		  ORDER BY created_at, id`,
		`SELECT id, COALESCE(thread_id, '')
		   FROM messages
		  WHERE agent_id = $1
		    AND direction = 'outbound'
		    AND provider_message_id <> ''
		    AND provider_message_id = ANY($2)
		  ORDER BY created_at, id`,
	} {
		rows, err := tx.Query(ctx, query, agentID, keys)
		if err != nil {
			return nil, err
		}
		scanned, scanErr := scanThreadAnchorRows(rows)
		rows.Close()
		if scanErr != nil {
			return nil, scanErr
		}
		for _, row := range scanned {
			found[row.id] = row
		}
	}
	out := make([]threadAnchorRow, 0, len(found))
	for _, row := range found {
		out = append(out, row)
	}
	return out, nil
}

type threadAnchorRows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}

func scanThreadAnchorRows(rows threadAnchorRows) ([]threadAnchorRow, error) {
	var out []threadAnchorRow
	for rows.Next() {
		var row threadAnchorRow
		if err := rows.Scan(&row.id, &row.threadID); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func mergeThreadAnchorRows(groups ...[]threadAnchorRow) []threadAnchorRow {
	seen := make(map[string]struct{})
	var out []threadAnchorRow
	for _, group := range groups {
		for _, row := range group {
			if _, ok := seen[row.id]; ok {
				continue
			}
			seen[row.id] = struct{}{}
			out = append(out, row)
		}
	}
	return out
}
