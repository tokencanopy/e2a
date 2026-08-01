package httpapi

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/tokencanopy/e2a/internal/startertemplates"
)

// starterTemplatesCursor is the opaque keyset position over the alias-sorted
// starter catalog: the last returned alias. The catalog is a small embedded set
// with no created_at, so alias (its unique, stable sort key) is the whole key.
type starterTemplatesCursor struct {
	Alias string `json:"a"`
}

// listStarterTemplatesInput carries the standard cursor/limit (PageParams).
type listStarterTemplatesInput struct {
	PageParams
}

// Starter templates sub-resource (beta, read-only).
//
// Starter templates are the pre-built masters shipped in
// internal/startertemplates: professionally designed subject + HTML + text
// sources a caller copies into their own template library (see from_starter
// on POST /v1/templates). The catalog is embedded in the binary — there is
// no per-user state, so the surface is two GETs and nothing else.

// StarterTemplateVariableView describes one interpolation slot a starter
// template declares.
type StarterTemplateVariableView struct {
	Name        string `json:"name"`
	Required    bool   `json:"required" doc:"Required variables should always be supplied; optional ones render as empty strings when absent."`
	Raw         bool   `json:"raw" doc:"Raw ({{{...}}}) slots accept pre-rendered HTML fragments inserted without escaping. Never feed untrusted input to raw slots."`
	Description string `json:"description"`
	Example     string `json:"example" doc:"A realistic sample value, usable as template_data for previews."`
}

// StarterTemplateView is one starter template in the list — catalog metadata
// only. The (large) body sources are returned by the get-by-alias endpoint.
type StarterTemplateView struct {
	Alias       string                        `json:"alias"`
	Name        string                        `json:"name"`
	Description string                        `json:"description"`
	Version     string                        `json:"version"`
	Subject     string                        `json:"subject" doc:"Subject template source ({{variable}} interpolation)."`
	Variables   []StarterTemplateVariableView `json:"variables" nullable:"false"`
}

// StarterTemplateDetailView is the single-starter shape: the list fields
// (flattened via embedding) plus the full body sources.
type StarterTemplateDetailView struct {
	StarterTemplateView
	Body     string `json:"text" doc:"The plain-text part's template source."`
	HTMLBody string `json:"html" doc:"The HTML part's template source."`
}

func starterTemplateView(m startertemplates.Master) StarterTemplateView {
	vars := make([]StarterTemplateVariableView, 0, len(m.Variables))
	for _, v := range m.Variables {
		vars = append(vars, StarterTemplateVariableView{
			Name: v.Name, Required: v.Required, Raw: v.Raw,
			Description: v.Description, Example: v.Example,
		})
	}
	return StarterTemplateView{
		Alias:       m.Alias,
		Name:        m.Name,
		Description: m.Description,
		Version:     m.Version,
		Subject:     m.Subject,
		Variables:   vars,
	}
}

type listStarterTemplatesOutput struct {
	Body Page[StarterTemplateView]
}
type starterTemplateOutput struct{ Body StarterTemplateDetailView }

// StarterTemplateAliasParam is the path input for single-starter ops.
type StarterTemplateAliasParam struct {
	Alias string `path:"alias" doc:"The starter template's alias, e.g. welcome."`
}

func (s *Server) registerStarterTemplates() {
	huma.Register(s.API, huma.Operation{
		OperationID: "listStarterTemplates", Method: http.MethodGet, Path: "/v1/starter-templates",
		Summary: "List starter templates (beta)", Tags: []string{"templates"},
		Description: "List the pre-built starter templates shipped with the deployment, sorted by alias. Returns catalog metadata only; fetch one by alias for the full body sources, or copy one into your library with from_starter on POST /v1/templates. " + templatesBetaDoc,
		Security:    []map[string][]string{{"bearer": {}}},
		Extensions:  beta(),
	}, s.handleListStarterTemplates)

	huma.Register(s.API, huma.Operation{
		OperationID: "getStarterTemplate", Method: http.MethodGet, Path: "/v1/starter-templates/{alias}",
		Summary: "Get a starter template (beta)", Tags: []string{"templates"},
		Description: "Fetch one starter template by alias, including its full plain-text and HTML body sources. " + templatesBetaDoc,
		Security:    []map[string][]string{{"bearer": {}}},
		Extensions:  beta(),
	}, s.handleGetStarterTemplate)
}

func (s *Server) handleListStarterTemplates(ctx context.Context, in *listStarterTemplatesInput) (*listStarterTemplatesOutput, error) {
	// The catalog is global, but the cursor is still bound to the calling
	// account like every other collection — the handler is auth-gated, so a
	// principal is always in hand, and a blanket rule stays auditable where a
	// per-collection carve-out list would be how holes reappear.
	user, err := s.requireAccountUser(ctx)
	if err != nil {
		return nil, err
	}
	cat := startertemplates.Catalog() // already alias-sorted (ascending)
	// Keyset over the alias sort key: skip everything up to and including the
	// cursor's alias, then return the next `limit`.
	var afterAlias string
	if in.Cursor != "" {
		var cur starterTemplatesCursor
		if err := s.decodeCursor(user.ID, cursorStarterTemplates, in.Cursor, &cur); err != nil {
			return nil, err
		}
		afterAlias = cur.Alias
	}
	limit := effectiveLimit(in.Limit)
	items := make([]StarterTemplateView, 0, limit)
	var lastAlias string
	hasMore := false
	for _, m := range cat {
		if afterAlias != "" && m.Alias <= afterAlias {
			continue
		}
		if len(items) == limit {
			hasMore = true
			break
		}
		items = append(items, starterTemplateView(m))
		lastAlias = m.Alias
	}
	var nextCursor string
	if hasMore {
		var err error
		if nextCursor, err = EncodeCursor(s.deps.CursorSecret, user.ID, cursorStarterTemplates, starterTemplatesCursor{Alias: lastAlias}); err != nil {
			return nil, NewError(http.StatusInternalServerError, "internal_error", "failed to build pagination cursor")
		}
	}
	return &listStarterTemplatesOutput{Body: NewPage(items, nextCursor)}, nil
}

func (s *Server) handleGetStarterTemplate(ctx context.Context, in *StarterTemplateAliasParam) (*starterTemplateOutput, error) {
	if _, err := s.requireAccountUser(ctx); err != nil {
		return nil, err
	}
	m, ok := startertemplates.Get(in.Alias)
	if !ok {
		return nil, NewError(http.StatusNotFound, "starter_template_not_found", "starter template not found")
	}
	return &starterTemplateOutput{Body: StarterTemplateDetailView{
		StarterTemplateView: starterTemplateView(m),
		Body:                m.TextBody,
		HTMLBody:            m.HTMLBody,
	}}, nil
}
