package httpapi

import (
	"context"
	"log"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/tokencanopy/e2a/internal/identity"
)

// Read-only accounts (docs/design/account-read-only.md).
//
// An account whose sending is paused with pause class `abuse` is read-only:
// no write of any kind succeeds on /v1, whatever the credential (API key of
// either scope, dashboard session, OAuth access token, agent access token,
// delegated token — they all resolve to a principal owned by the account).
// Reads keep working, and so does moving the account to the trash (the
// permanent erase stays 409 erase_held). Other pause classes refuse only
// sending.
//
// This middleware is the ONLY enforcement point on /v1. Handlers do not check
// read-only themselves; every operation is classified in operationAccess
// below, and TestReadOnlySpecWalk walks the committed OpenAPI document so an
// operation added without a classification fails CI.

// opAccess classifies what an operation does to account state.
type opAccess int

const (
	// accessRead changes no account state. Never refused for a read-only
	// account, and never pays the read-only lookup.
	accessRead opAccess = iota + 1
	// accessWrite changes account state. Refused with 403 account_read_only
	// for a read-only account.
	accessWrite
	// accessWriteAllowedReadOnly changes state but stays available to a
	// read-only account by product decision (the account trash).
	accessWriteAllowedReadOnly
	// accessPublic is unauthenticated; there is no account to consult.
	accessPublic
)

// operationAccess classifies every /v1 operation. The rule is the HTTP
// method — GET/HEAD read, everything else writes — with exactly three kinds of
// exception, each listed with its reason:
//
//   - validateTemplate is a POST that only validates a draft and stores
//     nothing (read);
//   - deleteAccount is the one write a read-only account keeps: moving the
//     account to the trash (the handler still refuses permanent=true with 409
//     erase_held while any pause applies);
//   - getInfo is public.
//
// Keep this table exhaustive: TestReadOnlySpecWalk fails for an operation in
// the spec that is missing here, and for any entry that disagrees with the
// method rule and its exceptions.
var operationAccess = map[string]opAccess{
	// account
	"getAccount":                 accessRead,
	"exportAccount":              accessRead,
	"deleteAccount":              accessWriteAllowedReadOnly,
	"getAccountMetrics":          accessRead,
	"listApiKeys":                accessRead,
	"createApiKey":               accessWrite,
	"deleteApiKey":               accessWrite,
	"getSendingAccessRequest":    accessRead,
	"createSendingAccessRequest": accessWrite,
	"listSuppressions":           accessRead,
	"deleteSuppression":          accessWrite,

	// agents
	"listAgents":         accessRead,
	"createAgent":        accessWrite,
	"getAgent":           accessRead,
	"updateAgent":        accessWrite,
	"deleteAgent":        accessWrite,
	"restoreAgent":       accessWrite,
	"testAgent":          accessWrite,
	"getAgentMetrics":    accessRead,
	"getAgentProtection": accessRead,
	"putAgentProtection": accessWrite,

	// agent suppressions
	"listAgentSuppressions":  accessRead,
	"createAgentSuppression": accessWrite,
	"deleteAgentSuppression": accessWrite,

	// engagements (outreach)
	"listEngagements":  accessRead,
	"getEngagement":    accessRead,
	"upsertEngagement": accessWrite,
	"deleteEngagement": accessWrite,

	// conversations
	"listConversations": accessRead,
	"getConversation":   accessRead,

	// messages
	"listMessages":        accessRead,
	"getMessage":          accessRead,
	"getMessageLifecycle": accessRead,
	"getAttachment":       accessRead,
	"sendMessage":         accessWrite,
	"replyToMessage":      accessWrite,
	"forwardMessage":      accessWrite,
	"updateMessage":       accessWrite,
	"deleteMessage":       accessWrite,
	"restoreMessage":      accessWrite,

	// reviews (HITL, both directions)
	"listReviews":   accessRead,
	"getReview":     accessRead,
	"approveReview": accessWrite,
	"rejectReview":  accessWrite,

	// contacts
	"listContacts":      accessRead,
	"getContact":        accessRead,
	"createContact":     accessWrite,
	"updateContact":     accessWrite,
	"deleteContact":     accessWrite,
	"importContacts":    accessWrite,
	"deleteImportBatch": accessWrite,

	// domains
	"listDomains":    accessRead,
	"getDomain":      accessRead,
	"registerDomain": accessWrite,
	"verifyDomain":   accessWrite,
	"deleteDomain":   accessWrite,

	// events
	"listEvents":     accessRead,
	"getEvent":       accessRead,
	"redeliverEvent": accessWrite,

	// templates
	"listTemplates":        accessRead,
	"getTemplate":          accessRead,
	"createTemplate":       accessWrite,
	"updateTemplate":       accessWrite,
	"deleteTemplate":       accessWrite,
	"validateTemplate":     accessRead,
	"listStarterTemplates": accessRead,
	"getStarterTemplate":   accessRead,

	// webhooks
	"listWebhooks":          accessRead,
	"getWebhook":            accessRead,
	"listWebhookDeliveries": accessRead,
	"createWebhook":         accessWrite,
	"updateWebhook":         accessWrite,
	"deleteWebhook":         accessWrite,
	"rotateWebhookSecret":   accessWrite,
	"testWebhook":           accessWrite,

	// deployment discovery
	"getInfo": accessPublic,
}

// classifyOperation returns the operation's access class. An operation
// missing from operationAccess (which TestReadOnlySpecWalk forbids) falls
// back to the method rule, so an unclassified write is refused rather than
// let through.
func classifyOperation(op *huma.Operation) opAccess {
	if a, ok := operationAccess[op.OperationID]; ok {
		return a
	}
	switch op.Method {
	case http.MethodGet, http.MethodHead:
		return accessRead
	default:
		return accessWrite
	}
}

// accountReadOnlyError is the canonical 403 account_read_only envelope.
func (s *Server) accountReadOnlyError() *ErrorEnvelope {
	return NewError(http.StatusForbidden, "account_read_only", identity.AccountReadOnlyMessage(s.deps.SupportContact))
}

// readOnlyGuard is the Huma middleware that refuses writes for a read-only
// account. Reads and public operations pass without any lookup. For a write
// it resolves the principal (reusing the per-request auth memo) and consults
// the account's control row on every request — no cache, so a pause or a
// resume applies to the next request. An unauthenticated or
// auth-unavailable request falls through so the handler emits its canonical
// 401/503. A failed read-only lookup fails closed: 503, and the write does
// not run.
func (s *Server) readOnlyGuard(ctx huma.Context, next func(huma.Context)) {
	op := ctx.Operation()
	if op == nil || s.deps.AccountReadOnly == nil {
		next(ctx)
		return
	}
	switch classifyOperation(op) {
	case accessWrite:
	default:
		next(ctx)
		return
	}
	p := principalFromContext(ctx.Context())
	if p == nil {
		r := RequestFromContext(ctx.Context())
		if r == nil {
			next(ctx)
			return
		}
		resolved, err := s.resolvePrincipal(r)
		if err != nil {
			next(ctx)
			return
		}
		p = resolved
		ctx = huma.WithContext(ctx, withPrincipal(ctx.Context(), p))
	}
	if p == nil || p.User == nil {
		// A resolver that returns neither an error nor an account is a
		// contract violation; refuse rather than guess who is writing.
		log.Printf("[httpapi] read-only check for %s: principal without an account", op.OperationID)
		writeEnvelope(ctx, NewError(http.StatusServiceUnavailable, "auth_unavailable",
			"account state is temporarily unavailable; retry"))
		return
	}
	readOnly, err := s.deps.AccountReadOnly(ctx.Context(), p.User.ID)
	if err != nil {
		log.Printf("[httpapi] read-only check for %s failed: %v", op.OperationID, err)
		writeEnvelope(ctx, NewError(http.StatusServiceUnavailable, "auth_unavailable",
			"account state is temporarily unavailable; retry"))
		return
	}
	if readOnly {
		writeEnvelope(ctx, s.accountReadOnlyError())
		return
	}
	next(ctx)
}

// accountReadOnlyView reports the read-only flag for GET /v1/account, or nil
// when the deployment does not wire the check or it could not be read (the
// field is then omitted rather than guessed).
func (s *Server) accountReadOnlyView(ctx context.Context, userID string) *bool {
	if s.deps.AccountReadOnly == nil {
		return nil
	}
	ro, err := s.deps.AccountReadOnly(ctx, userID)
	if err != nil {
		return nil
	}
	return &ro
}
