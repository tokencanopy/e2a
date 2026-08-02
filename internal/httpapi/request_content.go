package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/danielgtaylor/huma/v2"
)

// Request-content guards that apply to EVERY v1 operation.
//
// Two rules are enforced here, deliberately global rather than per-field:
// no client-supplied string anywhere in a request may contain U+0000 (NUL),
// and every client-supplied string must be well-formed UTF-8.
//
// Why blanket rules. Postgres text columns can store neither a NUL byte nor
// an invalid UTF-8 byte sequence — the driver hands the value to the server
// and the server rejects it (SQLSTATE 22021) — so any caller string that
// reaches a text column turns a client mistake into an HTTP 500. Guarding the
// five columns that happened to be reachable today leaves the sixth one to be
// found by the next caller, and a "reject only what is persisted" rule is
// unknowable from the outside: a caller cannot tell which of
// subject/text/html/filename lands in a column and which is only composed.
// One rule, stated once, is the only version of this a client can reason
// about — and it costs nothing, because neither U+0000 nor a broken byte
// sequence is meaningful in an email address, a display name, a subject line,
// or a MIME header.
//
// The two rules need two different enforcement points:
//
//   - Path, query, header and cookie params reach the bound input struct with
//     their raw bytes intact, so the post-bind walk below (scanInput) catches
//     both NUL and invalid UTF-8 there.
//   - JSON body fields DO NOT: encoding/json silently replaces invalid UTF-8
//     bytes with U+FFFD while decoding into a string, so by the time the walk
//     sees a body field the corruption is already laundered into valid UTF-8
//     and silently persisted as mojibake. Raw body bytes are therefore
//     checked BEFORE decoding, in requireUTF8Body (wired into the huma
//     Formats table by New). RFC 8259 §8.1 requires JSON to be UTF-8, so this
//     rejects nothing legal. A NUL survives decoding only as the \u0000
//     escape, which the walk still sees - so the walk keeps the NUL rule for
//     bodies, and the format guard owns the UTF-8 rule for bodies.
//
// The walk is enforced at registration time (see registerOp) rather than in
// each handler so a new operation inherits it by construction; there is no
// per-endpoint guard to forget.
const (
	nulValueMessage  = "must not contain a NUL (U+0000) character"
	nulKeyMessage    = "object keys must not contain a NUL (U+0000) character"
	utf8ValueMessage = "must be valid UTF-8"
	utf8KeyMessage   = "object keys must be valid UTF-8"
)

// errBodyNotUTF8 is returned by requireUTF8Body before JSON decoding. Huma
// folds it into a 400 validation error located at "body"; the envelope
// constructor (humaErrorConstructor) deliberately drops the raw offending
// bytes, so the invalid sequence is never echoed back.
var errBodyNotUTF8 = errors.New("request body must be valid UTF-8")

// requireUTF8Body wraps a body format so the RAW request bytes must be valid
// UTF-8 before they are decoded. This is the only place the body's invalid
// bytes still exist — see the package rule comment above.
func requireUTF8Body(format huma.Format) huma.Format {
	unmarshal := format.Unmarshal
	format.Unmarshal = func(data []byte, v any) error {
		if !utf8.Valid(data) {
			return errBodyNotUTF8
		}
		return unmarshal(data, v)
	}
	return format
}

// contentViolation is the located reason a request was refused.
type contentViolation struct {
	Location string
	Message  string
}

// registerOp registers a v1 operation with Huma, interposing the shared
// request-content guards between Huma's schema validation and the handler.
// Every operation in this package goes through it; calling huma.Register
// directly would silently opt an endpoint out of the guards.
func registerOp[I, O any](api huma.API, op huma.Operation, handler func(context.Context, *I) (*O, error)) {
	huma.Register(api, op, func(ctx context.Context, in *I) (*O, error) {
		if bad := scanInput(reflect.ValueOf(in)); bad != nil {
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

// contentScanner carries the walk's current field path. The overwhelmingly common
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
type contentScanner struct {
	path []locSegment
}

// locSegment is one step of a field path: a named field/map key, or an index.
type locSegment struct {
	name    string
	index   int
	indexed bool
	// untrusted marks a caller-controlled map key. Field names come from our
	// own structs, but a map key is arbitrary caller input of arbitrary size —
	// the location is echoed into both error.message and details.fields, so an
	// unbounded key would reflect megabytes of caller bytes back in the
	// response (Huma binds the body before auth runs, so unauthenticated).
	// Truncated at render time, on the failure path only.
	untrusted bool
}

// location materializes the current path, e.g. body.contacts[417].display_name.
// Called at most once per request, on the failure path.
func (s *contentScanner) location() string {
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
		name := seg.name
		if seg.untrusted {
			name = truncateForError(name)
		}
		b.WriteString(name)
	}
	return b.String()
}

func (s *contentScanner) violation(message string) *contentViolation {
	return &contentViolation{Location: s.location(), Message: message}
}

// scanInput walks a bound Huma input struct — body, path, query, header
// and cookie fields alike — and reports the first string carrying a NUL or an
// invalid UTF-8 byte sequence. (For JSON body fields the UTF-8 arm is
// vacuously true — the decoder has already replaced invalid bytes, which is
// why requireUTF8Body checks the raw bytes — but non-body params arrive
// unlaundered and are caught here.) Locations use Huma's own convention
// (body.contacts[3].display_name, query.source, header.Idempotency-Key) so a
// client can point at the offending field without guessing.
func scanInput(v reflect.Value) *contentViolation {
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
	var scanner contentScanner
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
			if bad := scanInput(v.Field(i)); bad != nil {
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
func (s *contentScanner) scanValue(v reflect.Value) *contentViolation {
	switch v.Kind() {
	case reflect.String:
		str := v.String()
		if strings.IndexByte(str, 0) >= 0 {
			return s.violation(nulValueMessage)
		}
		if !utf8.ValidString(str) {
			return s.violation(utf8ValueMessage)
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
				if !utf8.ValidString(key.String()) {
					return s.violation(utf8KeyMessage)
				}
				s.path = append(s.path, locSegment{name: key.String(), untrusted: true})
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
