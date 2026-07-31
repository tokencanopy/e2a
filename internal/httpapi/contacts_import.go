package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/tokencanopy/e2a/internal/identity"
)

// Bulk contact import. Synchronous and bounded by design: the real workload is
// hundreds of rows, so a capped single request avoids an entire async job,
// status-polling, and partial-progress surface. Going async later is a new
// endpoint, not a change to this one.
//
// Two properties are load-bearing and tested rather than asserted in prose:
// import never sends anything, and it never weakens consent. A suppressed
// address is imported and MARKED, so the count a user sees stays honest.
const (
	maxContactImportRows      = 1000
	maxContactImportBodyBytes = 20 << 20

	contactImportBetaDescription = "Beta: the contact import surface may change before it is declared stable."
)

// ContactImportRow is one row of an upload.
type ContactImportRow struct {
	Address     string         `json:"address" required:"true" maxLength:"320" doc:"Email address. Accepts a bare address or an RFC 5322 mailbox (\"A. Partner <partner@fund.vc>\")."`
	DisplayName *string        `json:"display_name,omitempty" maxLength:"320" doc:"Optional human-readable name. Omit it and an existing contact keeps the name it already has — so a narrower re-upload that drops the name column does not erase names. Send an explicit empty string to clear one."`
	Metadata    map[string]any `json:"metadata,omitempty" doc:"Optional caller-owned key/value data, opaque to e2a. Flat objects only; the same per-contact bounds apply, and a row that exceeds them fails on its own without affecting the rest of the batch."`
}

// ImportContactsRequest is one upload.
type ImportContactsRequest struct {
	Contacts   []ContactImportRow `json:"contacts" required:"true" nullable:"false" minItems:"1" maxItems:"1000" doc:"The rows to import. At most 1000 per request; paginate client-side for larger lists."`
	OnConflict string             `json:"on_conflict,omitempty" enum:"merge,skip" default:"merge" doc:"What to do when the address already exists. merge (default) refreshes display_name and metadata and leaves provenance and any state hanging off the contact untouched — so re-uploading a corrected spreadsheet is safe. skip leaves the existing contact completely alone."`
	AgentEmail string             `json:"agent_email,omitempty" maxLength:"320" doc:"Optionally enroll every valid resolved contact with this owned, live agent in the same transaction. Existing engagement state is preserved."`
	Stage      string             `json:"stage,omitempty" maxLength:"128" doc:"Initial opaque stage for engagements created by this import. Requires agent_email and never overwrites an existing engagement's stage."`
}

type importContactsInput struct {
	IdempotencyKey string `header:"Idempotency-Key" doc:"Optional idempotency key for safe retries (unique per logical request). A retry with the same key and byte-identical body replays the first response — including the original batch_id and per-row results — instead of importing the rows a second time. Strongly recommended: an import that times out after the rows landed is otherwise indistinguishable from one that failed."`
	Body           ImportContactsRequest
	RawBody        []byte
}

// ContactImportItemResult is the outcome of one submitted row. Every row gets
// exactly one result, indexed by its position in the request, so a caller can
// reconcile its source spreadsheet line-by-line.
type ContactImportItemResult struct {
	Index      int    `json:"index" doc:"Zero-based position of this row in the submitted contacts array."`
	Address    string `json:"address,omitempty" doc:"Canonicalized address. Absent when the row could not be parsed."`
	Status     string `json:"status" doc:"Outcome for this row. Known values: created, updated, skipped, failed."`
	Code       string `json:"code,omitempty" doc:"Machine-branchable reason, present for skipped and failed rows. Known values: duplicate_in_batch, already_exists, invalid_recipient, invalid_request, contact_limit_reached."`
	Message    string `json:"message,omitempty" doc:"Human-readable explanation for a skipped or failed row."`
	Suppressed bool   `json:"suppressed,omitempty" doc:"True when this address is on a suppression list. The contact is still recorded — suppression is surfaced here so the import count stays honest rather than silently smaller — but sends to it will be refused."`
	Enrolled   bool   `json:"enrolled,omitempty" doc:"True when this import created the optional per-agent outreach enrolment for this row. False means no agent was requested or the enrolment already existed."`
}

// ContactImportResult summarizes one upload.
type ContactImportResult struct {
	BatchID  string                    `json:"batch_id" doc:"Identifier for this import. Pass it to DELETE /v1/contacts/imports/{batch_id} to reverse the upload, or as a filter on the contacts list."`
	Created  int                       `json:"created"`
	Updated  int                       `json:"updated"`
	Skipped  int                       `json:"skipped"`
	Failed   int                       `json:"failed"`
	Results  []ContactImportItemResult `json:"results" nullable:"false" doc:"One entry per submitted row, in request order."`
	Reversed bool                      `json:"-"`
}

type importContactsOutput struct {
	Body ContactImportResult
}

type deleteImportBatchInput struct {
	BatchID string `path:"batch_id"`
	DeleteConfirm
}

// DeleteImportBatchResult is the reversal receipt.
type DeleteImportBatchResult struct {
	Deleted            bool   `json:"deleted"`
	BatchID            string `json:"batch_id"`
	ContactsDeleted    int    `json:"contacts_deleted" doc:"How many contacts this reversal removed."`
	ContactsRetained   int    `json:"contacts_retained" doc:"How many contacts from this batch were deliberately kept: edited since the import, enrolled in outreach that survives, or carrying correspondence history."`
	EngagementsDeleted int    `json:"engagements_deleted" doc:"How many per-agent outreach enrolments created by this import were removed. Enrolments edited or used since the import survive and are not counted here."`
}

type deleteImportBatchOutput struct {
	Body DeleteImportBatchResult
}

func (s *Server) registerContactImport() {
	huma.Register(s.API, huma.Operation{
		OperationID: "importContacts", Method: http.MethodPost, Path: "/v1/contacts/import",
		Summary: "Import contacts in bulk (beta)", Tags: []string{"contacts"},
		Description:  "Creates or updates up to 1000 contacts in one request and returns a per-row outcome, so one malformed row never rejects the rest of the upload. Import is inert: it records identity and sends nothing. Addresses already on a suppression list are still imported but flagged, so the reported count matches what was submitted. Account-scoped credentials only. " + contactImportBetaDescription,
		Security:     []map[string][]string{{"bearer": {}}},
		MaxBodyBytes: maxContactImportBodyBytes,
		Extensions:   beta(),
	}, s.handleImportContacts)

	huma.Register(s.API, huma.Operation{
		OperationID: "deleteImportBatch", Method: http.MethodDelete, Path: "/v1/contacts/imports/{batch_id}",
		Summary: "Reverse a contact import (beta)", Tags: []string{"contacts"},
		Description: "Reverses the durable import batch. Requires ?confirm=DELETE. It removes only what is verifiably untouched: contacts the batch created that have not been edited, enrolled in surviving outreach, or corresponded with since, and per-agent enrolments the batch created that carry no later edit, message, or recorded activity. Pre-existing outreach and suppressions are never affected, and a contact with any surviving engagement is always retained. The response reports each category; contacts_deleted + contacts_retained accounts for every batch-created contact that still exists. Account-scoped credentials only. " + contactImportBetaDescription,
		Security:    []map[string][]string{{"bearer": {}}},
		Extensions:  beta(),
	}, s.handleDeleteImportBatch)
}

func (s *Server) handleImportContacts(ctx context.Context, in *importContactsInput) (*importContactsOutput, error) {
	user, err := s.requireContactStore(ctx)
	if err != nil {
		return nil, err
	}
	if s.deps.ImportContacts == nil {
		return nil, NewError(http.StatusNotImplemented, "not_implemented", "contact import is not available on this deployment")
	}
	if in.Body.Stage != "" && in.Body.AgentEmail == "" {
		return nil, NewError(http.StatusBadRequest, "invalid_request", "stage requires agent_email")
	}
	var agentID string
	if in.Body.AgentEmail != "" {
		agentID, err = validateContactAddress(in.Body.AgentEmail)
		if err != nil {
			return nil, NewError(http.StatusBadRequest, "invalid_request", "agent_email must be a valid email address")
		}
	}
	if len(in.Body.Contacts) == 0 {
		// Defence in depth: the schema already rejects a missing, null, or
		// empty array with 422 invalid_request, but a successful zero-row
		// import reads as "it worked" while recording nothing — the worst
		// possible answer to a bulk upload — so the bound is enforced here
		// too, mirroring the row cap below.
		return nil, NewError(http.StatusBadRequest, "invalid_request", "contacts must contain at least one row")
	}
	if len(in.Body.Contacts) > maxContactImportRows {
		// Defence in depth: the schema already caps this, but the bound is what
		// keeps a synchronous import safe, so it is enforced here too.
		return nil, NewError(http.StatusRequestEntityTooLarge, "payload_too_large",
			"at most 1000 contacts may be imported per request")
	}

	// Validate every row up front. Rows that fail never reach the database, and
	// their failure is recorded against their own index so the caller can fix
	// exactly those lines and re-upload.
	rows := make([]identity.ContactImportRow, len(in.Body.Contacts))
	prevalidated := make([]*ContactImportItemResult, len(in.Body.Contacts))
	for i, row := range in.Body.Contacts {
		address, addrErr := validateContactAddress(row.Address)
		if addrErr != nil {
			prevalidated[i] = &ContactImportItemResult{
				Index: i, Status: identity.ImportStatusFailed,
				Code: "invalid_recipient", Message: "address must be a valid email address",
			}
			continue
		}
		if metaErr := validateContactMetadata(row.Metadata); metaErr != nil {
			prevalidated[i] = &ContactImportItemResult{
				Index: i, Address: address, Status: identity.ImportStatusFailed,
				Code: "invalid_request", Message: humanErrorMessage(metaErr),
			}
			continue
		}
		rows[i] = identity.ContactImportRow{
			Address: address, DisplayName: row.DisplayName, Metadata: row.Metadata,
		}
	}

	// Only rows that survived validation are handed to the store; their
	// original indexes are carried so outcomes can be merged back in order.
	var accepted []identity.ContactImportRow
	var acceptedIndex []int
	for i := range rows {
		if prevalidated[i] == nil {
			accepted = append(accepted, rows[i])
			acceptedIndex = append(acceptedIndex, i)
		}
	}

	merge := in.Body.OnConflict != "skip"

	// Keyed-idempotency guard. This matters more here than on any other contact
	// operation: an import that times out AFTER the rows landed is
	// indistinguishable from one that failed, and the natural reaction is to
	// upload the file again. Replaying the first response — including its
	// original batch_id, so the reversal still addresses the right rows —
	// is what makes that retry safe.
	_, result, err := runIdempotent(s, ctx, user.ID, in.IdempotencyKey,
		"/v1/contacts/import", in.RawBody,
		func() (int, ContactImportResult, error) {
			batchID := identity.NewImportBatchID()
			var outcomes []identity.ContactImportOutcome
			var ierr error
			if agentID == "" {
				outcomes, ierr = s.deps.ImportContacts(ctx, user.ID, batchID, accepted, merge)
			} else {
				if s.deps.ImportContactsWithOptions == nil {
					return 0, ContactImportResult{}, NewError(http.StatusNotImplemented, "not_implemented", "contact enrollment is not available on this deployment")
				}
				outcomes, ierr = s.deps.ImportContactsWithOptions(ctx, user.ID, batchID, accepted,
					identity.ContactImportOptions{Merge: merge, AgentID: agentID, Stage: in.Body.Stage})
			}
			if errors.Is(ierr, identity.ErrAgentNotFound) {
				return 0, ContactImportResult{}, NewError(http.StatusNotFound, "not_found", "agent not found")
			}
			if ierr != nil || len(outcomes) != len(accepted) {
				return 0, ContactImportResult{}, NewError(http.StatusInternalServerError, "internal_error", "failed to import contacts")
			}

			items := make([]ContactImportItemResult, len(in.Body.Contacts))
			for i, pre := range prevalidated {
				if pre != nil {
					items[i] = *pre
				}
			}
			for n, outcome := range outcomes {
				i := acceptedIndex[n]
				items[i] = ContactImportItemResult{
					Index: i, Address: outcome.Address, Status: outcome.Status,
					Code: outcome.Code, Message: outcome.Message, Enrolled: outcome.Enrolled,
				}
			}
			// Suppression is part of the response contract, so compute it before
			// completing the idempotency record. Replays must return these exact
			// flags even if consent state changes later.
			s.markSuppressedImportRows(ctx, user.ID, agentID, items)
			complete := ContactImportResult{BatchID: batchID, Results: items}
			for _, item := range items {
				switch item.Status {
				case identity.ImportStatusCreated:
					complete.Created++
				case identity.ImportStatusUpdated:
					complete.Updated++
				case identity.ImportStatusSkipped:
					complete.Skipped++
				default:
					complete.Failed++
				}
			}
			return http.StatusOK, complete, nil
		})
	if err != nil {
		return nil, err
	}
	logImportOutcome(user.ID, result)
	return &importContactsOutput{Body: result}, nil
}

// logImportOutcome writes one operational line per import.
//
// Bulk import is the one contact operation worth logging: it is a single
// user-visible action whose outcome is a mixture, and a support question about
// it ("I uploaded 500 and only 480 landed") is unanswerable without a trace.
// Per-request logging for the rest of the surface would be noise.
//
// Deliberately NO addresses. The whole payload is other people's email
// addresses, and this is exactly the "full PII payloads" the logging rules
// exclude. Failure CODES plus the batch id are enough to answer the support
// question, and the caller already has the per-row detail in the response.
func logImportOutcome(userID string, r ContactImportResult) {
	codes := map[string]int{}
	suppressed := 0
	for _, item := range r.Results {
		if item.Code != "" {
			codes[item.Code]++
		}
		if item.Suppressed {
			suppressed++
		}
	}
	log.Printf("[contacts] import batch=%s user=%s rows=%d created=%d updated=%d skipped=%d failed=%d suppressed=%d",
		r.BatchID, userID, len(r.Results), r.Created, r.Updated, r.Skipped, r.Failed, suppressed)
	if len(codes) > 0 {
		// Sorted so the line is stable and greppable across runs.
		keys := make([]string, 0, len(codes))
		for k := range codes {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s=%d", k, codes[k]))
		}
		log.Printf("[contacts] import batch=%s reasons %s", r.BatchID, strings.Join(parts, " "))
	}
}

// markSuppressedImportRows flags rows whose address the account already
// blocks. A lookup failure is deliberately non-fatal: the contacts are already
// written, and losing an advisory flag is not worth failing an otherwise
// successful import over. Sends remain blocked regardless — this flag is
// informational, never the enforcement point.
func (s *Server) markSuppressedImportRows(ctx context.Context, userID, agentID string, items []ContactImportItemResult) {
	if agentID == "" && s.deps.SuppressedAddresses == nil {
		return
	}
	if agentID != "" && s.deps.EffectiveSuppressions == nil {
		return
	}
	var addresses []string
	for _, item := range items {
		if item.Address != "" && item.Status != identity.ImportStatusFailed {
			addresses = append(addresses, item.Address)
		}
	}
	if len(addresses) == 0 {
		return
	}
	var blocked []string
	var err error
	if agentID == "" {
		blocked, err = s.deps.SuppressedAddresses(ctx, userID, addresses)
	} else {
		blocked, err = s.deps.EffectiveSuppressions(ctx, userID, agentID, addresses)
	}
	if err != nil || len(blocked) == 0 {
		return
	}
	blockedSet := make(map[string]struct{}, len(blocked))
	for _, a := range blocked {
		blockedSet[identity.NormalizeMailboxAddress(a)] = struct{}{}
	}
	for i := range items {
		if _, ok := blockedSet[items[i].Address]; ok {
			items[i].Suppressed = true
		}
	}
}

// humanErrorMessage extracts the message from a handler error envelope so a
// per-row failure can carry the same explanation a single-contact create would
// have returned.
func humanErrorMessage(err error) string {
	var se huma.StatusError
	if errors.As(err, &se) {
		return se.Error()
	}
	return "invalid row"
}

func (s *Server) handleDeleteImportBatch(ctx context.Context, in *deleteImportBatchInput) (*deleteImportBatchOutput, error) {
	user, err := s.requireContactStore(ctx)
	if err != nil {
		return nil, err
	}
	if s.deps.DeleteImportBatch == nil {
		return nil, NewError(http.StatusNotImplemented, "not_implemented", "contact import is not available on this deployment")
	}
	deleted, retained, engagementsDeleted, err := s.deps.DeleteImportBatch(ctx, user.ID, in.BatchID)
	if errors.Is(err, identity.ErrImportBatchNotFound) {
		// Absent, already reversed, and another account's batch are the same
		// answer, so this cannot be used to probe for other tenants' imports.
		return nil, NewError(http.StatusNotFound, "import_batch_not_found", "import batch not found")
	}
	if err != nil {
		return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to reverse import")
	}
	return &deleteImportBatchOutput{Body: DeleteImportBatchResult{
		Deleted: true, BatchID: in.BatchID,
		ContactsDeleted: deleted, ContactsRetained: retained,
		EngagementsDeleted: engagementsDeleted,
	}}, nil
}
