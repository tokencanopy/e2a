package identity

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/mail"
	"sort"
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

func (s *Store) recordThreadResolutionTx(tx pgx.Tx, source string, count int) {
	if s.threadIdentityMetrics == nil || source == "" || count <= 0 {
		return
	}
	registerPostCommit(tx, func() {
		s.recordThreadResolution(source, count)
	})
}

func (s *Store) recordThreadAssignment(assignment messageThreadAssignment) {
	s.recordThreadResolution(assignment.resolutionSource, 1)
	for _, source := range assignment.diagnosticSources {
		s.recordThreadResolution(source, 1)
	}
}

func (s *Store) recordThreadAssignmentTx(tx pgx.Tx, assignment messageThreadAssignment) {
	if s.threadIdentityMetrics == nil {
		return
	}
	assignment.diagnosticSources = append([]string(nil), assignment.diagnosticSources...)
	registerPostCommit(tx, func() {
		s.recordThreadAssignment(assignment)
	})
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
		s.recordThreadResolutionTx(tx, "lazy_legacy_anchor", 1)
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
		s.recordThreadAssignmentTx(tx, assignment)
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

// MaxThreadAnchorMatches is the largest exact-match set the synchronous
// resolver will inspect for one RFC identifier. The N+1 lookup detects
// overflow; an overflowing identifier is ambiguous and resolution continues
// to an older candidate. Normal platform twins need two rows.
const MaxThreadAnchorMatches = 16

type sourcedThreadCandidate struct {
	RFCMessageIDCandidate
	source string
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

	candidates := prepareInboundThreadCandidates(evidence)
	initialMatches, err := s.findThreadAnchorsBatchTx(ctx, tx, agentID, candidates)
	if err != nil {
		return messageThreadAssignment{}, err
	}
	if err := lockInitiallyThreadlessAnchorsTx(ctx, tx, agentID, initialMatches); err != nil {
		return messageThreadAssignment{}, err
	}
	// Re-read after the deterministic lock. A concurrent EnsureThreadTx that
	// started first may have changed a null row while we waited; consensus must
	// use that committed value rather than the initial snapshot.
	matches, err := s.findThreadAnchorsBatchTx(ctx, tx, agentID, candidates)
	if err != nil {
		return messageThreadAssignment{}, err
	}

	for i, candidate := range candidates {
		rows := matches[i]
		if len(rows) == 0 {
			diagnostics = append(diagnostics, "legacy_anchor_unmatched")
			continue
		}
		if len(rows) > MaxThreadAnchorMatches {
			diagnostics = append(diagnostics, "ambiguous_anchor")
			maybeLogThreadAmbiguity(time.Now(), len(rows), countAnchorThreads(rows))
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

func prepareInboundThreadCandidates(evidence InboundThreadEvidence) []sourcedThreadCandidate {
	capacity := min(len(evidence.InReplyTo), rfcmessageid.MaxTokens) +
		min(len(evidence.References), rfcmessageid.MaxTokens)
	candidates := make([]sourcedThreadCandidate, 0, capacity)
	appendField := func(field []RFCMessageIDCandidate, source string) {
		seen := make(map[string]struct{}, min(len(field), rfcmessageid.MaxTokens))
		first := max(0, len(field)-rfcmessageid.MaxTokens)
		for i := len(field) - 1; i >= first && len(seen) < rfcmessageid.MaxTokens; i-- {
			candidate := field[i]
			canonical, err := rfcmessageid.Canonicalize(candidate.Canonical)
			if err != nil {
				continue
			}
			if _, duplicate := seen[canonical]; duplicate {
				continue
			}
			seen[canonical] = struct{}{}
			candidate.Canonical = canonical
			candidates = append(candidates, sourcedThreadCandidate{
				RFCMessageIDCandidate: candidate,
				source:                source,
			})
		}
	}
	appendField(evidence.InReplyTo, "rfc_in_reply_to")
	appendField(evidence.References, "rfc_references")
	return candidates
}

func countAnchorThreads(rows []threadAnchorRow) int {
	threads := make(map[string]struct{})
	for _, row := range rows {
		if row.threadID != "" {
			threads[row.threadID] = struct{}{}
		}
	}
	return len(threads)
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

// findThreadAnchorsBatchTx resolves every requested RFC candidate in one SQL
// round trip. Each of the canonical, legacy-inbound, and legacy-outbound exact
// index probes is capped at N+1, then their union is deduplicated and capped at
// N+1 again. Returning N+1 rows marks that candidate ambiguous without ever
// materializing an attacker-controlled duplicate set.
func (s *Store) findThreadAnchorsBatchTx(ctx context.Context, tx pgx.Tx, agentID string, candidates []sourcedThreadCandidate) ([][]threadAnchorRow, error) {
	matches := make([][]threadAnchorRow, len(candidates))
	if len(candidates) == 0 {
		return matches, nil
	}
	ordinals := make([]int32, len(candidates))
	originals := make([]string, len(candidates))
	canonicals := make([]string, len(candidates))
	for i, candidate := range candidates {
		ordinals[i] = int32(i)
		originals[i] = candidate.Original
		canonicals[i] = candidate.Canonical
	}
	rows, err := tx.Query(ctx,
		`WITH requested AS (
		   SELECT *
		     FROM unnest($2::integer[], $3::text[], $4::text[])
		          AS input(ordinal, original, canonical)
		 )
		 SELECT requested.ordinal, matched.id, matched.thread_id
		   FROM requested
		   CROSS JOIN LATERAL (
		     SELECT candidate.id, candidate.thread_id
		       FROM (
		         (SELECT m.id, COALESCE(m.thread_id, '') AS thread_id
		            FROM messages m
		           WHERE m.agent_id = $1
		             AND m.rfc_message_id_key = requested.canonical
		           LIMIT $5)
		         UNION ALL
		         (SELECT m.id, COALESCE(m.thread_id, '') AS thread_id
		            FROM messages m
		           WHERE m.agent_id = $1
		             AND m.direction = 'inbound'
		             AND m.email_message_id <> ''
		             AND (m.email_message_id = requested.original
		                  OR m.email_message_id = requested.canonical)
		           LIMIT $5)
		         UNION ALL
		         (SELECT m.id, COALESCE(m.thread_id, '') AS thread_id
		            FROM messages m
		           WHERE m.agent_id = $1
		             AND m.direction = 'outbound'
		             AND m.provider_message_id <> ''
		             AND (m.provider_message_id = requested.original
		                  OR m.provider_message_id = requested.canonical)
		           LIMIT $5)
		       ) AS candidate
		      GROUP BY candidate.id, candidate.thread_id
		      ORDER BY candidate.id
		      LIMIT $5
		   ) AS matched
		  ORDER BY requested.ordinal, matched.id`,
		agentID, ordinals, originals, canonicals, MaxThreadAnchorMatches+1,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var ordinal int
		var row threadAnchorRow
		if err := rows.Scan(&ordinal, &row.id, &row.threadID); err != nil {
			return nil, err
		}
		if ordinal < 0 || ordinal >= len(matches) {
			return nil, fmt.Errorf("thread anchor lookup returned invalid ordinal %d", ordinal)
		}
		matches[ordinal] = append(matches[ordinal], row)
	}
	return matches, rows.Err()
}

func lockInitiallyThreadlessAnchorsTx(ctx context.Context, tx pgx.Tx, agentID string, matches [][]threadAnchorRow) error {
	unique := make(map[string]struct{})
	for _, candidateRows := range matches {
		if len(candidateRows) > MaxThreadAnchorMatches {
			continue
		}
		for _, row := range candidateRows {
			if row.threadID == "" {
				unique[row.id] = struct{}{}
			}
		}
	}
	if len(unique) == 0 {
		return nil
	}
	ids := make([]string, 0, len(unique))
	for id := range unique {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	rows, err := tx.Query(ctx,
		`SELECT id
		   FROM messages
		  WHERE agent_id = $1
		    AND id = ANY($2)
		    AND thread_id IS NULL
		  ORDER BY id
		  FOR UPDATE`,
		agentID, ids,
	)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
	}
	return rows.Err()
}
