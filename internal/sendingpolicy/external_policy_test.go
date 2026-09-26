package sendingpolicy

import (
	"strings"
	"testing"
)

// The external-sending-access object is optional precisely so a legacy
// payload keeps its reviewed hash. These tests pin that upgrade contract and
// the strictness of the new object.

func TestExternalAccessAbsentKeepsGenerationZeroHash(t *testing.T) {
	p := DisabledPolicy()
	if p.ExternalSendingAccess != nil {
		t.Fatal("generation zero must not carry external_sending_access")
	}
	hash, err := Hash(p)
	if err != nil {
		t.Fatal(err)
	}
	if hash != generationZeroSHA256 {
		t.Fatalf("absent object changed the legacy hash: %s", hash)
	}
	if p.ExternalSendingMode() != ModeDisabled {
		t.Fatalf("absent object must read as disabled, got %q", p.ExternalSendingMode())
	}
	parsed, err := ParsePolicy([]byte(generationZeroCanonical))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.ExternalSendingAccess != nil {
		t.Fatal("parsing a legacy payload must leave the object absent")
	}
}

func TestExternalAccessPresentChangesHashAndRoundTrips(t *testing.T) {
	p := DisabledPolicy()
	p.ExternalSendingAccess = &ExternalSendingAccessPolicy{Mode: ModeEnforce, AccountsCreatedAtOrAfter: "2026-10-01T00:00:00Z"}
	hash, err := Hash(p)
	if err != nil {
		t.Fatal(err)
	}
	if hash == generationZeroSHA256 {
		t.Fatal("a present object must produce a different reviewed hash")
	}
	canonical, err := CanonicalBytes(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(canonical), `"external_sending_access":{"accounts_created_at_or_after":"2026-10-01T00:00:00Z","mode":"enforce","paid_plan_codes":[]}`) {
		t.Fatalf("canonical form missing object: %s", canonical)
	}
	parsed, err := ParsePolicy(canonical)
	if err != nil {
		t.Fatal(err)
	}
	again, err := Hash(parsed)
	if err != nil || again != hash {
		t.Fatalf("round trip hash %s != %s (err %v)", again, hash, err)
	}
}

func TestExternalAccessValidationIsStrict(t *testing.T) {
	cases := map[string]ExternalSendingAccessPolicy{
		"unknown mode":        {Mode: "on", AccountsCreatedAtOrAfter: "2026-10-01T00:00:00Z"},
		"empty mode":          {Mode: "", AccountsCreatedAtOrAfter: "2026-10-01T00:00:00Z"},
		"missing cutoff":      {Mode: ModeShadow},
		"not rfc3339":         {Mode: ModeShadow, AccountsCreatedAtOrAfter: "2026-10-01"},
		"non-utc offset":      {Mode: ModeShadow, AccountsCreatedAtOrAfter: "2026-10-01T02:00:00+02:00"},
		"fractional seconds":  {Mode: ModeShadow, AccountsCreatedAtOrAfter: "2026-10-01T00:00:00.5Z"},
		"lowercase separator": {Mode: ModeShadow, AccountsCreatedAtOrAfter: "2026-10-01t00:00:00z"},
	}
	for name, esa := range cases {
		esa := esa
		t.Run(name, func(t *testing.T) {
			p := DisabledPolicy()
			p.ExternalSendingAccess = &esa
			if err := p.Validate(); err == nil {
				t.Fatalf("expected %+v to be rejected", esa)
			}
		})
	}
}

func TestExternalAccessParseRejectsUnknownNestedField(t *testing.T) {
	raw := strings.TrimSuffix(generationZeroCanonical, "}") +
		`,"external_sending_access":{"mode":"shadow","accounts_created_at_or_after":"2026-10-01T00:00:00Z","allow_all":true}}`
	if _, err := ParsePolicy([]byte(raw)); err == nil {
		t.Fatal("an unknown nested key must fail closed")
	}
}

func TestExternalAccessNormalizedCopiesObject(t *testing.T) {
	p := DisabledPolicy()
	p.ExternalSendingAccess = &ExternalSendingAccessPolicy{Mode: ModeShadow, AccountsCreatedAtOrAfter: "2026-10-01T00:00:00Z"}
	n := p.normalized()
	n.ExternalSendingAccess.Mode = ModeEnforce
	if p.ExternalSendingAccess.Mode != ModeShadow {
		t.Fatal("normalized must not alias the caller's object")
	}
}

func TestExternalAccessPaidPlanCodes(t *testing.T) {
	base := func(codes []string) RuntimePolicy {
		p := DisabledPolicy()
		p.ExternalSendingAccess = &ExternalSendingAccessPolicy{Mode: ModeEnforce, AccountsCreatedAtOrAfter: "2026-10-01T00:00:00Z", PaidPlanCodes: codes}
		return p
	}
	for name, codes := range map[string][]string{"blank": {"a", " "}, "duplicate": {"a", "a"}} {
		if err := base(codes).Validate(); err == nil {
			t.Errorf("%s paid_plan_codes must be rejected", name)
		}
	}
	nilHash, err := Hash(base(nil))
	if err != nil {
		t.Fatal(err)
	}
	emptyHash, _ := Hash(base([]string{}))
	if nilHash != emptyHash {
		t.Fatal("nil and empty paid_plan_codes must hash alike")
	}
	listed := base([]string{"tier_b", "tier_a"}).ExternalSendingAccess
	for code, want := range map[string]bool{"tier_a": true, "tier_b": true, "tier_c": false, "": false, "TIER_A": false} {
		if got := listed.paidPlan(code); got != want {
			t.Errorf("paidPlan(%q) = %v, want %v", code, got, want)
		}
	}
	if base(nil).ExternalSendingAccess.paidPlan("tier_a") {
		t.Error("an empty list entitles nobody")
	}
}
