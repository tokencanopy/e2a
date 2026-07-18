package httpapi

import "net/http"

// Emitter helpers for the three batch-send error codes registered in
// error_catalog.go. Kept here so the /v1 error-vocabulary test
// (TestErrorCodeVocabularyMatchesCatalog) sees a literal Code: string for each
// batch code; the batch handler lands in a later commit and will call these.
// See docs/design/batch-send.md §1.4, §5.1, §14 (Q11/Q15) for the contract.

// errBatchHITLUnsupported is emitted when a batch-send request targets an agent
// whose outbound protection could produce a Review verdict — i.e. HITL is
// enabled per §14 Q13 (OutboundPolicyAction=="review" OR
// OutboundScanSensitivity!="off"). MVP refuses batch send for these agents so
// the batch primitive does not silently break HITL guarantees (§5.1); the
// caller must use single-send per recipient or disable HITL.
func errBatchHITLUnsupported() *ErrorEnvelope {
	return NewError(http.StatusForbidden, "batch_hitl_unsupported",
		"batch send is not available for agents with HITL enabled")
}

// errTooManyMessages is emitted when a batch-send request's messages[] array
// has more items than the per-request cap (100 for MVP — see §14 Q5) or fewer
// than 1. Distinct from too_many_recipients, which caps recipients WITHIN a
// single message.
func errTooManyMessages(provided int) *ErrorEnvelope {
	return NewError(http.StatusBadRequest, "too_many_messages", "too many messages in batch").
		WithDetails(TooManyMessagesDetails{MaxMessages: maxBatchMessages, Provided: provided})
}

// errDuplicateRecipient is emitted when the same recipient address appears in
// the `to` set of more than one item in a batch request. Silent deduplication
// would hide caller-side bugs — see §14 Q11.
func errDuplicateRecipient(address string, itemIndices []int) *ErrorEnvelope {
	return NewError(http.StatusBadRequest, "duplicate_recipient",
		"same recipient address appears in more than one batch item").
		WithDetails(DuplicateRecipientDetails{Address: address, ItemIndices: itemIndices})
}

// maxBatchMessages is the per-request cap on len(messages) for POST /v1/agents/{email}/batches.
// See docs/design/batch-send.md §14 Q5. Kept here (not in the handler file) so
// the errTooManyMessages helper — and its future call sites — share the single
// source of truth for the cap constant.
const maxBatchMessages = 100
