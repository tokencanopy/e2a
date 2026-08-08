package httpapi

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tokencanopy/e2a/internal/outbound"
)

// TestRecipientOctetLimitsAreDocumented pins that the SMTP mailbox octet limits
// — enforced synchronously since the recipient-validation hardening, but
// absent from the spec — are stated on every field they gate. A client
// generating long plus-addressed local parts otherwise gets an unexplained 400.
func TestRecipientOctetLimitsAreDocumented(t *testing.T) {
	raw, err := json.Marshal(New(Deps{}).API.OpenAPI())
	if err != nil {
		t.Fatalf("render OpenAPI: %v", err)
	}
	var doc struct {
		Components struct {
			Schemas map[string]map[string]any `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse OpenAPI: %v", err)
	}

	// Recipient lists reject with invalid_recipient; reply_to rejects with
	// invalid_request. The distinction is real (different call sites) and a
	// client branching on error.code needs it stated correctly.
	for schema, fields := range map[string][]string{
		"SendEmailRequest": {"to", "cc", "bcc"},
		"ReplyRequest":     {"cc", "bcc"},
		"ForwardRequest":   {"to", "cc", "bcc"},
	} {
		assertOctetLimitDoc(t, doc.Components.Schemas, schema, fields, "invalid_recipient")
	}
	for _, schema := range []string{"SendEmailRequest", "ReplyRequest", "ForwardRequest"} {
		assertOctetLimitDoc(t, doc.Components.Schemas, schema, []string{"reply_to"}, "invalid_request")
	}
}

func assertOctetLimitDoc(t *testing.T, schemas map[string]map[string]any, schema string, fields []string, code string) {
	t.Helper()
	properties, ok := schemas[schema]["properties"].(map[string]any)
	if !ok {
		t.Errorf("%s has no properties", schema)
		return
	}
	for _, field := range fields {
		property, ok := properties[field].(map[string]any)
		if !ok {
			t.Errorf("%s.%s is missing", schema, field)
			continue
		}
		description, _ := property["description"].(string)
		for _, fragment := range []string{"64 octets", "254 octets", code} {
			if !strings.Contains(description, fragment) {
				t.Errorf("%s.%s description does not mention %q", schema, field, fragment)
			}
		}
	}
}

// TestDocumentedOctetLimitsMatchTheEnforcer keeps the two numbers in the
// descriptions tied to the function that actually rejects: 64 octets of local
// part and 254 octets of addr-spec, counted in BYTES, so a multi-byte local
// part trips sooner than its character count suggests.
func TestDocumentedOctetLimitsMatchTheEnforcer(t *testing.T) {
	domain := "@fund.vc"
	if err := outbound.ValidateMailboxAddress(strings.Repeat("a", 64) + domain); err != nil {
		t.Errorf("a 64-octet local part must be accepted, got %v", err)
	}
	if err := outbound.ValidateMailboxAddress(strings.Repeat("a", 65) + domain); err == nil {
		t.Error("a 65-octet local part must be rejected — the documented limit is 64")
	}
	// 16 four-byte runes = 64 octets but only 16 code points: the documented
	// "counted in UTF-8 bytes" caveat is the whole point of the sentence.
	if err := outbound.ValidateMailboxAddress(strings.Repeat("😀", 17) + domain); err == nil {
		t.Error("a 68-octet (17-rune) local part must be rejected — the limit counts octets, not characters")
	}

	// 254 octets total: build a long domain so the addr-spec limit, not the
	// local-part limit, is the one that trips.
	long := "a@" + strings.Repeat("b", 250) + ".vc" // 2 + 253 = 255 octets
	if err := outbound.ValidateMailboxAddress(long); err == nil {
		t.Errorf("a %d-octet addr-spec must be rejected — the documented limit is 254", len(long))
	}
	fits := "a@" + strings.Repeat("b", 249) + ".vc" // 254 octets
	if len(fits) != 254 {
		t.Fatalf("fixture is %d octets, want 254", len(fits))
	}
	if err := outbound.ValidateMailboxAddress(fits); err != nil {
		t.Errorf("a 254-octet addr-spec must be accepted, got %v", err)
	}
}
