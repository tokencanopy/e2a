package httpapi

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/tokencanopy/e2a/internal/identity"
)

// AccountUserView is the authenticated principal's identity (A-1). Returned by
// GET /v1/account (MCP `whoami`) so an agent/operator can answer "who am I"
// without a follow-up call.
type AccountUserView struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

// AccountView is the authenticated account: identity + plan + limits + usage
// (A-2, formerly LimitsView). `whoami` maps here. It is scope-aware: `scope`
// is always set, and `agent_email` is populated ONLY for agent-scoped
// credentials (where the credential *is* a single agent) — omitted for
// account scope, which spans many agents.
type AccountView struct {
	User AccountUserView `json:"user"`
	// Scope is an OPEN set on this response view (evolving vocabulary), not a
	// closed enum — see docs/api.md "Versioning & stability".
	Scope        string          `json:"scope" doc:"Credential scope. Open set: new values may be added over time, so treat these as strings and tolerate unknown values. Known values: account, agent."`
	AgentAddress string          `json:"agent_email,omitempty"`
	PlanCode     string          `json:"plan_code"`
	Limits       LimitsCapsView  `json:"limits"`
	Usage        LimitsUsageView `json:"usage"`
	UpgradeURL   string          `json:"upgrade_url"`
}

type LimitsCapsView struct {
	MaxAgents        int   `json:"max_agents"`
	MaxDomains       int   `json:"max_domains"`
	MaxMessagesMonth int   `json:"max_messages_month"`
	MaxStorageBytes  int64 `json:"max_storage_bytes" doc:"Storage-quota cap in bytes, checked against usage.storage_bytes (see its doc for what is counted)."`
}

// LimitsUsageView is the current consumption snapshot.
//
// StorageBytes is the storage-quota accounting basis: per stored message it
// sums the RAW MIME byte length (raw_message — the message-level size_bytes)
// PLUS retained outbound draft body columns (body_text, body_html, and the
// draft attachments blob). These survive terminal transitions and can coexist
// with canonical sent MIME. A DB trigger on the messages table (migrations
// 016/039) maintains the total transactionally.
type LimitsUsageView struct {
	Agents        int   `json:"agents"`
	Domains       int   `json:"domains"`
	MessagesMonth int   `json:"messages_month"`
	StorageBytes  int64 `json:"storage_bytes" doc:"Bytes of retained message content counted against the storage quota: per message, RAW MIME plus any retained outbound body and attachment columns, including content retained after terminal transitions."`
}

type accountOutput struct{ Body AccountView }

const limitsUnavailableRetrySeconds = 5

func (s *Server) registerAccount() {
	registerOp(s.API, huma.Operation{
		OperationID: "getAccount", Method: http.MethodGet, Path: "/v1/account",
		Summary: "Get account: identity + plan limits + usage (whoami)", Tags: []string{"account"},
		Description: "The authenticated principal's identity (user + scope; agent_email for agent-scoped credentials), plan caps, and current usage. Works for both account- and agent-scoped credentials. (Deployment discovery — shared domain, slug registration — is the separate public GET /v1/info.)",
		Security:    []map[string][]string{{"bearer": {}}},
		Responses: map[string]*huma.Response{
			"503": s.jsonResponse(reflect.TypeOf(ErrorEnvelope{}), "ErrorEnvelope",
				"Service Unavailable — code limits_unavailable: the limits subsystem is temporarily unavailable. Wait Retry-After seconds, then retry."),
			"default": s.errorEnvelopeResponse(),
		},
	}, s.handleGetMyLimits)

	registerOp(s.API, huma.Operation{
		OperationID: "exportAccount", Method: http.MethodGet, Path: "/v1/account/export",
		Summary: "Export your data (GDPR right-of-access)", Tags: []string{"account"},
		Description: "A JSON dump of every record the authenticated account owns. " +
			"Contract: the export envelope (the top-level keys and schema_version) is stable; " +
			"the interior record shapes are versioned by schema_version and may evolve — " +
			"branch on schema_version before interpreting interior records. " +
			"Interior schemas carry `x-stability-level: beta` in this document to mark that " +
			"exemption machine-readably; the operation itself is stable GA surface.",
		Security: []map[string][]string{{"bearer": {}}},
	}, s.handleExportUserData)

	registerOp(s.API, huma.Operation{
		OperationID: "deleteAccount", Method: http.MethodDelete, Path: "/v1/account",
		Summary: "Delete your account + all data (irreversible)", Tags: []string{"account"},
		Description: "Permanently deletes the account and cascades all owned data. Requires ?confirm=DELETE. Returns 409 send_in_progress while an outbound provider call has a fresh lease; retry after it finishes. Returns 200 with a deletion receipt (deleted:true plus per-table cascade counts) — like every delete op, which all return 200 + a deletion object.",
		Security:    []map[string][]string{{"bearer": {}}},
		Responses: map[string]*huma.Response{
			"409": s.jsonResponse(reflect.TypeOf(ErrorEnvelope{}), "ErrorEnvelope",
				"Conflict — code send_in_progress: an outbound provider call has a fresh lease. Retry after it finishes."),
			"default": s.errorEnvelopeResponse(),
		},
	}, s.handleDeleteAccount)

	registerOp(s.API, huma.Operation{
		OperationID: "listSuppressions", Method: http.MethodGet, Path: "/v1/account/suppressions",
		Summary: "List suppressed recipient addresses", Tags: []string{"account"},
		Description: "Addresses e2a will refuse to send to (auto-added on a hard bounce or complaint, or added manually). Sends to a suppressed address fail with recipient_suppressed.",
		Security:    []map[string][]string{{"bearer": {}}},
	}, s.handleListSuppressions)

	registerOp(s.API, huma.Operation{
		OperationID: "deleteSuppression", Method: http.MethodDelete, Path: "/v1/account/suppressions/{address}",
		Summary: "Remove an address from the suppression list", Tags: []string{"account"},
		Description: "Un-suppress a recipient. A previously-blocked send to it then succeeds (idempotency keys are released, so no fresh key is needed). Requires ?confirm=DELETE. Returns 200 with a deletion object ({deleted:true, address}).",
		Security:    []map[string][]string{{"bearer": {}}},
	}, s.handleDeleteSuppression)
}

// SuppressionView is the wire shape of one (user, address) entry on the
// per-tenant suppression list — the HTTP DTO over the identity.Suppression
// storage model. Field-for-field identical JSON; mapped via suppressionView().
type SuppressionView struct {
	Address         string    `json:"address"`
	Reason          string    `json:"reason,omitempty"`
	Source          string    `json:"source"` // bounce | complaint | manual
	SourceMessageID string    `json:"source_message_id,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
}

// suppressionView maps the storage model to its wire view.
func suppressionView(s identity.Suppression) SuppressionView {
	return SuppressionView{
		Address:         s.Address,
		Reason:          s.Reason,
		Source:          s.Source,
		SourceMessageID: s.SourceMessageID,
		CreatedAt:       s.CreatedAt,
	}
}

// suppressionsOutput uses the shared Page[T] envelope (items + next_cursor);
// next_cursor is null at launch. Suppressions auto-grow on every bounce/
// complaint, so the pagination slot matters most here. See listAgentsOutput.
// (GA blocker #3.)
type suppressionsOutput struct {
	Body Page[SuppressionView]
}

// suppressionsCursor is the opaque keyset position: the last row's
// (created_at, address). Compact keys keep the cursor short.
type suppressionsCursor struct {
	CreatedAt time.Time `json:"c"`
	Address   string    `json:"a"`
}

type listSuppressionsInput struct {
	PageParams
}

func (s *Server) handleListSuppressions(ctx context.Context, in *listSuppressionsInput) (*suppressionsOutput, error) {
	user, err := s.requireAccountUser(ctx)
	if err != nil {
		return nil, err
	}
	if s.deps.ListSuppressions == nil {
		return nil, NewError(http.StatusNotImplemented, "not_implemented", "suppressions are not available on this deployment")
	}
	var afterCreatedAt time.Time
	var afterAddress string
	if in.Cursor != "" {
		var cur suppressionsCursor
		if err := s.decodeCursor(user.ID, cursorAccountSuppressions, in.Cursor, &cur); err != nil {
			return nil, err
		}
		afterCreatedAt = cur.CreatedAt
		afterAddress = cur.Address
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 100
	}
	// Fetch limit+1 to detect a further page.
	list, err := s.deps.ListSuppressions(ctx, user.ID, limit+1, afterCreatedAt, afterAddress)
	if err != nil {
		return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to list suppressions")
	}
	hasMore := len(list) > limit
	if hasMore {
		list = list[:limit]
	}
	var nextCursor string
	if hasMore {
		last := list[len(list)-1]
		nextCursor, err = EncodeCursor(s.deps.CursorSecret, user.ID, cursorAccountSuppressions, suppressionsCursor{CreatedAt: last.CreatedAt, Address: last.Address})
		if err != nil {
			return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to build pagination cursor")
		}
	}
	items := make([]SuppressionView, len(list))
	for i, sup := range list {
		items[i] = suppressionView(sup)
	}
	return &suppressionsOutput{Body: NewPage(items, nextCursor)}, nil
}

type deleteSuppressionInput struct {
	Address string `path:"address"`
	DeleteConfirm
}
type deleteSuppressionOutput struct{ Body DeleteSuppressionResult }

func (s *Server) handleDeleteSuppression(ctx context.Context, in *deleteSuppressionInput) (*deleteSuppressionOutput, error) {
	user, err := s.requireAccountUser(ctx)
	if err != nil {
		return nil, err
	}
	if s.deps.RemoveSuppression == nil {
		return nil, NewError(http.StatusNotImplemented, "not_implemented", "suppressions are not available on this deployment")
	}
	found, err := s.deps.RemoveSuppression(ctx, user.ID, in.Address)
	if err != nil {
		return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to remove suppression")
	}
	if !found {
		return nil, NewError(http.StatusNotFound, "not_found", "address not on the suppression list")
	}
	return &deleteSuppressionOutput{Body: DeleteSuppressionResult{Deleted: true, Address: in.Address}}, nil
}

type exportOutput struct {
	ContentDisposition string `header:"Content-Disposition"`
	Body               *identity.UserExport
}

func (s *Server) handleExportUserData(ctx context.Context, _ *struct{}) (*exportOutput, error) {
	user, err := s.requireAccountUser(ctx)
	if err != nil {
		return nil, err
	}
	if s.deps.ExportUserData == nil {
		return nil, NewError(http.StatusInternalServerError, "internal_error", "export unavailable")
	}
	dump, err := s.deps.ExportUserData(ctx, user.ID)
	if err != nil {
		return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to export user data")
	}
	// A-3: top-level export collections render as [] not null when empty.
	if dump != nil {
		dump.Domains = orEmpty(dump.Domains)
		dump.Agents = orEmpty(dump.Agents)
		dump.APIKeys = orEmpty(dump.APIKeys)
		dump.Messages = orEmpty(dump.Messages)
		dump.UsageEvents = orEmpty(dump.UsageEvents)
		dump.OAuthConnections = orEmpty(dump.OAuthConnections)
	}
	return &exportOutput{
		ContentDisposition: `attachment; filename="e2a-export-` + user.ID + `.json"`,
		Body:               dump,
	}, nil
}

type deleteAccountInput struct {
	DeleteConfirm
}

type deleteAccountOutput struct {
	Body *identity.DeleteUserDataResult
}

func (s *Server) handleDeleteAccount(ctx context.Context, in *deleteAccountInput) (*deleteAccountOutput, error) {
	user, err := s.requireAccountUser(ctx)
	if err != nil {
		return nil, err
	}
	// Confirm is enforced declaratively by Huma (required + enum:[DELETE] on
	// DeleteConfirm): a missing/wrong ?confirm is a 422 before this handler.
	if s.deps.DeleteUserData == nil {
		return nil, NewError(http.StatusInternalServerError, "internal_error", "delete unavailable")
	}
	res, err := s.deps.DeleteUserData(ctx, user)
	if err != nil {
		if errors.Is(err, identity.ErrSendInProgress) {
			return nil, NewError(http.StatusConflict, "send_in_progress",
				"an outbound provider call is still in progress; retry after it finishes")
		}
		return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to delete user data")
	}
	// Conform to the uniform delete-object shape: every delete op returns
	// {deleted:true, ...}; the account receipt keeps its per-table cascade
	// counts on top of that base.
	res.Deleted = true
	return &deleteAccountOutput{Body: res}, nil
}

func (s *Server) handleGetMyLimits(ctx context.Context, _ *struct{}) (*accountOutput, error) {
	// whoami works for BOTH scopes (A-1): an agent-scoped credential must be
	// able to learn its own identity, so this authenticates any principal
	// rather than requiring account scope.
	p, err := s.requirePrincipal(ctx)
	if err != nil {
		return nil, err
	}
	user := p.User
	if s.deps.GetLimits == nil {
		return nil, NewError(http.StatusServiceUnavailable, "limits_unavailable", "limits subsystem not configured").
			WithDetails(RetryAfterDetails{RetryAfterSeconds: limitsUnavailableRetrySeconds}).
			WithRetryAfter(limitsUnavailableRetrySeconds)
	}
	caps, err := s.deps.GetLimits(ctx, user.ID)
	if err != nil {
		return nil, NewError(http.StatusInternalServerError, "internal_error", "limits lookup failed")
	}
	var usage LimitsUsageView
	if s.deps.GetUsage != nil {
		usage = s.deps.GetUsage(ctx, user.ID)
	}
	// agent_email only for agent-scoped credentials (the credential IS one
	// agent; its id == its email by construction). Omitted for account scope.
	agentAddress := ""
	if p.Scope == identity.ScopeAgent {
		agentAddress = p.AgentID
	}
	return &accountOutput{Body: AccountView{
		User:         AccountUserView{ID: user.ID, Email: user.Email},
		Scope:        p.Scope,
		AgentAddress: agentAddress,
		PlanCode:     caps.PlanCode,
		Limits: LimitsCapsView{
			MaxAgents:        caps.MaxAgents,
			MaxDomains:       caps.MaxDomains,
			MaxMessagesMonth: caps.MaxMessagesMonth,
			MaxStorageBytes:  caps.MaxStorageBytes,
		},
		Usage:      usage,
		UpgradeURL: caps.UpgradeURL,
	}}, nil
}
