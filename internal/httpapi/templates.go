package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/tokencanopy/e2a/internal/emailtemplate"
	"github.com/tokencanopy/e2a/internal/identity"
	"github.com/tokencanopy/e2a/internal/startertemplates"
)

// User email templates sub-resource (beta).
//
// A template is a reusable subject + plain-text body (+ optional HTML part)
// whose {{variable}} placeholders (internal/emailtemplate) are rendered
// server-side at send time. CRUD lives here; the send integration lives in
// outbound.go (template_id / template_alias / template_data on the send body).

const templatesBetaDoc = "Beta: templates are unstable — their shape may change before they are declared stable."

const (
	templateMaxNameLen = 200
)

// templatePart ties one template part name to its escape mode. This table is
// THE single definition driving create/update parse validation
// (validateTemplateFields), send-time rendering (resolveSendTemplate) and
// the validate endpoint's preview (handleValidateTemplate) — the three
// surfaces can never disagree on which part HTML-escapes.
type templatePart struct {
	name     string
	escape   emailtemplate.EscapeMode
	optional bool // the part may be absent (html)
}

var templateParts = [...]templatePart{
	{name: "subject", escape: emailtemplate.EscapeNone},
	{name: "text", escape: emailtemplate.EscapeNone},
	{name: "html", escape: emailtemplate.EscapeHTML, optional: true},
}

// templateAliasRe is the alias charset: a letter, then up to 127 of
// [A-Za-z0-9._-]. Aliases are per-user unique handles for send-time lookup.
var templateAliasRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._-]{0,127}$`)

// TemplateData is a free-form JSON object carrying template variables
// (template_data on send, test_data on validate). It decodes with
// json.Decoder.UseNumber so numeric values arrive as json.Number and render
// digit-exact — plain encoding/json would decode every number as float64,
// corrupting integers beyond 2^53 (123456789012345678 → …680). The OpenAPI
// schema is unchanged: reflection sees an ordinary map → free-form object.
type TemplateData map[string]any

func (d *TemplateData) UnmarshalJSON(b []byte) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return err
	}
	*d = m
	return nil
}

// TemplateView is the full template resource (create/get/update responses;
// the list returns TemplateSummaryView).
type TemplateView struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Alias    string `json:"alias,omitempty" doc:"Optional per-user unique handle usable as template_alias on send."`
	Subject  string `json:"subject"`
	Body     string `json:"text" doc:"The plain-text part's template source."`
	HTMLBody string `json:"html,omitempty" doc:"The optional HTML part's template source."`
	// Read-only starter provenance, set only on from_starter creates.
	FromStarterAlias   string    `json:"from_starter_alias,omitempty" doc:"The starter template this was copied from (read-only, set by from_starter creates). Beta: templates are unstable — their shape may change before they are declared stable."`
	FromStarterVersion string    `json:"from_starter_version,omitempty" doc:"The starter catalog version at copy time (read-only, set by from_starter creates). Beta: templates are unstable — their shape may change before they are declared stable."`
	CreatedAt          time.Time `json:"created_at" format:"date-time"`
	UpdatedAt          time.Time `json:"updated_at" format:"date-time"`
}

func templateView(tp *identity.Template) TemplateView {
	return TemplateView{
		ID:                 tp.ID,
		Name:               tp.Name,
		Alias:              tp.Alias,
		Subject:            tp.Subject,
		Body:               tp.Body,
		HTMLBody:           tp.HTMLBody,
		FromStarterAlias:   tp.FromStarterAlias,
		FromStarterVersion: tp.FromStarterVersion,
		CreatedAt:          tp.CreatedAt.UTC(),
		UpdatedAt:          tp.UpdatedAt.UTC(),
	}
}

// TemplateSummaryView is the list-item shape: metadata only, NO body/
// html_body sources (a full library of maximal templates would ship
// megabytes per list call; every list consumer needs metadata). Fetch one by
// id for the sources — same split as the starter-templates list/detail.
type TemplateSummaryView struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Alias     string    `json:"alias,omitempty" doc:"Optional per-user unique handle usable as template_alias on send."`
	Subject   string    `json:"subject"`
	CreatedAt time.Time `json:"created_at" format:"date-time"`
	UpdatedAt time.Time `json:"updated_at" format:"date-time"`
}

func templateSummaryView(tp *identity.TemplateSummary) TemplateSummaryView {
	return TemplateSummaryView{
		ID:        tp.ID,
		Name:      tp.Name,
		Alias:     tp.Alias,
		Subject:   tp.Subject,
		CreatedAt: tp.CreatedAt.UTC(),
		UpdatedAt: tp.UpdatedAt.UTC(),
	}
}

type templateOutput struct{ Body TemplateView }
type listTemplatesOutput struct {
	Body Page[TemplateSummaryView]
}

// CreateTemplateRequest — either literal source (name, subject and text
// required; alias and html optional) or from_starter (a starter alias
// copied verbatim; name/alias default to the starter's). All template parts
// must parse. The required-ness of name/subject/text is enforced in the
// handler (validateTemplateFields), not the schema, because from_starter
// supplies them.
type CreateTemplateRequest struct {
	Name        string `json:"name,omitempty" doc:"Human-readable template name. Required unless from_starter supplies the default."`
	Alias       string `json:"alias,omitempty" doc:"Optional per-user unique handle ([A-Za-z][A-Za-z0-9._-]{0,127}) usable as template_alias on send."`
	Subject     string `json:"subject,omitempty" doc:"Subject template source ({{variable}} interpolation, no HTML escaping). Required unless from_starter is set."`
	Body        string `json:"text,omitempty" doc:"Plain-text body template source (no HTML escaping). Required unless from_starter is set."`
	HTMLBody    string `json:"html,omitempty" doc:"Optional HTML body template source ({{x}} is HTML-escaped, {{{x}}} is raw)."`
	FromStarter string `json:"from_starter,omitempty" doc:"Copy a starter template (by alias, see GET /v1/starter-templates) verbatim into your library. Mutually exclusive with subject, text and html — the copy is verbatim; edit the created template afterwards. name and alias default to the starter's and may be overridden. Beta: templates are unstable — their shape may change before they are declared stable."`
}
type createTemplateInput struct{ Body CreateTemplateRequest }

// UpdateTemplateRequest is the PATCH body — pointer fields so absent != zero.
// Setting alias or html to "" clears it. Changed parts are re-parsed.
type UpdateTemplateRequest struct {
	Name     *string `json:"name,omitempty"`
	Alias    *string `json:"alias,omitempty" doc:"Set to \"\" to clear the alias."`
	Subject  *string `json:"subject,omitempty"`
	Body     *string `json:"text,omitempty"`
	HTMLBody *string `json:"html,omitempty" doc:"Set to \"\" to remove the HTML part."`
}
type updateTemplateInput struct {
	ID   string `path:"id"`
	Body UpdateTemplateRequest
}

// TemplateIDParam is the path input for single-template read/update ops.
type TemplateIDParam struct {
	ID string `path:"id"`
}

// deleteTemplateInput is TemplateIDParam plus the uniform destructive-delete
// guard (DeleteConfirm) — a dedicated input so only DELETE requires ?confirm.
type deleteTemplateInput struct {
	ID string `path:"id"`
	DeleteConfirm
}

func (s *Server) registerTemplates() {
	registerOp(s.API, huma.Operation{
		OperationID: "createTemplate", Method: http.MethodPost, Path: "/v1/templates",
		Summary: "Create a template (beta)", Tags: []string{"templates"},
		Description: "Create a reusable email template. subject and text (and html when present) must parse: {{variable}} interpolation with dot paths; {{{variable}}} renders raw in the HTML part. Alternatively set from_starter to copy a starter template verbatim. " + templatesBetaDoc,
		Security:    []map[string][]string{{"bearer": {}}}, DefaultStatus: http.StatusCreated,
		Extensions: beta(),
	}, s.handleCreateTemplate)

	registerOp(s.API, huma.Operation{
		OperationID: "listTemplates", Method: http.MethodGet, Path: "/v1/templates",
		Summary: "List templates (beta)", Tags: []string{"templates"},
		Description: "List the account's templates, newest first. Returns metadata only (no text/html); fetch one by id for the full sources. " + templatesBetaDoc,
		Security:    []map[string][]string{{"bearer": {}}},
		Extensions:  beta(),
	}, s.handleListTemplates)

	registerOp(s.API, huma.Operation{
		OperationID: "getTemplate", Method: http.MethodGet, Path: "/v1/templates/{id}",
		Summary: "Get a template (beta)", Tags: []string{"templates"},
		Description: "Fetch one template by id. " + templatesBetaDoc,
		Security:    []map[string][]string{{"bearer": {}}},
		Extensions:  beta(),
	}, s.handleGetTemplate)

	registerOp(s.API, huma.Operation{
		OperationID: "updateTemplate", Method: http.MethodPatch, Path: "/v1/templates/{id}",
		Summary: "Update a template (beta)", Tags: []string{"templates"},
		Description: "Partial update. Changed template parts are re-parsed; set alias or html to \"\" to clear them. " + templatesBetaDoc,
		Security:    []map[string][]string{{"bearer": {}}},
		Extensions:  beta(),
	}, s.handleUpdateTemplate)

	registerOp(s.API, huma.Operation{
		OperationID: "deleteTemplate", Method: http.MethodDelete, Path: "/v1/templates/{id}",
		Summary: "Delete a template (beta)", Tags: []string{"templates"},
		Description: "Delete a template. In-flight sends are unaffected (rendering happens at send time). Requires ?confirm=DELETE. Returns 200 with a deletion object ({deleted:true, id}). " + templatesBetaDoc,
		Security:    []map[string][]string{{"bearer": {}}},
		Extensions:  beta(),
	}, s.handleDeleteTemplate)

	registerOp(s.API, huma.Operation{
		OperationID: "validateTemplate", Method: http.MethodPost, Path: "/v1/templates/validate",
		Summary: "Validate template source (beta)", Tags: []string{"templates"},
		Description: "Dry-run template source without persisting: reports per-part parse errors, a rendered preview (against test_data when provided), and suggested_data — a placeholder value for every variable the source references. " + templatesBetaDoc,
		Security:    []map[string][]string{{"bearer": {}}},
		Extensions:  beta(),
	}, s.handleValidateTemplate)
}

// templateParseError parses one template part, mapping a syntax error to the
// 400 invalid_template envelope with the part + parse message in details.
func templateParseError(part, src string) *ErrorEnvelope {
	if _, err := emailtemplate.Parse(src); err != nil {
		return NewError(http.StatusBadRequest, "invalid_template", "template part "+part+" failed to parse: "+err.Error()).
			WithDetails(map[string]any{"part": part, "message": err.Error()})
	}
	return nil
}

// validateTemplateFields runs the create-time rules over the effective field
// set (create passes the request; PATCH passes the post-patch state).
func validateTemplateFields(name, alias, subject, body, htmlBody string) *ErrorEnvelope {
	if name == "" {
		return NewError(http.StatusBadRequest, "invalid_request", "name is required")
	}
	if len(name) > templateMaxNameLen {
		return NewError(http.StatusBadRequest, "invalid_request", "name too long (max 200 chars)")
	}
	if alias != "" && !templateAliasRe.MatchString(alias) {
		return NewError(http.StatusBadRequest, "invalid_request", "alias must match [A-Za-z][A-Za-z0-9._-]{0,127}")
	}
	if subject == "" || body == "" {
		return NewError(http.StatusBadRequest, "invalid_request", "subject and text are required")
	}
	srcs := [...]string{subject, body, htmlBody}
	for i, p := range templateParts {
		if p.optional && srcs[i] == "" {
			continue
		}
		if env := templateParseError(p.name, srcs[i]); env != nil {
			return env
		}
	}
	return nil
}

func (s *Server) handleCreateTemplate(ctx context.Context, in *createTemplateInput) (*templateOutput, error) {
	user, err := s.requireAccountUser(ctx)
	if err != nil {
		return nil, err
	}
	if s.deps.CreateTemplate == nil {
		return nil, NewError(http.StatusInternalServerError, "internal_error", "templates unavailable")
	}
	b := in.Body
	create := identity.TemplateCreate{
		Name: b.Name, Alias: b.Alias, Subject: b.Subject, Body: b.Body, HTMLBody: b.HTMLBody,
	}
	if b.FromStarter != "" {
		// The starter is copied VERBATIM — literal source is rejected rather
		// than merged; the caller edits the created template afterwards.
		if b.Subject != "" || b.Body != "" || b.HTMLBody != "" {
			return nil, NewError(http.StatusBadRequest, "invalid_request",
				"from_starter is mutually exclusive with subject, text and html — the starter is copied verbatim; edit the template after creating it")
		}
		m, ok := startertemplates.Get(b.FromStarter)
		if !ok {
			return nil, NewError(http.StatusNotFound, "starter_template_not_found", "starter template not found")
		}
		create.Subject, create.Body, create.HTMLBody = m.Subject, m.TextBody, m.HTMLBody
		// Record provenance: which master, at which catalog version.
		create.FromStarterAlias, create.FromStarterVersion = m.Alias, m.Version
		if create.Name == "" {
			create.Name = m.Name
		}
		if create.Alias == "" {
			create.Alias = m.Alias
		}
	}
	if env := validateTemplateFields(create.Name, create.Alias, create.Subject, create.Body, create.HTMLBody); env != nil {
		return nil, env
	}
	tp, err := s.deps.CreateTemplate(ctx, user.ID, create)
	if err != nil {
		switch {
		case errors.Is(err, identity.ErrTemplateAliasTaken):
			return nil, NewError(http.StatusConflict, "alias_taken", "a template with this alias already exists")
		case errors.Is(err, identity.ErrTemplateLimitReached):
			return nil, NewError(http.StatusBadRequest, "template_limit_reached", err.Error())
		default:
			return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to create template")
		}
	}
	return &templateOutput{Body: templateView(tp)}, nil
}

// listTemplatesInput carries the standard cursor/limit (PageParams).
type listTemplatesInput struct {
	PageParams
}

func (s *Server) handleListTemplates(ctx context.Context, in *listTemplatesInput) (*listTemplatesOutput, error) {
	user, err := s.requireAccountUser(ctx)
	if err != nil {
		return nil, err
	}
	if s.deps.ListTemplates == nil {
		return nil, NewError(http.StatusInternalServerError, "internal_error", "templates unavailable")
	}
	afterCreatedAt, afterID, err := s.decodeKeyset(user.ID, cursorTemplates, in.Cursor)
	if err != nil {
		return nil, err
	}
	limit := effectiveLimit(in.Limit)
	// Fetch limit+1 to detect a further page.
	tps, err := s.deps.ListTemplates(ctx, user.ID, limit+1, afterCreatedAt, afterID)
	if err != nil {
		return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to list templates")
	}
	hasMore := len(tps) > limit
	if hasMore {
		tps = tps[:limit]
	}
	items := make([]TemplateSummaryView, 0, len(tps))
	for i := range tps {
		items = append(items, templateSummaryView(&tps[i]))
	}
	var nextCursor string
	if hasMore {
		last := tps[len(tps)-1]
		if nextCursor, err = s.encodeKeyset(user.ID, cursorTemplates, last.CreatedAt, last.ID); err != nil {
			return nil, err
		}
	}
	return &listTemplatesOutput{Body: NewPage(items, nextCursor)}, nil
}

func (s *Server) handleGetTemplate(ctx context.Context, in *TemplateIDParam) (*templateOutput, error) {
	user, err := s.requireAccountUser(ctx)
	if err != nil {
		return nil, err
	}
	if s.deps.GetTemplate == nil {
		return nil, NewError(http.StatusInternalServerError, "internal_error", "templates unavailable")
	}
	tp, err := s.deps.GetTemplate(ctx, in.ID, user.ID)
	if err != nil {
		// Only a genuine miss is a 404 — any other store error (timeout,
		// connection loss) is a 500, mirroring update/delete.
		if errors.Is(err, identity.ErrTemplateNotFound) {
			return nil, NewError(http.StatusNotFound, "not_found", "template not found")
		}
		return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to load template")
	}
	if tp == nil {
		return nil, NewError(http.StatusNotFound, "not_found", "template not found")
	}
	return &templateOutput{Body: templateView(tp)}, nil
}

func (s *Server) handleUpdateTemplate(ctx context.Context, in *updateTemplateInput) (*templateOutput, error) {
	user, err := s.requireAccountUser(ctx)
	if err != nil {
		return nil, err
	}
	if s.deps.GetTemplate == nil || s.deps.UpdateTemplate == nil {
		return nil, NewError(http.StatusInternalServerError, "internal_error", "templates unavailable")
	}
	current, err := s.deps.GetTemplate(ctx, in.ID, user.ID)
	if err != nil {
		if errors.Is(err, identity.ErrTemplateNotFound) {
			return nil, NewError(http.StatusNotFound, "not_found", "template not found")
		}
		return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to load template")
	}
	if current == nil {
		return nil, NewError(http.StatusNotFound, "not_found", "template not found")
	}
	// Validate the effective post-patch state against the create-time rules
	// (mirrors handleUpdateWebhook). Re-parses anything changed; unchanged
	// parts re-parse too, which is harmless — they parsed at write time.
	eff := func(cur string, p *string) string {
		if p != nil {
			return *p
		}
		return cur
	}
	b := in.Body
	if env := validateTemplateFields(
		eff(current.Name, b.Name), eff(current.Alias, b.Alias),
		eff(current.Subject, b.Subject), eff(current.Body, b.Body),
		eff(current.HTMLBody, b.HTMLBody),
	); env != nil {
		return nil, env
	}
	tp, err := s.deps.UpdateTemplate(ctx, in.ID, user.ID, identity.TemplateUpdate{
		Name: b.Name, Alias: b.Alias, Subject: b.Subject, Body: b.Body, HTMLBody: b.HTMLBody,
	})
	if err != nil {
		switch {
		case errors.Is(err, identity.ErrTemplateNotFound):
			return nil, NewError(http.StatusNotFound, "not_found", "template not found")
		case errors.Is(err, identity.ErrTemplateAliasTaken):
			return nil, NewError(http.StatusConflict, "alias_taken", "a template with this alias already exists")
		default:
			return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to update template")
		}
	}
	return &templateOutput{Body: templateView(tp)}, nil
}

type deleteTemplateOutput struct{ Body DeleteTemplateResult }

func (s *Server) handleDeleteTemplate(ctx context.Context, in *deleteTemplateInput) (*deleteTemplateOutput, error) {
	user, err := s.requireAccountUser(ctx)
	if err != nil {
		return nil, err
	}
	if s.deps.DeleteTemplate == nil {
		return nil, NewError(http.StatusInternalServerError, "internal_error", "templates unavailable")
	}
	if err := s.deps.DeleteTemplate(ctx, in.ID, user.ID); err != nil {
		if errors.Is(err, identity.ErrTemplateNotFound) {
			return nil, NewError(http.StatusNotFound, "not_found", "template not found")
		}
		return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to delete template")
	}
	return &deleteTemplateOutput{Body: DeleteTemplateResult{Deleted: true, ID: in.ID}}, nil
}

// --- send integration (POST /v1/agents/{email}/messages) ---

// validateSendTemplateShape runs the DETERMINISTIC template-reference checks
// on the send body — they depend only on the request bytes, so they run in
// the handler prologue, before the idempotency claim (a keyed retry hits the
// same 400 either way; no mutable state is consulted).
//
// Rules (each → 400 invalid_request):
//   - template_id and template_alias are mutually exclusive;
//   - a template reference is mutually exclusive with literal
//     subject/text/html;
//   - template_data requires a template reference.
func validateSendTemplateShape(b *SendEmailRequest) *ErrorEnvelope {
	if b.TemplateID == "" && b.TemplateAlias == "" {
		if len(b.TemplateData) > 0 {
			return NewError(http.StatusBadRequest, "invalid_request", "template_data requires template_id or template_alias")
		}
		return nil
	}
	if b.TemplateID != "" && b.TemplateAlias != "" {
		return NewError(http.StatusBadRequest, "invalid_request", "template_id and template_alias are mutually exclusive")
	}
	if b.Subject != "" || b.Body != "" || b.HTMLBody != "" {
		return NewError(http.StatusBadRequest, "invalid_request", "a template reference is mutually exclusive with subject, text and html")
	}
	return nil
}

// resolveSendTemplate resolves and renders a template reference on the send
// body IN PLACE. Two ordering invariants:
//
//   - It runs INSIDE the idempotent execution (after the Claim/replay
//     handshake in deliver): template rows are mutable state, so a keyed
//     retry must replay the cached response even if the template was deleted
//     or edited between attempts — resolving before the claim would 404 (or
//     silently re-render different content) instead of replaying.
//   - Rendering still precedes the HITL hold — HoldForApprovalCore (inside
//     DeliverOutbound) persists literal subject/body and approve re-sends the
//     stored draft without re-rendering, so a reviewer must see (and approve)
//     the final rendered content, never raw template source.
//
// Lookup is scoped to the caller (missing/cross-user → 404
// template_not_found); parse/render failures → 400 template_render_failed
// with the part in details. On success b.Subject/b.Body/b.HTMLBody carry the
// rendered content and flow through the same validateOutboundBody checks as
// a literal send. validateSendTemplateShape must have passed already.
func (s *Server) resolveSendTemplate(ctx context.Context, userID string, b *SendEmailRequest) *ErrorEnvelope {
	if b.TemplateID == "" && b.TemplateAlias == "" {
		return nil
	}
	if s.deps.GetTemplate == nil || s.deps.GetTemplateByAlias == nil {
		return NewError(http.StatusInternalServerError, "internal_error", "templates unavailable")
	}

	var tp *identity.Template
	var err error
	if b.TemplateID != "" {
		tp, err = s.deps.GetTemplate(ctx, b.TemplateID, userID)
	} else {
		tp, err = s.deps.GetTemplateByAlias(ctx, b.TemplateAlias, userID)
	}
	if err != nil {
		// Only a genuine miss is a 404 — any other store error (timeout,
		// connection loss) must surface as a 500, not masquerade as a
		// deleted template.
		if errors.Is(err, identity.ErrTemplateNotFound) {
			return NewError(http.StatusNotFound, "template_not_found", "template not found")
		}
		return NewError(http.StatusInternalServerError, "internal_error", "failed to load template")
	}
	if tp == nil {
		return NewError(http.StatusNotFound, "template_not_found", "template not found")
	}

	// partVars remembers each part's referenced variables so the
	// rendered-empty diagnostic below can name them.
	partVars := map[string][]string{}
	render := func(part, src string, mode emailtemplate.EscapeMode) (string, *ErrorEnvelope) {
		tmpl, perr := emailtemplate.Parse(src)
		if perr != nil {
			return "", NewError(http.StatusBadRequest, "template_render_failed",
				"template part "+part+" failed to render: "+perr.Error()).
				WithDetails(map[string]any{"part": part, "message": perr.Error()})
		}
		partVars[part] = tmpl.Vars()
		out, rerr := tmpl.Render(b.TemplateData, mode)
		if rerr != nil {
			return "", NewError(http.StatusBadRequest, "template_render_failed",
				"template part "+part+" failed to render: "+rerr.Error()).
				WithDetails(map[string]any{"part": part, "message": rerr.Error()})
		}
		return out, nil
	}

	srcs := [...]string{tp.Subject, tp.Body, tp.HTMLBody}
	outs := [...]*string{&b.Subject, &b.Body, &b.HTMLBody}
	for i, p := range templateParts {
		if p.optional && srcs[i] == "" {
			continue
		}
		out, env := render(p.name, srcs[i], p.escape)
		if env != nil {
			return env
		}
		*outs[i] = out
	}
	// A subject or text part that rendered EMPTY is a guaranteed dead-end: the
	// generic outbound validation would reject it with a bare
	// "subject and text are required", which reads as nonsense to a caller
	// who did reference a template. Fail here with the empty part and the
	// variables it references (almost always missing template_data).
	for _, check := range []struct{ part, out string }{{"subject", b.Subject}, {"text", b.Body}} {
		if check.out != "" {
			continue
		}
		vars := partVars[check.part]
		if vars == nil {
			vars = []string{}
		}
		return NewError(http.StatusBadRequest, "template_rendered_empty",
			"template part "+check.part+" rendered empty — supply values for its variables ("+
				strings.Join(vars, ", ")+") in template_data").
			WithDetails(map[string]any{"part": check.part, "variables": vars})
	}
	return nil
}

// --- validate endpoint ---

// ValidateTemplateRequest carries template source to dry-run. Parts may be
// empty (an empty part parses trivially and renders empty).
type ValidateTemplateRequest struct {
	Subject  string       `json:"subject,omitempty"`
	Body     string       `json:"text,omitempty"`
	HTMLBody string       `json:"html,omitempty"`
	TestData TemplateData `json:"test_data,omitempty" doc:"Sample template_data to render the preview with. Missing variables render as empty strings."`
}
type validateTemplateInput struct{ Body ValidateTemplateRequest }

// TemplatePartError is one per-part validation failure.
type TemplatePartError struct {
	Part    string `json:"part" doc:"Which part failed. Known values: subject, text, html."`
	Message string `json:"message"`
}

// RenderedTemplateView is the rendered preview (present only when valid).
type RenderedTemplateView struct {
	Subject  string `json:"subject"`
	Body     string `json:"text"`
	HTMLBody string `json:"html,omitempty"`
}

// ValidateTemplateResponse is the dry-run report.
type ValidateTemplateResponse struct {
	Valid         bool                  `json:"valid"`
	Errors        []TemplatePartError   `json:"errors" nullable:"false"`
	Rendered      *RenderedTemplateView `json:"rendered,omitempty" doc:"Rendered preview against test_data (or empty data). Present only when valid."`
	SuggestedData map[string]any        `json:"suggested_data,omitempty" doc:"A placeholder value for every variable the source references — a starting point for template_data. Dot-path variables ({{user.name}}) emit NESTED objects, matching how the renderer resolves them."`
}
type validateTemplateOutput struct{ Body ValidateTemplateResponse }

func (s *Server) handleValidateTemplate(ctx context.Context, in *validateTemplateInput) (*validateTemplateOutput, error) {
	if _, err := s.requireAccountUser(ctx); err != nil {
		return nil, err
	}
	b := in.Body
	data := b.TestData
	if data == nil {
		data = map[string]any{}
	}

	resp := ValidateTemplateResponse{Errors: []TemplatePartError{}}
	rendered := &RenderedTemplateView{}
	suggested := map[string]any{}

	// Every part is dry-run, empty or not (an empty part parses trivially
	// and renders empty) — escape modes come from the shared templateParts
	// table, so the preview matches the send path exactly.
	srcs := [...]string{b.Subject, b.Body, b.HTMLBody}
	outs := [...]*string{&rendered.Subject, &rendered.Body, &rendered.HTMLBody}
	for i, p := range templateParts {
		tmpl, err := emailtemplate.Parse(srcs[i])
		if err != nil {
			resp.Errors = append(resp.Errors, TemplatePartError{Part: p.name, Message: err.Error()})
			continue
		}
		for _, v := range tmpl.Vars() {
			suggestPlaceholder(suggested, v)
		}
		out, err := tmpl.Render(data, p.escape)
		if err != nil {
			resp.Errors = append(resp.Errors, TemplatePartError{Part: p.name, Message: err.Error()})
			continue
		}
		*outs[i] = out
	}

	resp.Valid = len(resp.Errors) == 0
	if resp.Valid {
		resp.Rendered = rendered
	}
	if len(suggested) > 0 {
		resp.SuggestedData = suggested
	}
	return &validateTemplateOutput{Body: resp}, nil
}

// suggestPlaceholder inserts "<ident>_value" for one variable into the
// suggested_data map, building NESTED objects for dot paths — {{user.name}}
// yields {"user": {"name": "user.name_value"}} — because the renderer
// resolves dots as nested paths only, so a flat "user.name" key would render
// empty if pasted back as template_data. First writer wins on conflicts
// (e.g. {{user}} vs {{user.name}}): an existing scalar or object at any
// segment is left untouched.
func suggestPlaceholder(suggested map[string]any, ident string) {
	segs := strings.Split(ident, ".")
	cur := suggested
	for _, seg := range segs[:len(segs)-1] {
		next, ok := cur[seg].(map[string]any)
		if !ok {
			if _, exists := cur[seg]; exists {
				return // a scalar already claims this segment
			}
			next = map[string]any{}
			cur[seg] = next
		}
		cur = next
	}
	leaf := segs[len(segs)-1]
	if _, exists := cur[leaf]; !exists {
		cur[leaf] = ident + "_value"
	}
}
