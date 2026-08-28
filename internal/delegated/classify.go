package delegated

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

// protectedHeader is the strict view of a compact token's protected JOSE
// header. Only these three members matter to this package; extra members
// are tolerated (the header is size-capped, and RFC 7515 headers may
// carry x5c etc.) but typ/alg/kid are bounded individually.
type protectedHeader struct {
	Typ string `json:"typ"`
	Alg string `json:"alg"`
	Kid string `json:"kid"`
}

// splitCompact splits a compact serialization into exactly three
// non-empty segments, enforcing the raw-size cap. Returns ok=false when
// the string cannot be a delegated-parseable compact JWT.
func splitCompact(token string) (header, payload, signature string, ok bool) {
	if token == "" || len(token) > MaxCompactJWTBytes {
		return "", "", "", false
	}
	first := strings.IndexByte(token, '.')
	if first <= 0 {
		return "", "", "", false
	}
	second := strings.IndexByte(token[first+1:], '.')
	if second < 0 {
		return "", "", "", false
	}
	second += first + 1
	header, payload, signature = token[:first], token[first+1:second], token[second+1:]
	if payload == "" || signature == "" || strings.IndexByte(signature, '.') >= 0 {
		return "", "", "", false
	}
	return header, payload, signature, true
}

// decodeSegment base64url-decodes one unpadded segment, rejecting decoded
// output over maxDecoded bytes. The +1 read window is what turns the cap
// into an exact reject-at-limit+1 boundary rather than a truncation.
func decodeSegment(seg string, maxDecoded int) ([]byte, bool) {
	// Unpadded base64url: 4 chars decode to 3 bytes. Reject encodings that
	// cannot decode to <= maxDecoded before allocating.
	if base64.RawURLEncoding.DecodedLen(len(seg)) > maxDecoded {
		return nil, false
	}
	out, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil || len(out) > maxDecoded {
		return nil, false
	}
	return out, true
}

// parseProtectedHeader strictly parses the protected header of a compact
// token under the exact size limits, without touching the payload or
// signature bytes beyond splitting.
func parseProtectedHeader(token string) (protectedHeader, bool) {
	seg, _, _, ok := splitCompact(token)
	if !ok {
		return protectedHeader{}, false
	}
	raw, ok := decodeSegment(seg, MaxProtectedHeaderBytes)
	if !ok {
		return protectedHeader{}, false
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var hdr protectedHeader
	if err := dec.Decode(&hdr); err != nil {
		return protectedHeader{}, false
	}
	// One JSON value and nothing after it.
	if dec.More() {
		return protectedHeader{}, false
	}
	if len(hdr.Typ) > MaxTypBytes || len(hdr.Alg) > MaxAlgBytes || len(hdr.Kid) > MaxKidBytes {
		return protectedHeader{}, false
	}
	return hdr, true
}

// peekProtectedTyp reads ONLY the protected header's `typ`, tolerant of
// every other member. It is the classification-time parse: it enforces
// the structural preconditions (three non-empty segments, a decodable
// JSON-object header under the decoded-size cap) but deliberately does
// NOT apply the verification-time pins on `alg`/`kid` or their byte caps.
// Those pins belong to parseProtectedHeader (verification only): applying
// them here would let an at+jwt with an oversized or oddly-typed OTHER
// header member (a 129-char kid, a non-string alg) escape delegated
// ownership and fall through to another credential path, violating the
// §10.4 dispatch invariant. A non-string or absent `typ` yields "".
func peekProtectedTyp(token string) (string, bool) {
	seg, _, _, ok := splitCompact(token)
	if !ok {
		return "", false
	}
	raw, ok := decodeSegment(seg, MaxProtectedHeaderBytes)
	if !ok {
		return "", false
	}
	// Decode `typ` only; unknown members are ignored regardless of their
	// type or size (json ignores fields absent from the target struct).
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	var hdr struct {
		Typ string `json:"typ"`
	}
	if err := dec.Decode(&hdr); err != nil {
		return "", false
	}
	if dec.More() {
		return "", false
	}
	return hdr.Typ, true
}

// Classify reports whether this bearer credential is delegated-owned: a
// compact JWT, within the raw header/compact/segment limits, whose
// protected header carries exactly typ "at+jwt".
//
// The decision is parse-only — no signature, network, or configuration is
// consulted, so ownership is identical whether the verifier is enabled,
// disabled, or unavailable. A positively classified token must never
// reach any other credential path. Classification reads only `typ` and is
// tolerant of every other header member; the verification-time pins
// (alg/kid caps, strict typing) run later in Verify and reject a bad
// token as a delegated 401, never by handing it back to another path.
//
// rawAuthorizationLen is the length of the full Authorization header
// value, including the "Bearer " prefix, so the raw-header cap covers
// what actually arrived on the wire.
func Classify(rawAuthorizationLen int, bearer string) bool {
	if rawAuthorizationLen > MaxAuthorizationBytes {
		return false
	}
	typ, ok := peekProtectedTyp(bearer)
	if !ok {
		return false
	}
	return typ == TokenType
}
