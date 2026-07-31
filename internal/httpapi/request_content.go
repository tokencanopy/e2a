package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
)

// Request-content guards that apply to EVERY v1 operation.
//
// The rule enforced here is deliberately global rather than per-field: no
// client-supplied string anywhere in a request may contain U+0000 (NUL).
//
// Why a blanket rule. Postgres text columns cannot store a NUL byte at all —
// the driver hands the value to the server and the server rejects it — so any
// caller string that reaches a text column turns a client mistake into an
// HTTP 500. Guarding the five columns that happened to be reachable today
// leaves the sixth one to be found by the next caller, and a "reject only what
// is persisted" rule is unknowable from the outside: a caller cannot tell
// which of subject/text/html/filename lands in a column and which is only
// composed. One rule, stated once, is the only version of this a client can
// reason about — and it costs nothing, because U+0000 is not meaningful in an
// email address, a display name, a subject line, or a MIME header.
//
// It is enforced at registration time (see registerOp) rather than in each
// handler so a new operation inherits it by construction; there is no
// per-endpoint guard to forget.
const (
	nulValueMessage = "must not contain a NUL (U+0000) character"
	nulKeyMessage   = "object keys must not contain a NUL (U+0000) character"
)

// nulViolation is the located reason a request was refused.
type nulViolation struct {
	Location string
	Message  string
}

// registerOp registers a v1 operation with Huma, interposing the shared
// request-content guards between Huma's schema validation and the handler.
// Every operation in this package goes through it; calling huma.Register
// directly would silently opt an endpoint out of the guards.
func registerOp[I, O any](api huma.API, op huma.Operation, handler func(context.Context, *I) (*O, error)) {
	huma.Register(api, op, func(ctx context.Context, in *I) (*O, error) {
		if bad := scanInputForNUL(reflect.ValueOf(in)); bad != nil {
			return nil, NewError(http.StatusBadRequest, "invalid_request",
				fmt.Sprintf("%s %s", bad.Location, bad.Message)).
				WithDetails(ValidationErrorDetails{Fields: []FieldError{{
					Location: bad.Location,
					Message:  bad.Message,
				}}})
		}
		return handler(ctx, in)
	})
}

// nulScanner carries the walk's current field path. The overwhelmingly common
// case is a clean request, where every location string the walk could produce
// is thrown away — and building them eagerly is not free: a 1000-row contact
// import formatted ~14.7k throwaway strings (~350 KB) to describe fields that
// turned out to be fine.
//
// So the path is kept as a stack of segments, pushed and popped as the walk
// descends and unwinds, reusing one backing array for the whole request, and
// rendered into a string only at the point a violation is actually found. A
// linked list of nodes does NOT work here: the recursion makes Go's escape
// analysis heap-allocate every node, which merely trades string allocations
// for struct ones (measured: 350 KB → 352 KB, no win).
//
// Measured on the same 1000-row import (BenchmarkScanCleanImportBody):
// 350 KB / 14.7k allocs → 64.5 KB / 4k. The path machinery itself is now
// effectively free — the same body without metadata objects costs 438 B and 12
// allocs — and the remainder is reflect's own map-iteration cost over the 1000
// metadata maps, which is inherent to walking them at all.
type nulScanner struct {
	path []locSegment
}

// locSegment is one step of a field path: a named field/map key, or an index.
type locSegment struct {
	name    string
	index   int
	indexed bool
}

// location materializes the current path, e.g. body.contacts[417].display_name.
// Called at most once per request, on the failure path.
func (s *nulScanner) location() string {
	var b strings.Builder
	for i, seg := range s.path {
		if seg.indexed {
			b.WriteByte('[')
			b.WriteString(strconv.Itoa(seg.index))
			b.WriteByte(']')
			continue
		}
		if i > 0 {
			b.WriteByte('.')
		}
		b.WriteString(seg.name)
	}
	return b.String()
}

func (s *nulScanner) violation(message string) *nulViolation {
	return &nulViolation{Location: s.location(), Message: message}
}

// scanInputForNUL walks a bound Huma input struct — body, path, query, header
// and cookie fields alike — and reports the first string carrying a NUL.
// Locations use Huma's own convention (body.contacts[3].display_name,
// query.source, header.Idempotency-Key) so a client can point at the offending
// field without guessing.
func scanInputForNUL(v reflect.Value) *nulViolation {
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return nil
	}
	t := v.Type()
	// One scanner, and therefore one path buffer, for the whole input.
	var scanner nulScanner
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}
		var prefix string
		switch {
		case tagName(field, "path") != "":
			prefix = "path." + tagName(field, "path")
		case tagName(field, "query") != "":
			prefix = "query." + tagName(field, "query")
		case tagName(field, "header") != "":
			prefix = "header." + tagName(field, "header")
		case tagName(field, "cookie") != "":
			prefix = "cookie." + tagName(field, "cookie")
		case field.Name == "Body":
			prefix = "body"
		case field.Anonymous:
			// Embedded parameter groups (PageParams, DeleteConfirm) carry their
			// own binding tags one level down.
			if bad := scanInputForNUL(v.Field(i)); bad != nil {
				return bad
			}
			continue
		default:
			// RawBody and other untagged plumbing fields are not caller strings.
			continue
		}
		scanner.path = append(scanner.path[:0], locSegment{name: prefix})
		if bad := scanner.scanValue(v.Field(i)); bad != nil {
			return bad
		}
	}
	return nil
}

var timeType = reflect.TypeOf(time.Time{})

// scanValue walks one bound value to arbitrary depth. Byte slices are skipped:
// they are opaque payload bytes (RawBody, json.RawMessage), not caller-authored
// strings, and the JSON decoder could not have produced a string field from
// them.
func (s *nulScanner) scanValue(v reflect.Value) *nulViolation {
	switch v.Kind() {
	case reflect.String:
		if strings.IndexByte(v.String(), 0) >= 0 {
			return s.violation(nulValueMessage)
		}
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return nil
		}
		return s.scanValue(v.Elem())
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return nil
		}
		fallthrough
	case reflect.Array:
		s.path = append(s.path, locSegment{})
		last := len(s.path) - 1
		for i := 0; i < v.Len(); i++ {
			s.path[last] = locSegment{index: i, indexed: true}
			if bad := s.scanValue(v.Index(i)); bad != nil {
				return bad
			}
		}
		s.path = s.path[:last]
	case reflect.Map:
		iter := v.MapRange()
		for iter.Next() {
			key := iter.Key()
			pushed := false
			if key.Kind() == reflect.String {
				if strings.IndexByte(key.String(), 0) >= 0 {
					// The key itself is the offender; it is not echoed back into
					// the location, since that would put raw bad input in the
					// response the way FieldError deliberately avoids.
					return s.violation(nulKeyMessage)
				}
				s.path = append(s.path, locSegment{name: key.String()})
				pushed = true
			}
			if bad := s.scanValue(iter.Value()); bad != nil {
				return bad
			}
			if pushed {
				s.path = s.path[:len(s.path)-1]
			}
		}
	case reflect.Struct:
		if v.Type() == timeType {
			return nil
		}
		t := v.Type()
		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)
			if !field.IsExported() {
				continue
			}
			name := jsonFieldName(field)
			if name == "-" {
				continue
			}
			pushed := false
			if name != "" {
				s.path = append(s.path, locSegment{name: name})
				pushed = true
			}
			if bad := s.scanValue(v.Field(i)); bad != nil {
				return bad
			}
			if pushed {
				s.path = s.path[:len(s.path)-1]
			}
		}
	}
	return nil
}

// tagName returns the bare name from a Huma binding tag, dropping any options.
func tagName(field reflect.StructField, key string) string {
	value, ok := field.Tag.Lookup(key)
	if !ok {
		return ""
	}
	name, _, _ := strings.Cut(value, ",")
	return name
}

// jsonFieldName resolves the wire name of a body field. An embedded struct with
// no json tag returns "" so its fields are reported at the parent's location,
// matching how the JSON document is actually shaped.
func jsonFieldName(field reflect.StructField) string {
	tag, ok := field.Tag.Lookup("json")
	if !ok {
		if field.Anonymous {
			return ""
		}
		return field.Name
	}
	name, _, _ := strings.Cut(tag, ",")
	switch {
	case name == "-":
		return "-"
	case name != "":
		return name
	case field.Anonymous:
		return ""
	default:
		return field.Name
	}
}
