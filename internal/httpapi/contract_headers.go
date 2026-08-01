package httpapi

import "github.com/danielgtaylor/huma/v2"

const (
	headerXRequestID       = "XRequestID"
	headerRetryAfter       = "RetryAfter"
	headerRateLimitLimit   = "RateLimitLimit"
	headerRateLimitRemain  = "RateLimitRemaining"
	headerRateLimitReset   = "RateLimitReset"
	headerETag             = "ETag"
	headerResourceLocation = "ResourceLocation"
)

func headerRef(name string) *huma.Param {
	return &huma.Param{Ref: "#/components/headers/" + name}
}

// applyResponseHeaderContract documents headers emitted centrally by the
// request-id and rate-limit middleware. It enriches existing response objects;
// operation registration remains responsible for declaring possible statuses.
func (s *Server) applyResponseHeaderContract() {
	oapi := s.API.OpenAPI()
	minimumOne := float64(1)
	if oapi.Components.Headers == nil {
		oapi.Components.Headers = map[string]*huma.Header{}
	}
	oapi.Components.Headers[headerXRequestID] = &huma.Header{
		Description: "Always-present request correlation id. Error responses echo the same value in error.request_id.",
		Schema:      &huma.Schema{Type: huma.TypeString},
	}
	oapi.Components.Headers[headerRetryAfter] = &huma.Header{
		Description: "Positive integer seconds to wait before retrying a transient 429 or 503 response.",
		Schema:      &huma.Schema{Type: huma.TypeInteger, Minimum: &minimumOne},
	}
	oapi.Components.Headers[headerRateLimitLimit] = &huma.Header{
		Description: "Request quota for the current limiter window.",
		Schema:      &huma.Schema{Type: huma.TypeInteger},
	}
	oapi.Components.Headers[headerRateLimitRemain] = &huma.Header{
		Description: "Requests remaining in the current limiter window.",
		Schema:      &huma.Schema{Type: huma.TypeInteger},
	}
	oapi.Components.Headers[headerRateLimitReset] = &huma.Header{
		Description: "Seconds until the current limiter window resets.",
		Schema:      &huma.Schema{Type: huma.TypeInteger},
	}
	// ETag and Location are emitted per-operation from the handler output
	// structs, which carry no description — Huma renders them as bare
	// `type: string`. Both encode a convention a client MUST know to use them
	// correctly and cannot infer from the type, so document them centrally:
	// one wording, applied wherever the header actually appears.
	oapi.Components.Headers[headerETag] = &huma.Header{
		Description: "Opaque STRONG entity-tag validator for the returned representation, currently a quoted 32-hex-character token (e.g. \"9f86d081884c7d65…\"). Treat it as opaque: never parse, derive, or construct one. Store the received value and replay it VERBATIM in a later If-Match to make a write conditional. Any accepted write moves the validator, so a stale value cannot match and the conditional write is rejected with 412 precondition_failed.",
		Schema:      &huma.Schema{Type: huma.TypeString},
	}
	oapi.Components.Headers[headerResourceLocation] = &huma.Header{
		Description: "Canonical path-relative URL of the affected resource. Path segments are percent-encoded PER SEGMENT, which legally leaves the sub-delimiters @ + & = : $ unescaped — /v1/contacts/a.partner@fund.vc is an expected value, and so is the fully-escaped /v1/contacts/a.partner%40fund.vc. Neither form is promised, so never string-compare this header against a URL built locally: percent-decode the final path segment to recover the canonical resource key, then compare that.",
		Schema:      &huma.Schema{Type: huma.TypeString, Format: "uri-reference"},
	}
	forEachOperation(oapi, func(op *huma.Operation) {
		for status, response := range op.Responses {
			if response.Headers == nil {
				response.Headers = map[string]*huma.Param{}
			}
			response.Headers["X-Request-Id"] = headerRef(headerXRequestID)
			// Replace the undescribed auto-derived declarations wherever the
			// operation already promises these headers. Enriching what is
			// there (rather than adding it) keeps this pass from inventing a
			// header an operation does not actually emit.
			if _, ok := response.Headers["ETag"]; ok {
				response.Headers["ETag"] = headerRef(headerETag)
			}
			if _, ok := response.Headers["Location"]; ok {
				response.Headers["Location"] = headerRef(headerResourceLocation)
			}
			if status == "429" || status == "503" {
				response.Headers["Retry-After"] = headerRef(headerRetryAfter)
			}
			if pollLimitedOps[op.OperationID] || op.OperationID == "createAgent" {
				response.Headers["RateLimit-Limit"] = headerRef(headerRateLimitLimit)
				response.Headers["RateLimit-Remaining"] = headerRef(headerRateLimitRemain)
				response.Headers["RateLimit-Reset"] = headerRef(headerRateLimitReset)
			}
		}
	})
}
