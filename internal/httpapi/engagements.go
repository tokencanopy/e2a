package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/tokencanopy/e2a/internal/identity"
)

// Per-agent outreach state. This is the surface the outreach loop runs against:
// "who has not replied, is due, and is still mailable?" answered in one request.
//
// AUTHORIZATION NOTE — this is the one contact surface an AGENT-scoped
// credential may reach, and the divergence is deliberate. /v1/contacts is
// account-only because account-wide identity would expose every person a
// sibling agent corresponds with. Engagements are different: they are the
// runtime state the agent itself authors, and an agent that cannot read its own
// due list cannot do its job. It is safe because consent lives in
// agent_suppressions, a table this surface only ever reads through a join — an
// agent can move its own stage and can never un-suppress itself.
const engagementBetaDescription = "Beta: the outreach surface may change before it is declared stable."

// ContactEngagementView is one agent's relationship with one contact.
type ContactEngagementView struct {
	AgentEmail   string         `json:"agent_email"`
	Address      string         `json:"address"`
	Stage        string         `json:"stage" doc:"Caller-defined outreach stage. Opaque to e2a — there is no server-side state machine, and any string is valid."`
	NextActionAt *time.Time     `json:"next_action_at" doc:"When the caller wants to act next. e2a does not act on it; it drives the outreach query and the contact.due wake-up."`
	Metadata     map[string]any `json:"metadata" doc:"Caller-owned key/value data scoped to this relationship. Opaque to e2a."`

	Replied            bool       `json:"replied" doc:"Whether this contact has answered since outreach began — computed as last_inbound_at > first_outbound_at, so it means 'replied to us' rather than 'has ever written'. Server-owned."`
	Suppressed         bool       `json:"suppressed" doc:"Whether sends to this address are blocked, by an account-wide or agent-scoped suppression. Server-owned; reflects the same state the send path enforces."`
	SuppressionSource  string     `json:"suppression_source,omitempty" doc:"Why the block exists, when suppressed. Open set; tolerate unknown values. Known values: unsubscribe, manual, bounce, complaint."`
	SuppressionReason  string     `json:"suppression_reason,omitempty" doc:"Free-text detail recorded with the block, when present."`
	FirstOutboundAt    *time.Time `json:"first_outbound_at" doc:"Server-owned."`
	LastOutboundAt     *time.Time `json:"last_outbound_at" doc:"Server-owned. Include it in the due query (last_outbound_before) so a lost client state-write cannot cause a duplicate send."`
	LastInboundAt      *time.Time `json:"last_inbound_at" doc:"Server-owned."`
	OutboundCount      int        `json:"outbound_count" doc:"Successfully submitted outbound messages since enrollment. Queue failures and pre-enrollment history are excluded. Server-owned."`
	InboundCount       int        `json:"inbound_count" doc:"DMARC-authenticated inbound messages delivered since enrollment. Spoofed, held, blocked, and pre-enrollment messages are excluded. Server-owned."`
	LastConversationID string     `json:"last_conversation_id,omitempty" doc:"Server-owned."`

	// Embedded rather than referenced: an agent-scoped caller is barred from
	// /v1/contacts, so without this it could not compose a message at all.
	Contact   EmbeddedContactView `json:"contact"`
	CreatedAt time.Time           `json:"created_at"`
	UpdatedAt time.Time           `json:"updated_at"`
}

// EmbeddedContactView is the contact identity carried inline on an engagement.
type EmbeddedContactView struct {
	Address     string         `json:"address"`
	DisplayName string         `json:"display_name"`
	Metadata    map[string]any `json:"metadata"`
}

func engagementView(e identity.ContactEngagement) ContactEngagementView {
	meta := e.Metadata
	if meta == nil {
		meta = map[string]any{}
	}
	cmeta := e.ContactMetadata
	if cmeta == nil {
		cmeta = map[string]any{}
	}
	return ContactEngagementView{
		AgentEmail: e.AgentEmail, Address: e.Address,
		Stage: e.Stage, NextActionAt: e.NextActionAt, Metadata: meta,
		Replied: e.Replied(), Suppressed: e.Suppressed,
		SuppressionSource: e.SuppressionSource, SuppressionReason: e.SuppressionReason,
		FirstOutboundAt: e.FirstOutboundAt, LastOutboundAt: e.LastOutboundAt,
		LastInboundAt: e.LastInboundAt,
		OutboundCount: e.OutboundCount, InboundCount: e.InboundCount,
		LastConversationID: e.LastConversationID,
		Contact: EmbeddedContactView{
			Address: e.Address, DisplayName: e.DisplayName, Metadata: cmeta,
		},
		CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt,
	}
}

// engagementETag validates the fields an agent may write plus updated_at.
// Message-activity hooks also move updated_at, so an automation loop cannot
// unknowingly overwrite outreach state based on an older activity snapshot.
func engagementETag(e identity.ContactEngagement) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00%d", e.AgentEmail, e.Address, e.Stage, e.UpdatedAt.UnixNano())
	if e.NextActionAt != nil {
		fmt.Fprintf(h, "\x00%d", e.NextActionAt.UnixNano())
	}
	encoded, _ := json.Marshal(e.Metadata)
	h.Write(encoded)
	return `"` + hex.EncodeToString(h.Sum(nil)[:16]) + `"`
}

type engagementsCursor struct {
	CreatedAt  time.Time `json:"c"`
	ID         string    `json:"i"`
	AgentEmail string    `json:"g"`
	Filters    string    `json:"f"`
}

type listEngagementsInput struct {
	Email string `path:"email"`
	Stage string `query:"stage" doc:"Only engagements at this exact stage."`
	// Tri-state as an enum string rather than a bool: Huma has no optional-bool
	// query type, and omitting the param has to mean "both" rather than false.
	Replied            string    `query:"replied" enum:"true,false" doc:"Filter on whether the contact has answered since outreach began. Omit for both."`
	Suppressed         string    `query:"suppressed" enum:"true,false" doc:"Filter on whether sends are blocked. Omit for both."`
	NextActionBefore   time.Time `query:"next_action_before" doc:"Only engagements whose next_action_at has passed this instant (RFC 3339). Pass the current time to get everyone due."`
	LastOutboundBefore time.Time `query:"last_outbound_before" doc:"Only engagements not contacted since this instant (RFC 3339). Include it alongside next_action_before: last_outbound_at is server-maintained, so it excludes anyone just contacted even if the client's own state write was lost — without it, a failed write can cause a duplicate send."`
	PageParams
}

type listEngagementsOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         Page[ContactEngagementView]
}

// UpsertEngagementRequest enrolls a contact or updates the agent-owned fields.
// Only the three agent-owned fields are present; derived state is absent by
// design, so a client cannot fake a reply or a send. The struct is strict, so
// attempting one is a 422 rather than a silent no-op.
type UpsertEngagementRequest struct {
	Stage        *string        `json:"stage,omitempty" maxLength:"128" doc:"Set the outreach stage. Omit to leave unchanged."`
	NextActionAt *time.Time     `json:"next_action_at,omitempty" nullable:"true" doc:"Set when to act next (RFC 3339). Omit to leave unchanged; send null to clear the schedule."`
	Metadata     map[string]any `json:"metadata,omitempty" doc:"Replace the relationship metadata wholesale. Omit to leave unchanged; send an empty object to clear it."`
}

type upsertEngagementInput struct {
	Email   string `path:"email"`
	Address string `path:"address"`
	IfMatch string `header:"If-Match" doc:"Optional ETag from a prior read. When present the engagement must already exist and still match at the instant of the write, or the update is rejected with 412."`
	Body    UpsertEngagementRequest
	RawBody []byte
}

type upsertEngagementOutput struct {
	Status   int
	Location string `header:"Location"`
	ETag     string `header:"ETag"`
	Body     ContactEngagementView
}

type getEngagementInput struct {
	Email   string `path:"email"`
	Address string `path:"address"`
}

type getEngagementOutput struct {
	CacheControl string `header:"Cache-Control"`
	ETag         string `header:"ETag"`
	Body         ContactEngagementView
}

type deleteEngagementInput struct {
	Email   string `path:"email"`
	Address string `path:"address"`
	DeleteConfirm
}

// DeleteEngagementResult is the un-enrolment receipt.
type DeleteEngagementResult struct {
	Deleted bool   `json:"deleted"`
	Address string `json:"address"`
}

type deleteEngagementOutput struct {
	Body DeleteEngagementResult
}

func (s *Server) registerEngagements() {
	createdEngagement := s.jsonResponse(reflect.TypeOf(ContactEngagementView{}), "ContactEngagementView",
		"Created — the contact was newly enrolled in this agent's outreach.")
	// The 201 is hand-declared (Huma cannot infer two success statuses from a
	// single DefaultStatus), so its headers are declared here too. They point
	// at the SHARED header components so this response promises the same
	// validator semantics and the same path-encoding convention the
	// auto-derived 200 does — see contract_headers.go, which enriches every
	// declared ETag/Location with one wording.
	createdEngagement.Headers = map[string]*huma.Param{
		"ETag":     headerRef(headerETag),
		"Location": headerRef(headerResourceLocation),
	}
	huma.Register(s.API, huma.Operation{
		OperationID: "listEngagements", Method: http.MethodGet, Path: "/v1/agents/{email}/contacts",
		Summary: "List an agent's outreach (beta)", Tags: []string{"contacts"},
		Description: "Lists the contacts this agent is working, with the reply and delivery facts e2a derives from real message activity. Combine replied=false, next_action_before and last_outbound_before to get everyone due for a follow-up in one request. Agent-scoped credentials may read their own agent. " + engagementBetaDescription,
		Security:    []map[string][]string{{"bearer": {}}},
		Extensions:  beta(),
	}, s.handleListEngagements)

	huma.Register(s.API, huma.Operation{
		OperationID: "getEngagement", Method: http.MethodGet, Path: "/v1/agents/{email}/contacts/{address}",
		Summary: "Get one outreach record (beta)", Tags: []string{"contacts"},
		Description: "Fetches this agent's relationship with one contact. Returns an ETag for use with If-Match on a subsequent update. Agent-scoped credentials may read their own agent. " + engagementBetaDescription,
		Security:    []map[string][]string{{"bearer": {}}},
		Extensions:  beta(),
	}, s.handleGetEngagement)

	huma.Register(s.API, huma.Operation{
		OperationID: "upsertEngagement", Method: http.MethodPut, Path: "/v1/agents/{email}/contacts/{address}",
		Summary: "Enrol or update outreach state (beta)", Tags: []string{"contacts"},
		Description: "Enrols a contact in this agent's outreach, or updates the agent-owned fields of an existing enrolment. Omitted fields are left unchanged, so advancing the stage after a send does not disturb the schedule. Creates the contact if it does not exist. Pass If-Match from a prior read to prevent a stale automation loop from overwriting newer state; a conditional request never creates. Derived fields are server-owned and rejected. Returns 201 on first enrolment and 200 on a subsequent update. Agent-scoped credentials may write their own agent. " + engagementBetaDescription,
		Security:    []map[string][]string{{"bearer": {}}},
		// The handler emits 201 on create and 200 on update, which Huma cannot
		// infer from a single DefaultStatus. Undeclared, a spec-generated client
		// has no case for 201 and hands the caller nothing back on the FIRST
		// enrolment — the most common call. Re-adds `default`, which a custom
		// Responses map otherwise suppresses.
		Responses: map[string]*huma.Response{
			"201":     createdEngagement,
			"default": s.errorEnvelopeResponse(),
		},
		Extensions: beta(),
	}, s.handleUpsertEngagement)

	huma.Register(s.API, huma.Operation{
		OperationID: "deleteEngagement", Method: http.MethodDelete, Path: "/v1/agents/{email}/contacts/{address}",
		Summary: "Un-enrol a contact (beta)", Tags: []string{"contacts"},
		// The agent-scope sentence is not decoration: all four outreach
		// operations route through resolveOutreachAgent → requireAgentAccess,
		// so an agent-scoped credential may un-enrol on its own agent exactly
		// as it may read and write. Omitting the sentence here (the only one of
		// the four that did) read as if delete were account-only.
		Description: "Removes this agent's outreach state for a contact. Requires ?confirm=DELETE. The contact itself survives (identity is account-level and other agents may still be working them) and suppressions are untouched — un-enrolling is not consent and never restores sendability. Agent-scoped credentials may write their own agent. " + engagementBetaDescription,
		Security:    []map[string][]string{{"bearer": {}}},
		Extensions:  beta(),
	}, s.handleDeleteEngagement)
}

// resolveOutreachAgent enforces the agent-access ceiling and resolves the
// agent, conflating missing and foreign agents into one 404 so this surface
// cannot enumerate another account's agents.
func (s *Server) resolveOutreachAgent(ctx context.Context, email string) (*identity.Principal, *identity.AgentIdentity, error) {
	agentID := identity.NormalizeEmail(email)
	p, err := s.requireAgentAccess(ctx, agentID)
	if err != nil {
		return nil, nil, err
	}
	if s.deps.GetAgent == nil || s.deps.ListEngagements == nil {
		return nil, nil, NewError(http.StatusNotImplemented, "not_implemented", "contacts are not available on this deployment")
	}
	ag, err := s.deps.GetAgent(ctx, agentID)
	if err != nil || ag == nil || ag.UserID != p.User.ID {
		return nil, nil, NewError(http.StatusNotFound, "not_found", "agent not found")
	}
	return p, ag, nil
}

func (s *Server) handleListEngagements(ctx context.Context, in *listEngagementsInput) (*listEngagementsOutput, error) {
	p, ag, err := s.resolveOutreachAgent(ctx, in.Email)
	if err != nil {
		return nil, err
	}
	filter := identity.EngagementFilter{
		Stage:              in.Stage,
		Replied:            triState(in.Replied),
		Suppressed:         triState(in.Suppressed),
		NextActionBefore:   in.NextActionBefore,
		LastOutboundBefore: in.LastOutboundBefore,
	}
	fingerprint := engagementFilterFingerprint(filter)

	var afterCreatedAt time.Time
	var afterID string
	if in.Cursor != "" {
		var cur engagementsCursor
		if err := DecodeCursor([]string{s.deps.CursorSecret}, in.Cursor, &cur); err != nil ||
			cur.AgentEmail != ag.ID || cur.Filters != fingerprint {
			return nil, NewError(http.StatusBadRequest, "invalid_cursor", "invalid pagination cursor")
		}
		afterCreatedAt, afterID = cur.CreatedAt, cur.ID
	}

	limit := effectiveLimit(in.Limit)
	rows, err := s.deps.ListEngagements(ctx, p.User.ID, ag.ID, filter, limit+1, afterCreatedAt, afterID)
	if err != nil {
		return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to list outreach")
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	var next string
	if hasMore {
		last := rows[len(rows)-1]
		next, err = EncodeCursor(s.deps.CursorSecret, engagementsCursor{
			CreatedAt: last.CreatedAt, ID: last.ID, AgentEmail: ag.ID, Filters: fingerprint,
		})
		if err != nil {
			return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to build pagination cursor")
		}
	}
	items := make([]ContactEngagementView, len(rows))
	for i := range rows {
		items[i] = engagementView(rows[i])
	}
	return &listEngagementsOutput{CacheControl: "no-store", Body: NewPage(items, next)}, nil
}

func engagementFilterFingerprint(f identity.EngagementFilter) string {
	parts := f.Stage + "\x00"
	if f.Replied != nil {
		parts += boolKey(*f.Replied)
	}
	parts += "\x00"
	if f.Suppressed != nil {
		parts += boolKey(*f.Suppressed)
	}
	return parts + "\x00" + f.NextActionBefore.UTC().Format(time.RFC3339Nano) +
		"\x00" + f.LastOutboundBefore.UTC().Format(time.RFC3339Nano)
}

// triState maps an optional enum query param to a filter pointer: absent means
// "do not filter", which is distinct from filtering on false.
func triState(v string) *bool {
	switch v {
	case "true":
		t := true
		return &t
	case "false":
		f := false
		return &f
	}
	return nil
}

// bodyHasKey reports whether a top-level key was PRESENT in the request body,
// which JSON decoding alone cannot tell you: an omitted next_action_at and an
// explicit null both decode to a nil pointer, but they mean different things —
// "leave the schedule alone" versus "clear it". Huma hands us the raw bytes, so
// presence is checked there.
func bodyHasKey(raw []byte, key string) bool {
	if len(raw) == 0 {
		return false
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return false
	}
	_, present := probe[key]
	return present
}

func boolKey(b bool) string {
	if b {
		return "t"
	}
	return "f"
}

func (s *Server) handleGetEngagement(ctx context.Context, in *getEngagementInput) (*getEngagementOutput, error) {
	p, ag, err := s.resolveOutreachAgent(ctx, in.Email)
	if err != nil {
		return nil, err
	}
	e, err := s.loadEngagement(ctx, p.User.ID, ag.ID, in.Address)
	if err != nil {
		return nil, err
	}
	return &getEngagementOutput{CacheControl: "no-store", ETag: engagementETag(e), Body: engagementView(e)}, nil
}

func (s *Server) loadEngagement(ctx context.Context, userID, agentID, rawAddress string) (identity.ContactEngagement, error) {
	if s.deps.GetEngagement == nil {
		return identity.ContactEngagement{}, NewError(http.StatusNotImplemented, "not_implemented", "contacts are not available on this deployment")
	}
	address, addrErr := validateContactAddress(rawAddress)
	if addrErr != nil {
		// An unparseable address cannot name an existing engagement.
		return identity.ContactEngagement{}, NewError(http.StatusNotFound, "engagement_not_found", "engagement not found")
	}
	e, err := s.deps.GetEngagement(ctx, userID, agentID, address)
	if errors.Is(err, identity.ErrEngagementNotFound) {
		return identity.ContactEngagement{}, NewError(http.StatusNotFound, "engagement_not_found", "engagement not found")
	}
	if err != nil {
		return identity.ContactEngagement{}, NewError(http.StatusInternalServerError, "internal_error", "failed to load outreach record")
	}
	return e, nil
}

func (s *Server) handleUpsertEngagement(ctx context.Context, in *upsertEngagementInput) (*upsertEngagementOutput, error) {
	p, ag, err := s.resolveOutreachAgent(ctx, in.Email)
	if err != nil {
		return nil, err
	}
	if s.deps.UpsertEngagement == nil {
		return nil, NewError(http.StatusNotImplemented, "not_implemented", "contacts are not available on this deployment")
	}
	address, err := validateContactAddress(in.Address)
	if err != nil {
		return nil, err
	}
	if err := validateContactMetadata(in.Body.Metadata); err != nil {
		return nil, err
	}

	// next_action_at is tri-state on the wire: omitted (leave alone), a value
	// (set it), or explicit null (clear the schedule). The double pointer is how
	// "omitted" is distinguished from "set to null" below the handler.
	var next **time.Time
	if bodyHasKey(in.RawBody, "next_action_at") {
		v := in.Body.NextActionAt
		next = &v
	}

	var e identity.ContactEngagement
	var created bool
	if in.IfMatch != "" {
		current, loadErr := s.loadEngagement(ctx, p.User.ID, ag.ID, address)
		if loadErr != nil {
			var apiErr *ErrorEnvelope
			if errors.As(loadErr, &apiErr) && apiErr.status == http.StatusNotFound {
				return nil, NewError(http.StatusPreconditionFailed, "precondition_failed",
					"the outreach record does not exist; omit If-Match to enrol it")
			}
			return nil, loadErr
		}
		if !etagMatches(in.IfMatch, engagementETag(current)) {
			return nil, NewError(http.StatusPreconditionFailed, "precondition_failed",
				"the outreach record was modified by another request; re-read it and retry")
		}
		if s.deps.UpdateEngagementIfUnchanged == nil {
			return nil, NewError(http.StatusNotImplemented, "not_implemented",
				"conditional outreach updates are not available on this deployment")
		}
		e, err = s.deps.UpdateEngagementIfUnchanged(ctx, p.User.ID, ag.ID, address,
			in.Body.Stage, next, in.Body.Metadata, current.UpdatedAt)
	} else {
		e, created, err = s.deps.UpsertEngagement(ctx, p.User.ID, ag.ID, address,
			in.Body.Stage, next, in.Body.Metadata)
	}
	if errors.Is(err, identity.ErrContactLimitReached) {
		return nil, NewError(http.StatusBadRequest, "contact_limit_reached",
			"the account is at its contact limit")
	}
	if errors.Is(err, identity.ErrEngagementNotFound) ||
		errors.Is(err, identity.ErrEngagementPreconditionFailed) {
		return nil, NewError(http.StatusPreconditionFailed, "precondition_failed",
			"the outreach record was modified by another request; re-read it and retry")
	}
	if err != nil {
		return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to save outreach state")
	}
	out := &upsertEngagementOutput{Status: http.StatusOK, ETag: engagementETag(e), Body: engagementView(e)}
	if created {
		out.Status = http.StatusCreated
		out.Location = "/v1/agents/" + url.PathEscape(ag.ID) + "/contacts/" + url.PathEscape(e.Address)
	}
	return out, nil
}

func (s *Server) handleDeleteEngagement(ctx context.Context, in *deleteEngagementInput) (*deleteEngagementOutput, error) {
	p, ag, err := s.resolveOutreachAgent(ctx, in.Email)
	if err != nil {
		return nil, err
	}
	if s.deps.DeleteEngagement == nil {
		return nil, NewError(http.StatusNotImplemented, "not_implemented", "contacts are not available on this deployment")
	}
	address, addrErr := validateContactAddress(in.Address)
	if addrErr != nil {
		return nil, NewError(http.StatusNotFound, "engagement_not_found", "engagement not found")
	}
	removed, err := s.deps.DeleteEngagement(ctx, p.User.ID, ag.ID, address)
	if err != nil {
		return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to un-enrol contact")
	}
	if !removed {
		return nil, NewError(http.StatusNotFound, "engagement_not_found", "engagement not found")
	}
	// Deliberately touches nothing else: the contact is account-level and may be
	// worked by other agents, and consent lives in the suppression tables.
	return &deleteEngagementOutput{Body: DeleteEngagementResult{Deleted: true, Address: address}}, nil
}
