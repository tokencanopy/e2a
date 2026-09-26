package httpapi

import (
	"context"
	"errors"
	"log"
	"net/http"
	"reflect"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/tokencanopy/e2a/internal/sendingpolicy"
)

// External sending access — the customer-facing half. Everything here is
// read-only eligibility plus a request intake; nothing on this surface can
// grant access. Approval is a local operator command on the server.

const sendingAccessBetaDoc = "Beta: external sending access is a platform control that ships disabled; this surface may evolve."

// SendingAccessView is the additive `sending_access` object on GET
// /v1/account. Booleans only: it describes account eligibility, not a promise
// that a given send passes pause, quota, content or domain checks, and it
// deliberately carries no mailbox address.
type SendingAccessView struct {
	EnforcementApplies          bool `json:"enforcement_applies" doc:"True when the deployment enforces external sending access for this account (enforce mode, account inside the rollout cohort, not a platform account). Stays true after approval. False when the control is disabled or in shadow mode, or the account is outside the cohort."`
	SharedExternalApproved      bool `json:"shared_external_approved" doc:"True when an operator granted this account external sending through the shared sending identity. Reports the grant only, not whether enforcement is on."`
	PaidExternalSendingEntitled bool `json:"paid_external_sending_entitled" doc:"True when an active paid base subscription grants external sending (hosted service). Independent of shared_external_approved."`
	OwnerRecipientVerified      bool `json:"owner_recipient_verified" doc:"True when the account's current sign-in email was verified by a trusted login, so it is an allowed destination while external sending is restricted."`
}

func sendingAccessView(st sendingpolicy.ExternalAccessStatus) *SendingAccessView {
	return &SendingAccessView{
		EnforcementApplies:          st.EnforcementApplies,
		SharedExternalApproved:      st.SharedExternalApproved,
		PaidExternalSendingEntitled: st.PaidExternalSendingEntitled,
		OwnerRecipientVerified:      st.OwnerRecipientVerified,
	}
}

// accountSendingAccess resolves the optional object for GET /v1/account. A
// read failure omits the object rather than failing whoami or inventing
// false booleans; the gate still enforces the real state on every send.
func (s *Server) accountSendingAccess(ctx context.Context, userID string) *SendingAccessView {
	if s.deps.SendingAccessStatus == nil {
		return nil
	}
	st, err := s.deps.SendingAccessStatus(ctx, userID)
	if err != nil {
		log.Printf("[httpapi] sending access status unavailable: user=%s err=%v", userID, err)
		return nil
	}
	return sendingAccessView(st)
}

// SendingAccessRequestView is one approval request as its owner sees it. The
// state is customer-visible history, never a customer-writable grant.
type SendingAccessRequestView struct {
	ID                  string     `json:"id"`
	State               string     `json:"state" doc:"Open set: treat as a string and tolerate unknown values. Known values: pending (awaiting support review), approved, declined (contact support to appeal)."`
	UseCase             string     `json:"use_case"`
	Recipients          string     `json:"recipients"`
	ExpectedDailyVolume int        `json:"expected_daily_volume"`
	CreatedAt           time.Time  `json:"created_at"`
	DecidedAt           *time.Time `json:"decided_at,omitempty"`
}

func sendingAccessRequestView(r sendingpolicy.AccessRequest) SendingAccessRequestView {
	return SendingAccessRequestView{
		ID: r.ID, State: r.State, UseCase: r.UseCase, Recipients: r.Recipients,
		ExpectedDailyVolume: r.ExpectedDailyVolume, CreatedAt: r.CreatedAt, DecidedAt: r.DecidedAt,
	}
}

// SendingAccessRequestInput is the request form body. The account is never
// part of it: it is bound server-side from the authenticated principal.
type SendingAccessRequestInput struct {
	UseCase             string `json:"use_case" minLength:"1" maxLength:"2000" doc:"What you are building and why it needs to email external recipients."`
	Recipients          string `json:"recipients" minLength:"1" maxLength:"1000" doc:"Who you will email (for example: your own customers who signed up, your team)."`
	ExpectedDailyVolume int    `json:"expected_daily_volume" minimum:"1" maximum:"1000000" doc:"Expected recipients per day."`
}

type getSendingAccessRequestOutput struct {
	Body SendingAccessRequestView
}

type createSendingAccessRequestInput struct {
	Body SendingAccessRequestInput
}

type createSendingAccessRequestOutput struct {
	Status int
	Body   SendingAccessRequestView
}

// sendingAccessRequestRetryAfter is the advisory back-off for the per-account
// request cap (a rolling 30-day window).
const sendingAccessRequestRetryAfter = 24 * 60 * 60

func (s *Server) registerSendingAccess() {
	registerOp(s.API, huma.Operation{
		OperationID: "getSendingAccessRequest", Method: http.MethodGet, Path: "/v1/account/sending-access/request",
		Summary: "Get your latest external sending access request (beta)", Tags: []string{"account"},
		Description: "The account's most recent request for external sending access, with its review state. " +
			"404 not_found when the account has never filed one. Account-scoped credentials only. " + sendingAccessBetaDoc,
		Security:   []map[string][]string{{"bearer": {}}},
		Extensions: beta(),
	}, s.handleGetSendingAccessRequest)

	registerOp(s.API, huma.Operation{
		OperationID: "createSendingAccessRequest", Method: http.MethodPost, Path: "/v1/account/sending-access/request",
		Summary: "Request external sending access (beta)", Tags: []string{"account"},
		Description: "Files a request for support to review this account's external sending access. " +
			"Idempotent while a request is pending: submitting again returns the existing pending request (200) instead of creating another (201). " +
			"After a decline a new request may be filed as an appeal, up to 3 requests per 30 days (429 rate_limited beyond that). " +
			"Filing a request never grants access by itself. Account-scoped credentials only. " + sendingAccessBetaDoc,
		Security:      []map[string][]string{{"bearer": {}}},
		DefaultStatus: http.StatusCreated,
		// Two success statuses (201 created, 200 existing pending request),
		// which Huma cannot infer from DefaultStatus alone. Undeclared, a
		// spec-generated client has no case for 200 and hands the caller
		// nothing on a resubmit. Re-adds `default`, which a custom Responses
		// map otherwise suppresses.
		Responses: map[string]*huma.Response{
			"200": s.jsonResponse(reflect.TypeOf(SendingAccessRequestView{}), "SendingAccessRequestView",
				"OK — a request is already pending; it is returned unchanged and no new request was created."),
			"default": s.errorEnvelopeResponse(),
		},
		Extensions: beta(),
	}, s.handleCreateSendingAccessRequest)
}

func (s *Server) handleGetSendingAccessRequest(ctx context.Context, _ *struct{}) (*getSendingAccessRequestOutput, error) {
	user, err := s.requireAccountUser(ctx)
	if err != nil {
		return nil, err
	}
	if s.deps.LatestSendingAccessRequest == nil {
		return nil, NewError(http.StatusNotImplemented, "not_implemented", "sending access requests are not available on this deployment")
	}
	req, err := s.deps.LatestSendingAccessRequest(ctx, user.ID)
	if err != nil {
		return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to read sending access request")
	}
	if req == nil {
		return nil, NewError(http.StatusNotFound, "not_found", "no sending access request has been filed")
	}
	return &getSendingAccessRequestOutput{Body: sendingAccessRequestView(*req)}, nil
}

func (s *Server) handleCreateSendingAccessRequest(ctx context.Context, in *createSendingAccessRequestInput) (*createSendingAccessRequestOutput, error) {
	user, err := s.requireAccountUser(ctx)
	if err != nil {
		return nil, err
	}
	if s.deps.SubmitSendingAccessRequest == nil {
		return nil, NewError(http.StatusNotImplemented, "not_implemented", "sending access requests are not available on this deployment")
	}
	req, created, err := s.deps.SubmitSendingAccessRequest(ctx, user.ID, sendingpolicy.AccessRequestInput{
		UseCase:             in.Body.UseCase,
		Recipients:          in.Body.Recipients,
		ExpectedDailyVolume: in.Body.ExpectedDailyVolume,
	})
	switch {
	case errors.Is(err, sendingpolicy.ErrInvalidAccessRequest):
		return nil, NewError(http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, sendingpolicy.ErrAccessRequestRateLimited):
		return nil, NewError(http.StatusTooManyRequests, "rate_limited", "too many sending access requests; contact support to appeal a decision").
			WithDetails(RateLimitedDetails{RetryAfterSeconds: sendingAccessRequestRetryAfter}).
			WithRetryAfter(sendingAccessRequestRetryAfter)
	case err != nil:
		return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to file sending access request")
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
		if s.deps.NotifySendingAccessRequest != nil {
			// Best effort, bounded, after commit: a failed operator email must
			// not lose the request, which is durably queued either way.
			notifyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
			s.deps.NotifySendingAccessRequest(notifyCtx, user.ID, req)
			cancel()
		}
	}
	return &createSendingAccessRequestOutput{Status: status, Body: sendingAccessRequestView(req)}, nil
}
