package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/outbound"
)

// BatchAcceptResult is the outcome of a batch-send accept-tx. Items is
// positional — Items[i] corresponds to request item i and is either
// "accepted" (MessageID non-empty) or "suppressed" (Suppressed non-nil).
// See docs/design/batch-send.md §1.3.
type BatchAcceptResult struct {
	BatchID string
	Items   []BatchAcceptItem
}

// BatchAcceptItem is the discriminated union of one slot in the batch
// response: exactly one of MessageID / Suppressed is set. MessageID
// non-empty means the accept-tx inserted a `messages` row (delivery_status
// = accepted) and enqueued a River outbound_send job for it. Suppressed
// non-nil means the item's `to` envelope contained a suppression-list
// address; no row was inserted.
type BatchAcceptItem struct {
	MessageID  string
	Suppressed *SuppressedInfo
}

// SuppressedInfo describes one dropped batch item — the first suppressed
// recipient address found in the item's `to` list plus the suppressions.
// source category (bounce / complaint / manual). Populated at accept-tx
// time from suppressions.
type SuppressedInfo struct {
	Address string
	Reason  string
}

// BatchAcceptIdemCompleter is the batch-send analog of AcceptIdemCompleter:
// a closure the handler builds to write the idempotency cache row inside
// the same tx that inserts batches + messages + enqueues jobs. Only
// invoked once on success.
type BatchAcceptIdemCompleter func(ctx context.Context, tx pgx.Tx, result *BatchAcceptResult) error

// agentUsesHITL returns true when the agent's outbound protection config
// could produce a Review verdict — i.e. some send would be held for HITL
// review. Batch-send MVP refuses these agents outright rather than
// silently breaking HITL guarantees; see docs/design/batch-send.md §5.1
// and §14 Q13 for the frozen formula and reasoning.
//
// Block-only agents (Gate.Action == "block" with Scan.Sensitivity ==
// "off") are NOT HITL — block is automated denial, no human in the loop.
// Those agents reach the batch handler and any per-item Block verdict
// fails the whole batch with 403 blocked_by_policy (§14 Q14).
func agentUsesHITL(ag *identity.AgentIdentity) bool {
	if ag == nil {
		return false
	}
	return ag.OutboundPolicyAction == "review" ||
		(ag.OutboundScanSensitivity != "" && ag.OutboundScanSensitivity != "off")
}

// DeliverBatch is the accept-tx orchestrator for batch send — the batch
// analog of DeliverOutbound. Runs §9 steps 3-10 in one shot: HITL gate,
// per-item screening, suppression partition, rate reservation, per-item
// compose, and the single DB tx that inserts the batches row + N messages
// rows + N River outbound_send jobs + the idempotency cache row.
//
// Inputs:
//   - items — one outbound.SendRequest per BatchMessage the caller
//     submitted. The handler has already run per-item structural
//     validation (RFC 5322, XOR, per-item attachment caps) AND the
//     batch-level checks (dedup, len bounds, sum-of-attachments cap) so
//     DeliverBatch can proceed without those.
//   - idemCompleteTx — closure that writes the idempotency cache row
//     inside the accept-tx; nil disables idempotency completion for this
//     request.
//
// Returns:
//   - *BatchAcceptResult with the minted BatchID + a positional Items
//     slice (one entry per input item). Empty MessageID + non-nil
//     Suppressed = dropped.
//   - *OutboundError on any all-or-nothing failure path (HITL gate,
//     block verdict on any item, rate-limit reservation, compose error,
//     tx failure). The caller maps this to the appropriate HTTP status.
//
// The whole-batch invariant: if this function returns a non-nil error,
// zero rows exist (matches the frozen accept-time all-or-nothing
// invariant in docs/design/batch-send.md §2.1).
func (a *API) DeliverBatch(
	ctx context.Context,
	user *identity.User,
	agent *identity.AgentIdentity,
	items []outbound.SendRequest,
	idemCompleteTx BatchAcceptIdemCompleter,
) (*BatchAcceptResult, *OutboundError) {
	if len(items) == 0 {
		return nil, &OutboundError{Status: http.StatusBadRequest, Code: "invalid_request", Msg: "batch must contain at least one message"}
	}

	// Step 3 (§9): HITL gate. Refuse HITL-enabled agents outright. Content
	// scan + policy=review both count as HITL per §14 Q13.
	if agentUsesHITL(agent) {
		return nil, &OutboundError{
			Status: http.StatusForbidden,
			Code:   "batch_hitl_unsupported",
			Msg:    "batch send is not available for agents with HITL enabled; use single-send POST /v1/agents/{email}/messages per recipient, or disable HITL on this agent",
		}
	}

	// Queue-wired guard (mirrors DeliverOutbound). Missing queue wiring is
	// a startup bug; fail closed rather than submit inline.
	if a.outboundEnq == nil {
		log.Printf("[api] batch: outbound queue unavailable: agent=%s items=%d", agent.Domain, len(items))
		return nil, &OutboundError{Status: http.StatusInternalServerError, Code: "internal_error", Msg: "outbound delivery queue unavailable"}
	}

	// Step 6 (§9): Batch-wide suppression check — one query for the union
	// of all `to` addresses, partition items into keep[] and suppressed[].
	// §2.2: an item is dropped as a whole if ANY of its recipients hits
	// the suppression list.
	suppressionByAddr, err := a.suppressionLookupForBatch(ctx, user.ID, items)
	if err != nil {
		// Fail-open on suppression store errors (matches
		// checkSuppression's send-time posture) — a suppression store
		// outage must not block a batch that would otherwise succeed.
		// The check is a compliance feature, not a security gate.
		log.Printf("[api] batch suppression lookup failed (allowing send): %v", err)
		suppressionByAddr = nil
	}

	// Capture the original request length BEFORE partitioning reassigns
	// items to the keep subset — the response result.Items must have one
	// slot per ORIGINAL request item (accepted + suppressed), positionally
	// aligned with what the caller submitted.
	originalCount := len(items)

	// Partition items into keep[] (proceed) and record suppressed drops.
	items, keepIndexes, suppressedItems := partitionSuppressed(items, suppressionByAddr)

	// Step 5 (§9): Per-item screening. Block verdict on ANY item fails
	// the whole batch (§14 Q14 — all-or-nothing on block, matches
	// single-send's blocked_by_policy behavior). We screen only the
	// items we would proceed with — a suppressed item is dropped by
	// compliance, not policy, so it's out of scope for screening.
	for i, item := range items {
		verdict := a.screenOutbound(ctx, agent, item)
		if verdict.Block() {
			originalIndex := keepIndexes[i]
			auditID := blockAuditID(agent.ID, item)
			a.auditRowless(ctx, agent, auditID, item, verdict)
			a.emitBlockedOutbound(agent, auditID, item, verdict)
			return nil, &OutboundError{
				Status: http.StatusForbidden,
				Code:   "blocked_by_policy",
				Msg:    fmt.Sprintf("batch item %d blocked by outbound policy", originalIndex),
			}
		}
		if verdict.Review() {
			// Should be unreachable — agentUsesHITL above short-circuits
			// review-configured agents. Defence in depth: if content
			// scan somehow lands a review verdict on a scan=off agent
			// (a config drift), refuse rather than silently hold.
			return nil, &OutboundError{
				Status: http.StatusForbidden,
				Code:   "batch_hitl_unsupported",
				Msg:    "batch item would require review; batch send is not available for agents that trigger review",
			}
		}
	}

	// Step 7 (§9): Rate/quota reservation. AllowN atomically reserves
	// len(items) slots or rejects. Suppressed items don't count (§4.3).
	// The reservation happens outside the DB tx because a partial reserve
	// (Allow reserving a slot, then tx failing) is fine — the slot ages
	// out of the window naturally, matching single-send's pre-tx check.
	if len(items) > 0 && a.sendLimit != nil {
		ok, retryAfter := a.sendLimit.AllowN(agent.ID, len(items))
		if !ok {
			secs := int(retryAfter.Seconds())
			if secs < 1 {
				secs = 1
			}
			return nil, &OutboundError{
				Status:     http.StatusTooManyRequests,
				Code:       "rate_limited",
				Msg:        fmt.Sprintf("rate limit exceeded — batch of %d sends would exceed the per-agent throughput cap (max 60/min)", len(items)),
				Details:    map[string]any{"retry_after_seconds": secs},
				RetryAfter: secs,
			}
		}
	}

	// Step 8 & 9 (§9): Per-item compose. Templates were already resolved
	// at the handler layer (matching single-send's prepare()); Compose
	// runs recipient normalize + DKIM sign + MIME assembly here.
	composed := make([]composedBatchItem, len(items))
	for i, item := range items {
		comp, cerr := a.sender.ComposeForAccept(agent, item)
		if cerr != nil {
			if outbound.IsValidationError(cerr) {
				return nil, &OutboundError{
					Status: http.StatusBadRequest,
					Code:   "invalid_request",
					Msg:    fmt.Sprintf("batch item %d: %s", keepIndexes[i], cerr.Error()),
				}
			}
			log.Printf("[api] batch compose failed: agent=%s item=%d error=%v", agent.Domain, keepIndexes[i], cerr)
			return nil, &OutboundError{
				Status: http.StatusInternalServerError,
				Code:   "internal_error",
				Msg:    fmt.Sprintf("compose failed on batch item %d: %v", keepIndexes[i], cerr),
			}
		}
		composed[i] = composedBatchItem{req: item, comp: comp}
	}

	// Step 10 (§9): Accept-tx. All-or-nothing DB commit:
	//   a. batches header row
	//   b. bulk-insert N messages rows (delivery_status='accepted', batch_id set)
	//   c. River InsertManyTx enqueues N outbound_send jobs
	//   d. UPDATE messages SET send_job_id per row (loop; N ≤ 100)
	//   e. idempotency cache row
	batchID := identity.NewBatchID()
	suppressedJSON, err := marshalSuppressedItems(suppressedItems)
	if err != nil {
		log.Printf("[api] batch: marshal suppressed_json failed: %v", err)
		return nil, &OutboundError{Status: http.StatusInternalServerError, Code: "internal_error", Msg: "failed to serialize suppression records"}
	}

	result := &BatchAcceptResult{
		BatchID: batchID,
		Items:   make([]BatchAcceptItem, originalCount),
	}

	if txErr := a.store.WithTx(ctx, func(tx pgx.Tx) error {
		if err := a.store.CreateBatchTx(ctx, tx, &identity.Batch{
			BatchID:        batchID,
			UserID:         user.ID,
			AgentID:        agent.ID,
			Requested:      len(items) + len(suppressedItems),
			Accepted:       len(items),
			SuppressedJSON: suppressedJSON,
		}); err != nil {
			return fmt.Errorf("create batch: %w", err)
		}

		if len(composed) == 0 {
			// All-suppressed batch (§14 Q9) — still a valid 202. No
			// messages to insert, no jobs to enqueue, but the batches
			// header + idempotency completion still commit atomically.
			if idemCompleteTx != nil {
				if err := idemCompleteTx(ctx, tx, result); err != nil {
					return fmt.Errorf("idempotency complete: %w", err)
				}
			}
			return nil
		}

		inputs := make([]identity.OutboundMessageInput, len(composed))
		for i, c := range composed {
			inputs[i] = identity.OutboundMessageInput{
				AgentID:        agent.ID,
				ToRecipients:   c.comp.To,
				CC:             c.comp.CC,
				BCC:            c.comp.BCC,
				Subject:        c.req.Subject,
				MsgType:        "send",
				Method:         c.comp.Method,
				ConversationID: c.req.ConversationID,
				RawMessage:     c.comp.Raw,
				DeliveryStatus: "accepted",
				EnvelopeFrom:   c.comp.EnvelopeFrom,
				SentAs:         c.comp.SentAs,
				BatchID:        batchID,
			}
		}
		msgs, err := a.store.CreateOutboundMessagesTx(ctx, tx, inputs)
		if err != nil {
			return fmt.Errorf("bulk insert messages: %w", err)
		}
		msgIDs := make([]string, len(msgs))
		for i, m := range msgs {
			msgIDs[i] = m.ID
		}
		jobIDs, err := a.outboundEnq.EnqueueBatchTx(ctx, tx, msgIDs)
		if err != nil {
			return fmt.Errorf("enqueue batch jobs: %w", err)
		}
		// Stamp send_job_id per message (loop — no bulk stamp helper today,
		// and N ≤ 100 is well within the accept-tx budget).
		for i, jobID := range jobIDs {
			if err := a.store.StampSendJobIDTx(ctx, tx, msgIDs[i], jobID); err != nil {
				return fmt.Errorf("stamp send_job_id item %d: %w", i, err)
			}
		}

		// Fill in Items[i] for accepted slots.
		for i, id := range msgIDs {
			originalIndex := keepIndexes[i]
			result.Items[originalIndex] = BatchAcceptItem{MessageID: id}
		}

		if idemCompleteTx != nil {
			if err := idemCompleteTx(ctx, tx, result); err != nil {
				return fmt.Errorf("idempotency complete: %w", err)
			}
		}
		return nil
	}); txErr != nil {
		log.Printf("[api] batch accept tx failed: agent=%s items=%d error=%v", agent.Domain, len(items), txErr)
		return nil, &OutboundError{Status: http.StatusInternalServerError, Code: "internal_error", Msg: "failed to accept batch for send"}
	}

	// Fill in suppressed slots on result.Items (accepted slots were
	// filled inside the tx).
	for _, drop := range suppressedItems {
		result.Items[drop.ItemIndex] = BatchAcceptItem{
			Suppressed: &SuppressedInfo{Address: drop.Address, Reason: drop.Reason},
		}
	}

	log.Printf("[batch:%s] agent=%s accepted=%d suppressed=%d", batchID, agent.EmailAddress(), len(composed), len(suppressedItems))
	return result, nil
}

// composedBatchItem carries a batch item's composed MIME + its original
// SendRequest through the accept-tx step. Kept small — no need to hold on
// to per-item state beyond what CreateOutboundMessagesTx and
// StampSendJobIDTx need.
type composedBatchItem struct {
	req  outbound.SendRequest
	comp *outbound.ComposeResult
}

// suppressedDrop is the internal per-item drop record used to build the
// batches.suppressed_json blob and the response result.Items slot. Field
// names match identity.BatchSuppressedItem so JSON marshaling is a direct
// re-serialization.
type suppressedDrop struct {
	ItemIndex int    `json:"item_index"`
	Address   string `json:"address"`
	Reason    string `json:"reason"`
}

// suppressionLookupForBatch queries the suppression list ONCE for the
// union of every batch item's `to` addresses and returns a map of
// address → source. On error the caller is expected to fail-open (see
// DeliverBatch's use-site).
func (a *API) suppressionLookupForBatch(ctx context.Context, userID string, items []outbound.SendRequest) (map[string]string, error) {
	all := make([]string, 0)
	seen := map[string]struct{}{}
	for _, item := range items {
		for _, addr := range item.To {
			if _, ok := seen[addr]; ok {
				continue
			}
			seen[addr] = struct{}{}
			all = append(all, addr)
		}
	}
	if len(all) == 0 {
		return map[string]string{}, nil
	}
	return a.store.SuppressedAddressesWithSource(ctx, userID, all)
}

// partitionSuppressed splits items into keep[] (unaffected) and dropped[]
// (any `to` address in the suppression map). Returns:
//   - keep: the subset of items that proceed
//   - keepIndexes: original positions in the input, positionally aligned
//     with keep (keep[i] came from items[keepIndexes[i]])
//   - dropped: one suppressedDrop per removed item, with its original
//     ItemIndex + the first suppressed address that caused the drop
func partitionSuppressed(items []outbound.SendRequest, suppressionByAddr map[string]string) (
	keep []outbound.SendRequest, keepIndexes []int, dropped []suppressedDrop,
) {
	keep = make([]outbound.SendRequest, 0, len(items))
	keepIndexes = make([]int, 0, len(items))
	if len(suppressionByAddr) == 0 {
		// No suppressed addresses — every item is kept, no drops. Fast path.
		for i, item := range items {
			keep = append(keep, item)
			keepIndexes = append(keepIndexes, i)
		}
		return keep, keepIndexes, nil
	}
	for i, item := range items {
		var hit string
		for _, addr := range item.To {
			if _, ok := suppressionByAddr[identity.NormalizeEmail(addr)]; ok {
				hit = addr
				break
			}
		}
		if hit != "" {
			reason := suppressionByAddr[identity.NormalizeEmail(hit)]
			if reason == "" {
				reason = "manual"
			}
			dropped = append(dropped, suppressedDrop{ItemIndex: i, Address: hit, Reason: reason})
			continue
		}
		keep = append(keep, item)
		keepIndexes = append(keepIndexes, i)
	}
	return keep, keepIndexes, dropped
}

// marshalSuppressedItems produces the JSONB payload for
// batches.suppressed_json — a stable array shape mirroring the wire slot
// in SendBatchResponse.results[i].suppressed. Empty input → the
// canonical '[]' bytes, matching the DEFAULT on the column.
func marshalSuppressedItems(drops []suppressedDrop) ([]byte, error) {
	if len(drops) == 0 {
		return []byte("[]"), nil
	}
	return json.Marshal(drops)
}
