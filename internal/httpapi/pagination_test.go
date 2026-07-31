package httpapi

import (
	"encoding/json"
	"strings"
	"testing"
)

// testSecret is a stand-in for the deployment HMAC secret used to sign
// pagination cursors in these unit tests.
const testSecret = "0123456789abcdef0123456789abcdef"

// testResource is a stand-in collection name for the low-level
// encode/decode unit tests below; the endpoint-level binding tests live in
// pagination_endpoints_test.go and use the real resource constants.
const testResource CursorResource = "test_resource"

func TestCursorRoundTrip(t *testing.T) {
	type cur struct {
		After string `json:"after"`
		Dir   string `json:"dir"`
	}
	in := cur{After: "msg_123", Dir: "inbound"}
	enc, err := EncodeCursor(testSecret, testResource, in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if enc == "" {
		t.Fatal("expected non-empty cursor")
	}
	// The signed cursor is payload + "." + signature.
	if strings.Count(enc, ".") != 1 {
		t.Fatalf("expected exactly one '.' separator, got %q", enc)
	}
	var out cur
	if err := DecodeCursor([]string{testSecret}, testResource, enc, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out != in {
		t.Fatalf("round-trip mismatch: got %+v want %+v", out, in)
	}
}

func TestDecodeCursorEmptyIsNoop(t *testing.T) {
	out := struct{ A string }{A: "untouched"}
	if err := DecodeCursor([]string{testSecret}, testResource, "", &out); err != nil {
		t.Fatalf("empty cursor should be a no-op, got %v", err)
	}
	if out.A != "untouched" {
		t.Fatalf("empty cursor must leave dst untouched, got %+v", out)
	}
}

func TestDecodeCursorMalformed(t *testing.T) {
	var out struct{ A string }
	// Not valid base64url and no signature segment.
	if err := DecodeCursor([]string{testSecret}, testResource, "!!!not-base64!!!", &out); err != ErrInvalidCursor {
		t.Fatalf("want ErrInvalidCursor, got %v", err)
	}
	// Valid base64url payload + valid signature, but the payload is not the
	// dst struct shape (a bare string).
	bad := EncodeMust(t, testSecret, testResource, "a string, not the struct")
	if err := DecodeCursor([]string{testSecret}, testResource, bad, &out); err != ErrInvalidCursor {
		t.Fatalf("want ErrInvalidCursor on bad json, got %v", err)
	}
}

func TestDecodeCursorTamperedPayload(t *testing.T) {
	type cur struct {
		Agent string `json:"agent_email"`
	}
	enc := EncodeMust(t, testSecret, testResource, cur{Agent: "agent_a"})
	// Flip a byte in the base64 payload segment (before the '.'); the
	// signature no longer matches.
	dot := strings.IndexByte(enc, '.')
	if dot <= 0 {
		t.Fatalf("expected a signature separator in %q", enc)
	}
	b := []byte(enc)
	// Swap the first payload byte for a different base64url char.
	if b[0] == 'A' {
		b[0] = 'B'
	} else {
		b[0] = 'A'
	}
	var out cur
	if err := DecodeCursor([]string{testSecret}, testResource, string(b), &out); err != ErrInvalidCursor {
		t.Fatalf("tampered payload: want ErrInvalidCursor, got %v", err)
	}
}

func TestDecodeCursorWrongSecret(t *testing.T) {
	type cur struct {
		A string `json:"a"`
	}
	enc := EncodeMust(t, testSecret, testResource, cur{A: "x"})
	var out cur
	if err := DecodeCursor([]string{"a-different-secret-value-here-3232"}, testResource, enc, &out); err != ErrInvalidCursor {
		t.Fatalf("wrong secret: want ErrInvalidCursor, got %v", err)
	}
}

func TestDecodeCursorOldUnsignedFormatRejected(t *testing.T) {
	// An old-style cursor is plain base64url(json) with no "." + signature.
	type cur struct {
		A string `json:"a"`
	}
	signed := EncodeMust(t, testSecret, testResource, cur{A: "x"})
	unsigned, _, ok := strings.Cut(signed, ".")
	if !ok {
		t.Fatalf("expected a signature separator in %q", signed)
	}
	var out cur
	if err := DecodeCursor([]string{testSecret}, testResource, unsigned, &out); err != ErrInvalidCursor {
		t.Fatalf("old unsigned cursor: want ErrInvalidCursor, got %v", err)
	}
}

func TestDecodeCursorMultiSecretRotation(t *testing.T) {
	type cur struct {
		A string `json:"a"`
	}
	const secretA = "secret-A-padding-padding-padding-32"
	const secretB = "secret-B-padding-padding-padding-32"
	// Sign with B; verify against [A, B] — a rotation where B is the newest
	// secret and A is still accepted.
	enc := EncodeMust(t, secretB, testResource, cur{A: "rotated"})
	var out cur
	if err := DecodeCursor([]string{secretA, secretB}, testResource, enc, &out); err != nil {
		t.Fatalf("multi-secret verify: %v", err)
	}
	if out.A != "rotated" {
		t.Fatalf("round-trip mismatch: got %+v", out)
	}
}

func TestNewPageNullVsEmpty(t *testing.T) {
	// No next cursor -> next_cursor is JSON null; nil items -> [].
	p := NewPage[int](nil, "")
	raw, _ := json.Marshal(p)
	if string(raw) != `{"items":[],"next_cursor":null}` {
		t.Fatalf("unexpected empty page json: %s", raw)
	}

	p2 := NewPage([]int{1, 2}, "next")
	raw2, _ := json.Marshal(p2)
	if string(raw2) != `{"items":[1,2],"next_cursor":"next"}` {
		t.Fatalf("unexpected page json: %s", raw2)
	}
}

// EncodeMust is a test helper that encodes a cursor or fails the test.
func EncodeMust(t *testing.T, secret string, resource CursorResource, payload any) string {
	t.Helper()
	s, err := EncodeCursor(secret, resource, payload)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return s
}
