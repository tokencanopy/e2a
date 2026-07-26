package identity

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/tokencanopy/e2a/internal/filterquery"
)

// Everything messages-specific lives in this file — internal/filterquery
// stays schema-agnostic.
var (
	qRegistryOnce sync.Once
	qRegistry     *filterquery.Registry
)

// MessagesQRegistry returns the shared field registry for list_messages q.
func MessagesQRegistry() *filterquery.Registry {
	qRegistryOnce.Do(func() {
		reg, err := filterquery.NewRegistry(
			labelQField(),
			fromQField(),
			subjectQField(),
			createdQField(),
		)
		if err != nil {
			panic("filterquery: static messages registry is invalid: " + err.Error())
		}
		qRegistry = reg
	})
	return qRegistry
}

var qLabelRe = regexp.MustCompile(`^[a-z0-9:_-]{1,64}$`)

func labelQField() filterquery.FieldSpec {
	return filterquery.FieldSpec{
		Name: "label",
		Ops:  []string{":"},
		Coerce: func(raw string, quoted bool) (any, error) {
			if !qLabelRe.MatchString(raw) {
				return nil, fmt.Errorf("labels must match [a-z0-9:_-]+ (max 64 chars), got %q", raw)
			}
			return raw, nil
		},
		Emit: func(c *filterquery.Comparison, e *filterquery.EmitCtx) (string, error) {
			return "m.labels @> " + e.PH([]string{c.Value.(string)}), nil
		},
	}
}

// likeSubstring builds the ILIKE pattern shared by from:/subject:. '*' maps
// to '%'; literal %, _, and \\ are escaped with the flat-filter helper first.
func likeSubstring(v string) string {
	return "%" + strings.ReplaceAll(escapeLikePattern(v), "*", "%") + "%"
}

func textQField(name, column string, maxLen int) filterquery.FieldSpec {
	return filterquery.FieldSpec{
		Name: name,
		Ops:  []string{":", "=", "!="},
		Coerce: func(raw string, quoted bool) (any, error) {
			if raw == "" {
				return nil, fmt.Errorf("empty %s value", name)
			}
			if len(raw) > maxLen {
				return nil, fmt.Errorf("%s filter too long (max %d bytes)", name, maxLen)
			}
			return raw, nil
		},
		Emit: func(c *filterquery.Comparison, e *filterquery.EmitCtx) (string, error) {
			v := c.Value.(string)
			switch c.Op {
			case ":":
				return column + ` ILIKE ` + e.PH(likeSubstring(v)) + ` ESCAPE '\'`, nil
			case "=":
				return "LOWER(" + column + ") = LOWER(" + e.PH(v) + ")", nil
			default: // "!="
				return "LOWER(" + column + ") != LOWER(" + e.PH(v) + ")", nil
			}
		},
	}
}

func fromQField() filterquery.FieldSpec    { return textQField("from", "m.sender", 200) }
func subjectQField() filterquery.FieldSpec { return textQField("subject", "m.subject", 200) }

// createdValue carries date-coercion semantics: a date-only input (dayRange)
// makes comparisons cover the entire UTC calendar day.
type createdValue struct {
	at       time.Time
	dayRange bool
}

func createdQField() filterquery.FieldSpec {
	return filterquery.FieldSpec{
		Name: "created",
		Ops:  []string{"=", "!=", "<", "<=", ">", ">="},
		Coerce: func(raw string, quoted bool) (any, error) {
			if ts, err := time.Parse(time.RFC3339, raw); err == nil {
				return createdValue{at: ts}, nil
			}
			if d, err := time.Parse("2006-01-02", raw); err == nil {
				return createdValue{at: d, dayRange: true}, nil
			}
			return nil, fmt.Errorf("expected RFC3339 or YYYY-MM-DD, got %q", raw)
		},
		Emit: func(c *filterquery.Comparison, e *filterquery.EmitCtx) (string, error) {
			v := c.Value.(createdValue)
			if !v.dayRange {
				return "m.created_at " + c.Op + " " + e.PH(v.at), nil
			}

			end := v.at.AddDate(0, 0, 1)
			switch c.Op {
			case "=":
				return "(m.created_at >= " + e.PH(v.at) + " AND m.created_at < " + e.PH(end) + ")", nil
			case "!=":
				return "(m.created_at < " + e.PH(v.at) + " OR m.created_at >= " + e.PH(end) + ")", nil
			case "<":
				return "m.created_at < " + e.PH(v.at), nil
			case "<=":
				return "m.created_at < " + e.PH(end), nil
			case ">":
				return "m.created_at >= " + e.PH(end), nil
			default: // ">="
				return "m.created_at >= " + e.PH(v.at), nil
			}
		},
	}
}
